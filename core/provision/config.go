package provision

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the service configuration. Secrets never live in it: tokens come from the
// environment, named by token_env / secret_env.
type Config struct {
	Listen    string          `yaml:"listen"`
	DBPath    string          `yaml:"db_path"`
	Directory DirectoryConfig `yaml:"directory"`
	Tasks     TasksConfig     `yaml:"tasks"`
	Workflows WorkflowConfig  `yaml:"workflows"`
	Retry     RetryConfig     `yaml:"retry"`
	SCIM      SCIMConfig      `yaml:"scim"`
	Connector []ConnConfig    `yaml:"connectors"`
}

// DirectoryConfig points at the people module's directory API.
type DirectoryConfig struct {
	URL string `yaml:"url"`
	// AllowInsecureHTTP permits plain http, as the in-cluster Service of the people module is.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`
}

// TasksConfig names the people checklist templates used for the manager and IT tasks.
type TasksConfig struct {
	JoinerTemplate string `yaml:"joiner_template"`
	LeaverTemplate string `yaml:"leaver_template"`
}

// WorkflowConfig shapes the workflows.
type WorkflowConfig struct {
	// PrivilegedGroups are SCIM group names whose members are privileged: their access changes
	// need a second person (an approval).
	PrivilegedGroups []string `yaml:"privileged_groups"`
	// DryRun records every run and its plan but calls no connector.
	DryRun bool `yaml:"dry_run"`
	// DeleteAfterDays schedules delete_account that many days after a leaver's last day (0 = never
	// delete, only disable). Deletion always needs an approval.
	DeleteAfterDays int `yaml:"delete_after_days"`
	// CompensateWithDelete makes the compensation of a failed joiner delete the accounts it
	// created (default: disable them, which loses nothing).
	CompensateWithDelete bool `yaml:"compensate_with_delete"`
	// Timezone decides when a leaver's last day starts and ends (default UTC).
	Timezone string `yaml:"timezone"`
	// LeaverCutoff is when access ends on the last day: start_of_day (default, the safe choice:
	// the person has no access on their last day) or end_of_day (access lasts through the last day).
	LeaverCutoff string `yaml:"leaver_cutoff"`
}

// RetryConfig bounds retries: per-step attempts, backoff and the deadline of one connector call.
type RetryConfig struct {
	MaxAttempts int    `yaml:"max_attempts"`
	BaseDelay   string `yaml:"base_delay"`
	MaxDelay    string `yaml:"max_delay"`
	StepTimeout string `yaml:"step_timeout"`
}

// SCIMConfig shapes the SCIM server.
type SCIMConfig struct {
	Disabled bool `yaml:"disabled"`
	// RequireIfMatch makes every write demand an If-Match header (optimistic concurrency). It is ON
	// unless set to false: a PATCH made from a stale copy answers 412 instead of overwriting.
	RequireIfMatch *bool `yaml:"require_if_match"`
	// MaxScan bounds how many directory records one list request reads (default 5000).
	MaxScan int `yaml:"max_scan"`
}

// ConnConfig is one downstream system.
type ConnConfig struct {
	Name string `yaml:"name"`
	// Type is scim-client, webhook, or a stub: google-workspace, github, slack.
	Type              string   `yaml:"type"`
	URL               string   `yaml:"url"`
	TokenEnv          string   `yaml:"token_env"`  // scim-client: bearer token
	SecretEnv         string   `yaml:"secret_env"` // webhook: HMAC secret
	AllowPrivate      bool     `yaml:"allow_private"`
	AllowInsecureHTTP bool     `yaml:"allow_insecure_http"`
	Privileged        bool     `yaml:"privileged"`
	DefaultGroups     []string `yaml:"default_groups"`
	SyncGroups        *bool    `yaml:"sync_groups"`
}

var connNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// LoadConfig reads and validates a config file. Unknown keys are errors.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig parses and validates config text.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = ":8090"
	}
	if c.DBPath == "" {
		c.DBPath = "/data/provision.db"
	}
	if c.SCIM.MaxScan <= 0 {
		c.SCIM.MaxScan = 5000
	}
	if c.SCIM.MaxScan > 50000 {
		return nil, errors.New("config: scim.max_scan is at most 50000")
	}
	if c.Directory.URL == "" {
		return nil, errors.New("config: directory.url is required")
	}
	if _, err := validateTarget(c.Directory.URL, true, c.Directory.AllowInsecureHTTP); err != nil {
		return nil, fmt.Errorf("config: directory.url: %w", err)
	}
	if c.Workflows.DeleteAfterDays < 0 || c.Workflows.DeleteAfterDays > 3650 {
		return nil, errors.New("config: workflows.delete_after_days must be between 0 and 3650")
	}
	if _, err := c.Policy(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, cc := range c.Connector {
		if !connNameRe.MatchString(cc.Name) || cc.Name == connCore || cc.Name == connTasks {
			return nil, fmt.Errorf("config: connectors[%d]: name must match %s", i, connNameRe)
		}
		if seen[cc.Name] {
			return nil, fmt.Errorf("config: connector %q is listed twice", cc.Name)
		}
		seen[cc.Name] = true
		switch cc.Type {
		case "scim-client":
			if cc.TokenEnv == "" {
				return nil, fmt.Errorf("config: connector %q needs token_env", cc.Name)
			}
		case "webhook":
			if cc.SecretEnv == "" {
				return nil, fmt.Errorf("config: connector %q needs secret_env", cc.Name)
			}
		case "google-workspace", "github", "slack":
			// a stub: no URL, no credentials, no network
			if cc.URL != "" || cc.TokenEnv != "" || cc.SecretEnv != "" {
				return nil, fmt.Errorf("config: connector %q is a stub (unimplemented) and takes no url or credentials", cc.Name)
			}
			continue
		default:
			return nil, fmt.Errorf("config: connector %q has unknown type %q", cc.Name, cc.Type)
		}
		if _, err := validateTarget(cc.URL, cc.AllowPrivate, cc.AllowInsecureHTTP); err != nil {
			return nil, fmt.Errorf("config: connector %q url: %w", cc.Name, err)
		}
		for _, g := range cc.DefaultGroups {
			if !groupRe.MatchString(g) {
				return nil, fmt.Errorf("config: connector %q has an invalid default group", cc.Name)
			}
		}
	}
	for _, g := range c.Workflows.PrivilegedGroups {
		if !groupRe.MatchString(g) {
			return nil, errors.New("config: workflows.privileged_groups has an invalid group name")
		}
	}
	return &c, nil
}

// Policy is the validated, typed form of the workflow and retry settings.
type Policy struct {
	PrivilegedGroups     []string
	DryRun               bool
	DeleteAfterDays      int
	EndOfDay             bool // leaver access ends at the end of the last day, not its start
	CompensateWithDelete bool
	JoinerTemplate       string
	LeaverTemplate       string
	TZ                   *time.Location
	MaxAttempts          int
	BaseDelay, MaxDelay  time.Duration
	StepTimeout          time.Duration
	// MaxStepsPerRun refuses a plan larger than this (a runaway group list).
	MaxStepsPerRun int
	// MaxStepsPerTick bounds the work of one pass of the worker.
	MaxStepsPerTick int
	Conn            map[string]ConnConfig
}

// Policy validates the workflow and retry settings.
func (c *Config) Policy() (Policy, error) {
	p := Policy{PrivilegedGroups: c.Workflows.PrivilegedGroups, DryRun: c.Workflows.DryRun, DeleteAfterDays: c.Workflows.DeleteAfterDays,
		CompensateWithDelete: c.Workflows.CompensateWithDelete, JoinerTemplate: c.Tasks.JoinerTemplate, LeaverTemplate: c.Tasks.LeaverTemplate,
		MaxAttempts: c.Retry.MaxAttempts, MaxStepsPerRun: 200, MaxStepsPerTick: 25, Conn: map[string]ConnConfig{}}
	for _, cc := range c.Connector {
		p.Conn[cc.Name] = cc
	}
	var err error
	switch c.Workflows.LeaverCutoff {
	case "", "start_of_day":
	case "end_of_day":
		p.EndOfDay = true
	default:
		return p, errors.New("config: workflows.leaver_cutoff must be start_of_day or end_of_day")
	}
	if p.TZ, err = time.LoadLocation(firstNonEmpty(c.Workflows.Timezone, "UTC")); err != nil {
		return p, fmt.Errorf("config: workflows.timezone: %w", err)
	}
	if p.MaxAttempts == 0 {
		p.MaxAttempts = 5
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 20 {
		return p, errors.New("config: retry.max_attempts must be between 1 and 20")
	}
	for _, d := range []struct {
		dst *time.Duration
		src string
		def time.Duration
		min time.Duration
		max time.Duration
		key string
	}{
		{&p.BaseDelay, c.Retry.BaseDelay, 30 * time.Second, time.Second, time.Hour, "base_delay"},
		{&p.MaxDelay, c.Retry.MaxDelay, time.Hour, time.Second, 24 * time.Hour, "max_delay"},
		{&p.StepTimeout, c.Retry.StepTimeout, 30 * time.Second, time.Second, 5 * time.Minute, "step_timeout"},
	} {
		*d.dst = d.def
		if d.src != "" {
			v, err := time.ParseDuration(d.src)
			if err != nil || v < d.min || v > d.max {
				return p, fmt.Errorf("config: retry.%s must be a duration between %s and %s", d.key, d.min, d.max)
			}
			*d.dst = v
		}
	}
	return p, nil
}

// BuildConnectors turns the connector config into adapters. Secrets come from getenv; a missing or
// short secret is an error at start, never a connector that silently sends no credentials.
func BuildConnectors(cfg *Config, getenv func(string) string) (*ConnectorSet, error) {
	var cs []Connector
	for _, cc := range cfg.Connector {
		switch cc.Type {
		case "scim-client":
			tok := getenv(cc.TokenEnv)
			if len(tok) < 8 {
				return nil, fmt.Errorf("connector %q: %s is unset or too short", cc.Name, cc.TokenEnv)
			}
			cs = append(cs, &SCIMClient{ConnName: cc.Name, Base: trimSlash(cc.URL), Token: tok, HTTP: NewOutboundClient(cc.AllowPrivate, 0)})
		case "webhook":
			sec := getenv(cc.SecretEnv)
			if len(sec) < 16 {
				return nil, fmt.Errorf("connector %q: %s is unset or shorter than 16 characters", cc.Name, cc.SecretEnv)
			}
			cs = append(cs, &WebhookConnector{ConnName: cc.Name, URL: cc.URL, Secret: sec, HTTP: NewOutboundClient(cc.AllowPrivate, 0)})
		default:
			cs = append(cs, &Stub{ConnName: cc.Name, Kind: cc.Type})
		}
	}
	return NewConnectorSet(cs...)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// syncGroups reports whether a connector receives the person's SCIM groups as downstream group
// memberships. Default NO: scim.write on a group is otherwise a grant of downstream access.
func (c ConnConfig) syncGroups() bool { return c.SyncGroups != nil && *c.SyncGroups }

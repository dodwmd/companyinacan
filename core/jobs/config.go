package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is the non-secret configuration (jobs.yaml). Every field has a default so the file is
// optional, except company.name and company.base_url which a real deployment must set.
// Secrets come from the environment and the mounted keyring, never from this file.
type Config struct {
	Listen       string `yaml:"listen"`
	DBPath       string `yaml:"db_path"`
	ResumeDir    string `yaml:"resume_dir"`
	KeyringPath  string `yaml:"keyring_path"`
	MaxBodyBytes int64  `yaml:"max_body_bytes"` // API JSON bodies
	TrustProxy   bool   `yaml:"trust_proxy"`
	// DBTimeoutSeconds bounds the database work of one request.
	DBTimeoutSeconds int `yaml:"db_timeout_seconds"`

	Company struct {
		Name    string `yaml:"name"`
		BaseURL string `yaml:"base_url"` // public origin of the company site, no trailing slash
		LogoURL string `yaml:"logo_url"`
	} `yaml:"company"`

	RateLimit struct {
		PerMinute          int `yaml:"per_minute"`               // per API token
		AuthFailuresPerMin int `yaml:"auth_failures_per_minute"` // per client address, failures only
		ApplyPerHour       int `yaml:"apply_per_hour"`           // per client address, public applications
		MaxTracked         int `yaml:"max_tracked"`
	} `yaml:"rate_limit"`

	Apply struct {
		MinFillSeconds   int      `yaml:"min_fill_seconds"`
		TokenTTLMinutes  int      `yaml:"token_ttl_minutes"`
		MaxResumeBytes   int64    `yaml:"max_resume_bytes"`
		AllowedOrigins   []string `yaml:"allowed_origins"`
		CaptchaProvider  string   `yaml:"captcha_provider"` // "", turnstile, hcaptcha or recaptcha
		ConsentVersion   string   `yaml:"consent_version"`  // shown with the consent checkbox; recorded per candidate
		ConsentText      string   `yaml:"consent_text"`
		CollectEEO       bool     `yaml:"collect_eeo"`
		DefaultValidDays int      `yaml:"default_valid_days"` // validThrough when a job does not set one
		ScannerRequired  bool     `yaml:"scanner_required"`   // refuse resumes while no real scanner is wired
	} `yaml:"apply"`

	Approval struct {
		Required bool `yaml:"required"` // a job must be approved before it can open
	} `yaml:"approval"`

	EEO struct {
		MinCell int `yaml:"min_cell"` // report cells below this are suppressed
	} `yaml:"eeo"`

	Webhook struct {
		URL string `yaml:"url"`
	} `yaml:"webhook"`

	// Retention. Days count from the decision on the candidate's last application.
	Retention struct {
		RejectedDays       int `yaml:"rejected_days"`        // candidates never hired
		TalentPoolDays     int `yaml:"talent_pool_days"`     // candidates who opted into the talent pool
		HiredDays          int `yaml:"hired_days"`           // after the hire hand-off to people
		EEODays            int `yaml:"eeo_days"`             // anonymous diversity responses
		ClosedJobDays      int `yaml:"closed_job_days"`      // closed jobs with no applications left
		SweepIntervalHours int `yaml:"sweep_interval_hours"` // how often the background sweep runs
	} `yaml:"retention"`
}

// DefaultConfig returns the defaults. Retention periods are starting points the operator must
// review against local law (docs/recruiting.md); they are not advice.
func DefaultConfig() *Config {
	c := &Config{Listen: ":8080", DBPath: "/data/jobs.db", ResumeDir: "/data/resumes", KeyringPath: "/etc/can-jobs/keyring",
		MaxBodyBytes: 1 << 20, DBTimeoutSeconds: 8}
	c.RateLimit.PerMinute = 600
	c.RateLimit.AuthFailuresPerMin = 30
	c.RateLimit.ApplyPerHour = 10
	c.RateLimit.MaxTracked = 10000
	c.Apply.MinFillSeconds = 3
	c.Apply.TokenTTLMinutes = 120
	c.Apply.MaxResumeBytes = 5 << 20
	c.Apply.ConsentVersion = "v1"
	c.Apply.ConsentText = "I agree that the company processes my application data to consider me for this role and keeps it for the retention period stated in its privacy notice."
	c.Apply.DefaultValidDays = 60
	c.EEO.MinCell = 5
	c.Retention.RejectedDays = 365
	c.Retention.TalentPoolDays = 730
	c.Retention.HiredDays = 180
	c.Retention.EEODays = 1095
	c.Retention.ClosedJobDays = 1095
	c.Retention.SweepIntervalHours = 6
	return c
}

// LoadConfig reads path (strict YAML: unknown keys are errors). A missing file is fine unless
// mustExist.
func LoadConfig(path string, mustExist bool) (*Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
		switch {
		case errors.Is(err, os.ErrNotExist) && !mustExist:
		case err != nil:
			return nil, err
		default:
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			dec.KnownFields(true)
			if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return cfg, cfg.Validate()
}

// Validate reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	add := func(s string) { errs = append(errs, errors.New(s)) }
	if c.Listen == "" || c.DBPath == "" || c.ResumeDir == "" {
		add("listen, db_path and resume_dir are required")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 16<<20 {
		add("max_body_bytes must be between 1024 and 16777216")
	}
	if c.DBTimeoutSeconds < 1 || c.DBTimeoutSeconds > 60 {
		add("db_timeout_seconds must be between 1 and 60")
	}
	if c.Company.BaseURL != "" {
		u, err := url.Parse(c.Company.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" {
			add("company.base_url must be an origin such as https://example.com")
		}
	}
	if c.Company.LogoURL != "" {
		if u, err := url.Parse(c.Company.LogoURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add("company.logo_url must be an http(s) URL")
		}
	}
	if c.RateLimit.PerMinute < 0 || c.RateLimit.AuthFailuresPerMin < 0 || c.RateLimit.ApplyPerHour < 0 || c.RateLimit.MaxTracked < 100 {
		add("rate_limit: values must be >= 0 and max_tracked >= 100")
	}
	if c.Apply.MinFillSeconds < 0 || c.Apply.MinFillSeconds > 60 || c.Apply.TokenTTLMinutes < 1 || c.Apply.TokenTTLMinutes > 1440 {
		add("apply: min_fill_seconds 0-60 and token_ttl_minutes 1-1440")
	}
	if c.Apply.MaxResumeBytes < 1024 || c.Apply.MaxResumeBytes > 25<<20 {
		add("apply.max_resume_bytes must be between 1024 and 26214400")
	}
	switch c.Apply.CaptchaProvider {
	case "", "turnstile", "hcaptcha", "recaptcha":
	default:
		add("apply.captcha_provider must be turnstile, hcaptcha, recaptcha or empty")
	}
	if c.Apply.ConsentVersion == "" || len(c.Apply.ConsentVersion) > 20 || len(c.Apply.ConsentText) < 20 || len(c.Apply.ConsentText) > 1000 {
		add("apply.consent_version (at most 20 characters) and apply.consent_text (20 to 1000) are required")
	}
	if c.Apply.DefaultValidDays < 1 || c.Apply.DefaultValidDays > 365 {
		add("apply.default_valid_days must be between 1 and 365")
	}
	for _, o := range c.Apply.AllowedOrigins {
		if u, err := url.Parse(o); err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			add(fmt.Sprintf("apply.allowed_origins: %q is not an origin", o))
		}
	}
	if c.EEO.MinCell < 1 {
		add("eeo.min_cell must be at least 1")
	}
	if c.Webhook.URL != "" {
		if u, err := url.Parse(c.Webhook.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add("webhook.url must be an http(s) URL")
		}
	}
	r := c.Retention
	if r.RejectedDays < 1 || r.TalentPoolDays < 1 || r.HiredDays < 0 || r.EEODays < 0 || r.ClosedJobDays < 0 || r.SweepIntervalHours < 1 {
		add("retention: rejected_days and talent_pool_days must be >= 1, the others >= 0, sweep_interval_hours >= 1")
	}
	return errors.Join(errs...)
}

// CareersPath is where the public pages are mounted. Put a path-rewriting proxy rule in front
// if the company site wants them elsewhere.
const CareersPath = "/careers"

// Site is the public identity the renderers and feeds use.
func (c *Config) Site() SiteInfo {
	return SiteInfo{Company: c.Company.Name, BaseURL: strings.TrimRight(c.Company.BaseURL, "/"), CareersPath: CareersPath, LogoURL: c.Company.LogoURL}
}

package people

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

// Config is the service configuration. Secrets never live in it: tokens come from the
// environment, and the encryption keyring from a mounted file.
type Config struct {
	Listen      string            `yaml:"listen"`
	DBPath      string            `yaml:"db_path"`
	KeyringFile string            `yaml:"keyring_file"`
	Webhook     WebhookConfig     `yaml:"webhook"`
	Introspect  IntrospectConfig  `yaml:"introspect"`
	Retention   map[string]string `yaml:"retention"`
}

// WebhookConfig is where outbox events are delivered (can-core's event endpoint).
type WebhookConfig struct {
	URL string `yaml:"url"`
}

// IntrospectConfig points at the token-introspection endpoint that turns user tokens into
// {subject, roles}. Without it only the service-account token authenticates.
type IntrospectConfig struct {
	URL string `yaml:"url"`
	// AllowInsecureHTTP permits a plain-http, non-loopback URL (for example an in-cluster
	// Service protected by NetworkPolicy). The default refuses it: the endpoint receives
	// every user's bearer token.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`
}

// Secrets are read from the environment.
type Secrets struct {
	APIToken        string // service-account bearer with hr+admin roles, used by can-core (CAN_PEOPLE_API_TOKEN)
	WebhookSecret   string // HMAC secret for outbox deliveries (CAN_PEOPLE_WEBHOOK_SECRET)
	IntrospectToken string // credential for the introspection endpoint (CAN_PEOPLE_INTROSPECT_TOKEN)
}

// SecretsFromEnv loads and checks the secrets against cfg.
func SecretsFromEnv(getenv func(string) string, cfg *Config) (Secrets, error) {
	s := Secrets{APIToken: getenv("CAN_PEOPLE_API_TOKEN"), WebhookSecret: getenv("CAN_PEOPLE_WEBHOOK_SECRET"), IntrospectToken: getenv("CAN_PEOPLE_INTROSPECT_TOKEN")}
	var errs []error
	if len(s.APIToken) < 24 {
		errs = append(errs, errors.New("CAN_PEOPLE_API_TOKEN is required and must be at least 24 characters"))
	}
	if cfg.Webhook.URL != "" && len(s.WebhookSecret) < 16 {
		errs = append(errs, errors.New("webhook.url is set but CAN_PEOPLE_WEBHOOK_SECRET is missing or shorter than 16 characters"))
	}
	return s, errors.Join(errs...)
}

// LoadConfig reads and validates a config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, err
	}
	c, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// ParseConfig decodes strictly (unknown keys are errors), applies defaults and validates.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = ":8081"
	}
	if c.DBPath == "" {
		c.DBPath = "/data/people.db"
	}
	var errs []error
	if c.KeyringFile == "" {
		errs = append(errs, errors.New("keyring_file is required: sensitive fields are always encrypted"))
	}
	for _, u := range []struct{ name, v string }{{"webhook.url", c.Webhook.URL}, {"introspect.url", c.Introspect.URL}} {
		if u.v == "" {
			continue
		}
		p, err := url.Parse(u.v)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil {
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL without credentials", u.name))
		}
	}
	if u, err := url.Parse(c.Introspect.URL); c.Introspect.URL != "" && err == nil && u.Scheme == "http" && !c.Introspect.AllowInsecureHTTP {
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
			errs = append(errs, errors.New("introspect.url must be https (or loopback); set introspect.allow_insecure_http only for a protected in-cluster Service"))
		}
	}
	known := map[string]bool{}
	for _, cl := range Classes {
		known[cl] = true
	}
	for cl, v := range c.Retention {
		if !known[cl] {
			errs = append(errs, fmt.Errorf("retention: unknown data class %q (known: %s)", cl, strings.Join(Classes, ", ")))
			continue
		}
		if _, err := ParseRetention(v); err != nil {
			errs = append(errs, fmt.Errorf("retention.%s: %w", cl, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &c, nil
}

// RetentionRules merges the configured retention over the defaults.
func (c *Config) RetentionRules() (map[string]RetentionRule, error) {
	r := DefaultRetention()
	for cl, v := range c.Retention {
		rule, err := ParseRetention(v)
		if err != nil {
			return nil, err
		}
		r[cl] = rule
	}
	return r, nil
}

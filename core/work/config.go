package work

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"

	"go.yaml.in/yaml/v3"
)

// Config is the non-secret configuration (work.yaml). Every field has a default, so the
// file is optional. Secrets come from the environment, never from this file.
type Config struct {
	Listen       string `yaml:"listen"`
	DBPath       string `yaml:"db_path"`
	MaxBodyBytes int64  `yaml:"max_body_bytes"`
	TrustProxy   bool   `yaml:"trust_proxy"`
	RateLimit    struct {
		PerMinute          int `yaml:"per_minute"`               // per token
		AuthFailuresPerMin int `yaml:"auth_failures_per_minute"` // per client address
		MaxTracked         int `yaml:"max_tracked"`
	} `yaml:"rate_limit"`
	Webhook struct {
		URL string `yaml:"url"`
	} `yaml:"webhook"`
	// RetentionDays deletes issues resolved longer ago than this (0 keeps them forever).
	RetentionDays      int `yaml:"retention_days"`
	AuditRetentionDays int `yaml:"audit_retention_days"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() *Config {
	c := &Config{Listen: ":8080", DBPath: "/data/work.db", MaxBodyBytes: 1 << 20, RetentionDays: 1825, AuditRetentionDays: 2555}
	c.RateLimit.PerMinute = 600
	c.RateLimit.AuthFailuresPerMin = 30
	c.RateLimit.MaxTracked = 10000
	return c
}

// LoadConfig reads path (strict YAML: unknown keys are errors). An empty path or a missing
// default file yields the defaults.
func LoadConfig(path string, mustExist bool) (*Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist) && !mustExist:
		case err != nil:
			return nil, err
		default:
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			dec.KnownFields(true)
			if err := dec.Decode(cfg); err != nil && !errors.Is(err, errEOF) {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return cfg, cfg.Validate()
}

// Validate reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if c.DBPath == "" {
		errs = append(errs, errors.New("db_path is required"))
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 16<<20 {
		errs = append(errs, errors.New("max_body_bytes must be between 1024 and 16777216"))
	}
	if c.RateLimit.PerMinute < 0 || c.RateLimit.AuthFailuresPerMin < 0 || c.RateLimit.MaxTracked < 100 {
		errs = append(errs, errors.New("rate_limit: per_minute and auth_failures_per_minute must be >= 0 and max_tracked >= 100"))
	}
	if c.Webhook.URL != "" {
		if u, err := url.Parse(c.Webhook.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, errors.New("webhook.url must be an http(s) URL"))
		}
	}
	if c.RetentionDays < 0 || c.AuditRetentionDays < 0 {
		errs = append(errs, errors.New("retention_days and audit_retention_days must be >= 0"))
	}
	return errors.Join(errs...)
}

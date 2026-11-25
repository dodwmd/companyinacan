package finance

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is the non-secret configuration (finance.yaml). Every field has a default, so the
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
	// BaseCurrency is the company's reporting currency; every document stores its FX rate to it.
	BaseCurrency string `yaml:"base_currency"`
	// PaymentInstructions is the default "how to pay" text printed on invoices.
	PaymentInstructions string `yaml:"payment_instructions"`
	Seller              struct {
		Name    string `yaml:"name"`
		Address string `yaml:"address"`
		TaxID   string `yaml:"tax_id"`
	} `yaml:"seller"`
	// PublicBaseURL prefixes the signed invoice links the API returns (no trailing slash).
	PublicBaseURL string `yaml:"public_base_url"`
	// PaymentProvider and ESignProvider: "none" (default) or "fake" (development only).
	PaymentProvider string `yaml:"payment_provider"`
	ESignProvider   string `yaml:"esign_provider"`
	// RetentionYears is how long financial records are kept after their own date (0 keeps
	// them forever). AuditRetentionDays must be at least 365: the database refuses sooner.
	RetentionYears     int `yaml:"retention_years"`
	AuditRetentionDays int `yaml:"audit_retention_days"`
	// UI is the server-rendered web UI (specs/031-finance-ui). Off by default: the module then
	// serves the JSON API only.
	UI UIConfig `yaml:"ui"`
}

// UIConfig turns on the web UI at the root of the listener; /v1/*, /public/* and /healthz stay
// with the API.
type UIConfig struct {
	Enabled bool `yaml:"enabled"`
	// InsecureCookies drops the Secure attribute (and the __Host- cookie prefix) for plain-HTTP
	// local development. Cookies are Secure by default.
	InsecureCookies bool `yaml:"insecure_cookies"`
	// ForceSecure is the production guard: Secure cookies even when the listener sits behind a
	// TLS-terminating proxy and sees plain HTTP. It cannot be combined with insecure_cookies.
	ForceSecure bool `yaml:"force_secure"`
	// NotificationsURL, when set, adds a Notifications link to the navigation.
	NotificationsURL string `yaml:"notifications_url"`
	// IdleMinutes and AbsoluteHours bound a session (defaults 30 minutes and 8 hours).
	IdleMinutes   int `yaml:"idle_minutes"`
	AbsoluteHours int `yaml:"absolute_hours"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() *Config {
	c := &Config{Listen: ":8080", DBPath: "/data/finance.db", MaxBodyBytes: 1 << 20, BaseCurrency: "USD", PaymentProvider: "none", ESignProvider: "none", RetentionYears: 7, AuditRetentionDays: 2555}
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
	if _, ok := CurrencyExponent(c.BaseCurrency); !ok {
		errs = append(errs, errors.New("base_currency must be a known ISO 4217 code such as USD"))
	}
	for k, v := range map[string]string{"payment_provider": c.PaymentProvider, "esign_provider": c.ESignProvider} {
		if v != "none" && v != "fake" {
			errs = append(errs, fmt.Errorf("%s must be none or fake (real adapters are separate modules; see docs/finance.md)", k))
		}
	}
	if !validText(c.PaymentInstructions, maxText) || !validText(c.Seller.Address, 500) || !validText(c.Seller.Name, maxName) {
		errs = append(errs, errors.New("payment_instructions and seller fields are too long"))
	}
	if c.PublicBaseURL != "" {
		if u, err := url.Parse(c.PublicBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.HasSuffix(c.PublicBaseURL, "/") {
			errs = append(errs, errors.New("public_base_url must be an http(s) URL without a trailing slash"))
		}
	}
	if c.RetentionYears < 0 || (c.AuditRetentionDays != 0 && c.AuditRetentionDays < 365) {
		errs = append(errs, errors.New("retention_years must be >= 0 and audit_retention_days at least 365 (or 0 to keep everything)"))
	}
	if c.UI.InsecureCookies && c.UI.ForceSecure {
		errs = append(errs, errors.New("ui.insecure_cookies and ui.force_secure contradict each other: pick one"))
	}
	if c.UI.IdleMinutes < 0 || c.UI.IdleMinutes > 24*60 || c.UI.AbsoluteHours < 0 || c.UI.AbsoluteHours > 24*30 {
		errs = append(errs, errors.New("ui.idle_minutes and ui.absolute_hours are out of range"))
	}
	if u := c.UI.NotificationsURL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" || p.User != nil {
			errs = append(errs, errors.New("ui.notifications_url must be an http(s) URL without credentials"))
		}
	}
	return errors.Join(errs...)
}

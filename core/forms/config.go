// Package forms implements the in-house forms gateway: public form intake with spam
// filtering, SQLite storage, the `leads` capability API, notifications and the
// `lead.created.v1` event.
package forms

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	coreconfig "github.com/dodwmd/companyinacan/core/config"
)

// Config is the gateway configuration, loaded from YAML. Secrets are never part of it;
// they come from the environment (see Secrets).
type Config struct {
	Listen             string          `yaml:"listen"`
	APIListen          string          `yaml:"api_listen"`
	DBPath             string          `yaml:"db_path"`
	PathPrefix         string          `yaml:"path_prefix"`
	TrustProxy         bool            `yaml:"trust_proxy"`
	MaxBodyBytes       int64           `yaml:"max_body_bytes"`
	CORSAllowedOrigins []string        `yaml:"cors_allowed_origins"`
	RateLimit          RateLimitConfig `yaml:"rate_limit"`
	RetentionDays      int             `yaml:"retention_days"`
	// NotifyEmail is the notify_email of every form that has none of its own (the chart sets it from
	// the module setting of the same name).
	NotifyEmail []string      `yaml:"notify_email"`
	Email       EmailConfig   `yaml:"email"`
	Webhook     WebhookConfig `yaml:"webhook"`
	Captcha     CaptchaConfig `yaml:"captcha"`
	Forms       []Form        `yaml:"forms"`
}

// RateLimitConfig bounds submissions per client IP.
type RateLimitConfig struct {
	PerMinute     int `yaml:"per_minute"`
	MaxTrackedIPs int `yaml:"max_tracked_ips"`
}

// EmailConfig points at an email.send endpoint. An installed module leaves it empty and uses
// can-core. From is ignored (kept so older files still load): the sender is can-core's.
type EmailConfig struct {
	URL  string `yaml:"url"`
	From string `yaml:"from"`
}

// WebhookConfig is the direct webhook used until the event bus exists.
type WebhookConfig struct {
	URL            string `yaml:"url"`
	MaxAttempts    int    `yaml:"max_attempts"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// CaptchaConfig selects the optional challenge provider.
type CaptchaConfig struct {
	Provider string `yaml:"provider"` // none | turnstile | hcaptcha
}

// Form is one public form definition.
type Form struct {
	ID             string   `yaml:"id"`
	Name           string   `yaml:"name"`
	Fields         []Field  `yaml:"fields"`
	NotifyEmail    []string `yaml:"notify_email"`
	Redirect       string   `yaml:"redirect"`
	Honeypot       string   `yaml:"honeypot"`
	MinFillSeconds *int     `yaml:"min_fill_seconds"`
	Captcha        bool     `yaml:"captcha"`
	RetentionDays  int      `yaml:"retention_days"`
}

// Field is one accepted input of a form.
type Field struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"` // text | email | select
	Required  bool     `yaml:"required"`
	Multiline bool     `yaml:"multiline"` // allow line breaks (text fields only)
	MinLength int      `yaml:"min_length"`
	MaxLength int      `yaml:"max_length"`
	Options   []string `yaml:"options"`
}

// Field types.
const (
	TypeText   = "text"
	TypeEmail  = "email"
	TypeSelect = "select"
)

const (
	maxFieldLength = 10000
	maxFields      = 30
)

var (
	idRe        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)
	fieldNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
)

// MinFill is the minimum time between token issue and submit.
func (f *Form) MinFill() time.Duration {
	if f.MinFillSeconds == nil {
		return 0
	}
	return time.Duration(*f.MinFillSeconds) * time.Second
}

// Form returns the definition with the given id, or nil.
func (c *Config) Form(id string) *Form {
	for i := range c.Forms {
		if c.Forms[i].ID == id {
			return &c.Forms[i]
		}
	}
	return nil
}

// LoadConfig reads and validates a config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
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
// All problems are reported together.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&c.Listen, ":8080")
	def(&c.APIListen, ":8081")
	def(&c.DBPath, "/data/forms.db")
	def(&c.PathPrefix, "/_forms")
	def(&c.Captcha.Provider, "none")
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 65536
	}
	if c.RateLimit.PerMinute == 0 {
		c.RateLimit.PerMinute = 10
	}
	if c.RateLimit.MaxTrackedIPs == 0 {
		c.RateLimit.MaxTrackedIPs = 10000
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 730
	}
	if c.Webhook.MaxAttempts == 0 {
		c.Webhook.MaxAttempts = 5
	}
	if c.Webhook.TimeoutSeconds == 0 {
		c.Webhook.TimeoutSeconds = 10
	}
	for i := range c.Forms {
		f := &c.Forms[i]
		def(&f.Honeypot, "website")
		if len(f.NotifyEmail) == 0 {
			f.NotifyEmail = c.NotifyEmail
		}
		if f.MinFillSeconds == nil {
			three := 3
			f.MinFillSeconds = &three
		}
		if f.RetentionDays == 0 {
			f.RetentionDays = c.RetentionDays
		}
		for j := range f.Fields {
			fl := &f.Fields[j]
			if fl.MaxLength == 0 {
				fl.MaxLength = 1000
				if fl.Type == TypeEmail {
					fl.MaxLength = 254
				}
			}
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !strings.HasPrefix(c.PathPrefix, "/") || strings.HasSuffix(c.PathPrefix, "/") && c.PathPrefix != "/" || strings.ContainsAny(c.PathPrefix, " ?#") {
		bad("path_prefix %q must start with / and have no trailing slash", c.PathPrefix)
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 10<<20 {
		bad("max_body_bytes %d out of range (1024..10485760)", c.MaxBodyBytes)
	}
	if c.RateLimit.PerMinute < 1 || c.RateLimit.MaxTrackedIPs < 10 {
		bad("rate_limit: per_minute must be >= 1 and max_tracked_ips >= 10")
	}
	if c.RetentionDays < 1 {
		bad("retention_days must be positive")
	}
	switch c.Captcha.Provider {
	case "none", "turnstile", "hcaptcha":
	default:
		bad("captcha.provider %q must be none, turnstile or hcaptcha", c.Captcha.Provider)
	}
	if c.Webhook.URL != "" {
		if u, err := url.Parse(c.Webhook.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("webhook.url %q must be an http(s) URL", c.Webhook.URL)
		}
		if c.Webhook.MaxAttempts < 1 || c.Webhook.MaxAttempts > 10 {
			bad("webhook.max_attempts must be 1..10")
		}
	}
	if c.Email.URL != "" {
		if u, err := url.Parse(c.Email.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("email.url %q must be an http(s) URL", c.Email.URL)
		}
	}
	for _, o := range c.CORSAllowedOrigins {
		if u, err := url.Parse(o); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			bad("cors_allowed_origins: %q must be an origin like https://example.com", o)
		}
	}
	if len(c.Forms) == 0 {
		bad("at least one form must be defined")
	}
	seen := map[string]bool{}
	for i := range c.Forms {
		f := &c.Forms[i]
		if !idRe.MatchString(f.ID) {
			bad("forms[%d]: id %q must match %s", i, f.ID, idRe)
		}
		if seen[f.ID] {
			bad("forms[%d]: duplicate id %q", i, f.ID)
		}
		seen[f.ID] = true
		c.validateForm(f, bad)
	}
	return errors.Join(errs...)
}

func (c *Config) validateForm(f *Form, bad func(string, ...any)) {
	if f.Redirect != "" && (!strings.HasPrefix(f.Redirect, "/") || strings.HasPrefix(f.Redirect, "//") || strings.ContainsAny(f.Redirect, "\\\r\n")) {
		bad("form %q: redirect %q must be a same-origin path", f.ID, f.Redirect)
	}
	for _, e := range f.NotifyEmail {
		// The rule can-core applies to a recipient: anything else could never be sent.
		if !coreconfig.ValidAddress(e) {
			bad("form %q: notify_email %q is not a valid address", f.ID, e)
		}
	}
	if f.Captcha && c.Captcha.Provider == "none" {
		bad("form %q: captcha is enabled but captcha.provider is none", f.ID)
	}
	if !fieldNameRe.MatchString(f.Honeypot) {
		bad("form %q: honeypot %q must match %s", f.ID, f.Honeypot, fieldNameRe)
	}
	if f.MinFillSeconds != nil && (*f.MinFillSeconds < 0 || *f.MinFillSeconds > 600) {
		bad("form %q: min_fill_seconds must be 0..600", f.ID)
	}
	if len(f.Fields) == 0 || len(f.Fields) > maxFields {
		bad("form %q: needs 1..%d fields", f.ID, maxFields)
	}
	names := map[string]bool{}
	for _, fl := range f.Fields {
		if !fieldNameRe.MatchString(fl.Name) {
			bad("form %q: field name %q must match %s", f.ID, fl.Name, fieldNameRe)
		}
		if names[fl.Name] {
			bad("form %q: duplicate field %q", f.ID, fl.Name)
		}
		names[fl.Name] = true
		if fl.Name == f.Honeypot {
			bad("form %q: field %q clashes with the honeypot name", f.ID, fl.Name)
		}
		switch fl.Type {
		case TypeText, TypeEmail:
		case TypeSelect:
			if len(fl.Options) == 0 {
				bad("form %q: select field %q needs options", f.ID, fl.Name)
			}
		default:
			bad("form %q: field %q has unknown type %q (text, email, select)", f.ID, fl.Name, fl.Type)
		}
		if fl.MaxLength < 1 || fl.MaxLength > maxFieldLength {
			bad("form %q: field %q max_length must be 1..%d", f.ID, fl.Name, maxFieldLength)
		}
		if fl.MinLength < 0 || fl.MinLength > fl.MaxLength {
			bad("form %q: field %q min_length must be 0..max_length", f.ID, fl.Name)
		}
	}
}

// cleanEmail accepts only a bare addr-spec (no display name, no CR/LF) up to 254 bytes.
func cleanEmail(s string) (string, bool) {
	if s == "" || len(s) > 254 || strings.ContainsAny(s, "\r\n\x00<>,;\" ") {
		return "", false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return "", false
	}
	return a.Address, true
}

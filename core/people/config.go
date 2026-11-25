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
	Reviews     ReviewsConfig     `yaml:"reviews"`
	UI          UIConfig          `yaml:"ui"`
}

// ReviewsConfig holds the review defaults.
type ReviewsConfig struct {
	// MinResponses is the default number of feedback answers needed before any feedback about a
	// person is shown (at least 3: fewer could identify the authors). A cycle can raise it.
	MinResponses int `yaml:"min_responses"`
}

// UIConfig turns on the server-rendered self-service web UI (specs/026-people-ui). It is off by
// default: the module then serves the JSON API only.
type UIConfig struct {
	Enabled bool `yaml:"enabled"`
	// InsecureCookies drops the Secure attribute (and the __Host- cookie prefix) for plain-HTTP
	// local development. Leave it false behind TLS.
	InsecureCookies bool `yaml:"insecure_cookies"`
	// TrustProxy reads the client address for failed-login throttling from X-Forwarded-For (the
	// last entry, as appended by the ingress). Set it only behind a proxy you control.
	TrustProxy bool `yaml:"trust_proxy"`
	// NotificationsURL, when set, adds a Notifications link to the navigation (the can-core admin
	// UI notifications page).
	NotificationsURL string `yaml:"notifications_url"`
	// IdleMinutes and AbsoluteHours bound a session (defaults 30 minutes and 8 hours).
	IdleMinutes   int `yaml:"idle_minutes"`
	AbsoluteHours int `yaml:"absolute_hours"`
	// TokenLogin keeps the "paste a personal access token" sign-in (default true, for
	// compatibility). Turn it off once single sign-on from the admin UI is in use: then the only
	// way into the UI is a launch ticket (specs/036-module-ui-sso).
	TokenLogin *bool `yaml:"token_login"`
}

// tokenLogin reports whether the pasted-token sign-in is offered (default true).
func (c UIConfig) tokenLogin() bool { return c.TokenLogin == nil || *c.TokenLogin }

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
	CanSecret       string // verifies the webhooks can-core delivers to this module (CAN_WEBHOOK_SECRET)
}

// SecretsFromEnv loads and checks the secrets against cfg.
func SecretsFromEnv(getenv func(string) string, cfg *Config) (Secrets, error) {
	s := Secrets{APIToken: getenv("CAN_PEOPLE_API_TOKEN"), WebhookSecret: getenv("CAN_PEOPLE_WEBHOOK_SECRET"), IntrospectToken: getenv("CAN_PEOPLE_INTROSPECT_TOKEN"), CanSecret: getenv("CAN_WEBHOOK_SECRET")}
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
	if c.Reviews.MinResponses != 0 && (c.Reviews.MinResponses < minAnonymity || c.Reviews.MinResponses > 50) {
		errs = append(errs, fmt.Errorf("reviews.min_responses must be between %d and 50", minAnonymity))
	}
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
	if u := c.UI.NotificationsURL; u != "" {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil {
			errs = append(errs, errors.New("ui.notifications_url must be an http(s) URL without credentials"))
		}
	}
	if c.UI.IdleMinutes < 0 || c.UI.IdleMinutes > 24*60 || c.UI.AbsoluteHours < 0 || c.UI.AbsoluteHours > 24*30 {
		errs = append(errs, errors.New("ui.idle_minutes and ui.absolute_hours are out of range"))
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

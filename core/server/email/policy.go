// Package email is can-core's `email.send` capability (epic E03d, specs/039-email-send): an
// operator-configured SMTP relay, a durable outbox with retries and dead letters, per-module
// recipient allow-lists and quotas, and the notification e-mail channel.
//
// No recipient address, subject or body is ever written to the audit log, a log line, an event or
// an error string: those carry message ids, module ids and fixed reason texts only.
package email

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
)

// TLS modes of a relay.
const (
	TLSStartTLS = "starttls" // plain connection upgraded with STARTTLS; required, never optional
	TLSImplicit = "implicit" // TLS from the first byte (port 465)
	TLSNone     = "none"     // development only, see Source.AllowInsecure
)

// SystemSource is the outbox source of mail can-core sends itself (notifications). It is not a
// possible module id.
const SystemSource = "@core"

// approvalPrefix begins the outbox source of mail about the approvals one module opened. Like
// SystemSource it is not a possible module id.
const approvalPrefix = "@approvals:"

// ApprovalSource is the outbox source of approval mail of module: its switch and limits, counted
// apart from the module's other mail.
func ApprovalSource(module string) string { return approvalPrefix + module }

// externalRecipientsModule is the only module external_recipients applies to.
const externalRecipientsModule = "finance"

// Default quotas, in messages (one per recipient).
const (
	DefaultModulePerHour = 60
	DefaultModulePerDay  = 500
	systemPerHour        = 300
	systemPerDay         = 2000
)

// Relay is the resolved SMTP relay.
type Relay struct {
	Host               string
	Port               int
	TLS                string
	From               string
	Username, Password string
}

// ErrNotConfigured means there is no usable relay. The wrapped text says why and never contains a
// credential.
var ErrNotConfigured = errors.New("no email relay is configured")

// ErrSecretUnreadable means the relay Secret is there but could not be read: what the relay is, is
// unknown, which is not the same as there being none.
var ErrSecretUnreadable = errors.New("the relay Secret cannot be read")

// Source says where the relay settings and sending rules come from. Both are read afresh on every
// use, so a changed Secret or company.yaml applies without a restart.
type Source struct {
	// CompanyFile is the applied company.yaml (may be absent).
	CompanyFile string
	// SecretDir is a mounted Secret with the keys host, username, password and optionally port, tls
	// and from (may be absent).
	SecretDir string
	// Domain is used when company.yaml is absent.
	Domain string
	// AllowInsecure permits tls: none (CAN_SMTP_ALLOW_INSECURE; development only).
	AllowInsecure bool
}

// Policy is a snapshot of the relay and the sending rules.
type Policy struct {
	Relay    Relay
	RelayErr error // wraps ErrNotConfigured when Relay is unusable
	domain   string
	modules  map[string]config.EmailModule
	notify   map[string][]string // module id -> its notify_email setting
	notices  []string            // email.notification_recipients; nil = the company domain
	// ssoVerified: single sign-on is on and configured to accept only verified email claims.
	ssoVerified bool
}

// NoticeAllowed reports whether the rules (not single sign-on) allow notification mail to addr.
func (p Policy) NoticeAllowed(addr string) bool {
	rules := p.notices
	if rules == nil && p.domain != "" {
		rules = []string{"@" + p.domain}
	}
	return config.RecipientAllowed(addr, rules)
}

// NotificationText reports whether the module's own wording may be mailed.
func (p Policy) NotificationText(module string) bool { return p.modules[module].NotificationText }

// Rules are what one source may send.
type Rules struct {
	Enabled bool
	// Recipients is the allow-list of the sender's own mail. Notifications have their own rules
	// (Policy.NoticeAllowed).
	Recipients []string
	PerHour    int
	PerDay     int
	// AnyRecipient: the operator opted this module out of the allow-list (external_recipients).
	AnyRecipient bool
}

// Rules returns the sending rules of a source.
func (p Policy) Rules(source string) Rules {
	if source == SystemSource {
		return Rules{Enabled: true, PerHour: systemPerHour, PerDay: systemPerDay}
	}
	source = strings.TrimPrefix(source, approvalPrefix)
	m := p.modules[source]
	r := Rules{Enabled: m.Enabled == nil || *m.Enabled, PerHour: m.PerHour, PerDay: m.PerDay, Recipients: m.Recipients,
		AnyRecipient: m.ExternalRecipients && source == externalRecipientsModule}
	if r.PerHour <= 0 {
		r.PerHour = DefaultModulePerHour
	}
	if r.PerDay <= 0 {
		r.PerDay = DefaultModulePerDay
	}
	// An explicit list (even an empty one) replaces the default.
	if m.Recipients == nil {
		if p.domain != "" {
			r.Recipients = append(r.Recipients, "@"+p.domain)
		}
		r.Recipients = append(r.Recipients, p.notify[source]...)
	}
	return r
}

const maxCompanyFile, maxSecretValue = 1 << 20, 4 << 10

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // operator-configured paths
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is too large", filepath.Base(path))
	}
	return b, nil
}

// Load reads the current policy. An unreadable or unparsable company.yaml is an error: the sending
// rules are unknown, so nothing may be sent.
func (s Source) Load() (Policy, error) {
	var co *config.Company
	if s.CompanyFile != "" {
		raw, err := readBounded(s.CompanyFile, maxCompanyFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return Policy{}, fmt.Errorf("reading company.yaml: %w", err)
		default:
			doc, ps := config.LoadBytes(s.CompanyFile, raw)
			if doc == nil {
				return Policy{}, fmt.Errorf("company.yaml does not parse (%d problems)", len(ps))
			}
			if co, err = config.DecodeCompany(doc); err != nil {
				return Policy{}, errors.New("company.yaml does not decode")
			}
		}
	}
	return s.For(co)
}

// For is Load with a company.yaml the caller already holds (nil: none); CompanyFile is not read.
func (s Source) For(co *config.Company) (Policy, error) {
	p := Policy{domain: s.Domain, notify: map[string][]string{}}
	var cr config.EmailRelay
	if co != nil {
		if co.Domain != "" {
			p.domain = co.Domain
		}
		for id, m := range co.Modules {
			list, _ := m.Settings["notify_email"].([]any)
			for _, v := range list {
				if a, ok := v.(string); ok && config.ValidAddress(a) {
					p.notify[id] = append(p.notify[id], a)
				}
			}
		}
		p.ssoVerified = co.SSO != nil && co.SSO.Enabled && co.SSO.EmailVerifiedRequired()
		if co.Email != nil {
			p.modules, p.notices = co.Email.Modules, co.Email.NotificationRecipients
			if co.Email.Relay != nil {
				cr = *co.Email.Relay
			}
		}
	}
	relay, err := s.relay(cr, p.domain)
	if err != nil {
		p.RelayErr = fmt.Errorf("%w: %w", ErrNotConfigured, err)
		return p, nil
	}
	p.Relay = relay
	return p, nil
}

func (s Source) secret(key string) (string, error) {
	if s.SecretDir == "" {
		return "", nil
	}
	b, err := readBounded(filepath.Join(s.SecretDir, key), maxSecretValue)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w (key %q)", ErrSecretUnreadable, key)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// relay merges the Secret over company.yaml. Credentials are bound to the host stored next to them:
// whoever can edit company.yaml must not be able to have the relay password sent to another server.
func (s Source) relay(c config.EmailRelay, domain string) (Relay, error) {
	vals := map[string]string{}
	for _, k := range []string{"host", "port", "tls", "from", "username", "password"} {
		v, err := s.secret(k)
		if err != nil {
			return Relay{}, err
		}
		vals[k] = v
	}
	r := Relay{Host: c.Host, Port: c.Port, TLS: c.TLS, From: c.From, Username: vals["username"], Password: vals["password"]}
	if h := vals["host"]; h != "" {
		if c.Host != "" && !strings.EqualFold(c.Host, h) {
			return Relay{}, errors.New("email.relay.host in company.yaml differs from the host in the relay Secret; the credentials are only sent to the host stored with them")
		}
		r.Host = h
	}
	if (r.Username != "" || r.Password != "") && vals["host"] == "" {
		return Relay{}, errors.New("the relay Secret has credentials but no host key")
	}
	if (r.Username == "") != (r.Password == "") {
		return Relay{}, errors.New("the relay Secret needs both username and password, or neither")
	}
	if r.Host == "" {
		return Relay{}, errors.New("no relay host is set (company.yaml email.relay or the Secret can-smtp-relay)")
	}
	if !config.ValidRelayHost(r.Host) {
		return Relay{}, errors.New("the relay host is not a host name or address")
	}
	if v := vals["port"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Relay{}, errors.New("the relay port is not a number")
		}
		r.Port = n
	}
	if r.Port == 0 {
		r.Port = 587
	}
	if r.Port < 1 || r.Port > 65535 {
		return Relay{}, errors.New("the relay port is out of range")
	}
	if v := vals["tls"]; v != "" {
		r.TLS = v
	}
	if r.TLS == "" {
		r.TLS = TLSStartTLS
		if r.Port == 465 {
			r.TLS = TLSImplicit
		}
	}
	switch r.TLS {
	case TLSStartTLS, TLSImplicit:
	case TLSNone:
		if !s.AllowInsecure {
			return Relay{}, errors.New("tls: none is refused (development only: CAN_SMTP_ALLOW_INSECURE=true)")
		}
		if r.Username != "" {
			return Relay{}, errors.New("credentials are never sent without TLS")
		}
	default:
		return Relay{}, errors.New("the relay tls mode must be starttls or implicit")
	}
	if v := vals["from"]; v != "" {
		r.From = v
	}
	if r.From == "" && domain != "" {
		r.From = "no-reply@" + domain
	}
	if !config.ValidAddress(r.From) {
		return Relay{}, errors.New("the sender address (email.relay.from) is not a plain email address")
	}
	return r, nil
}

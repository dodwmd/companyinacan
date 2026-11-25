package provision

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/netguard"
)

// Identity is what a connector needs to create or update an account. It is read from the people
// directory when a step runs and goes to the downstream system only: it is never stored here and
// never put in an event or an audit entry.
type Identity struct {
	PartyID     string
	UserName    string // work email, the stable login
	GivenName   string
	FamilyName  string
	DisplayName string
	Active      bool
}

// Connector is a downstream system that holds accounts. Every method is idempotent for one key:
// the engine retries after a crash or a timeout and the connector must not duplicate anything.
// Methods return nil on success or a *ConnError; any other error counts as transient.
type Connector interface {
	Name() string
	// Verified is false for adapters that have never talked to the real service (the stubs).
	Verified() bool
	CreateAccount(ctx context.Context, key string, id Identity) (externalID string, err error)
	UpdateAccount(ctx context.Context, key, externalID string, id Identity) error
	DisableAccount(ctx context.Context, key, externalID string) error
	DeleteAccount(ctx context.Context, key, externalID string) error
	AddToGroup(ctx context.Context, key, externalID, group string) error
	RemoveFromGroup(ctx context.Context, key, externalID, group string) error
}

// ConnError carries only a short machine code, never a downstream response body, so a secret or
// a personal value in an error page cannot reach the database, the audit trail or the logs.
type ConnError struct {
	Code      string
	Permanent bool
}

func (e *ConnError) Error() string {
	if e.Permanent {
		return "connector: " + e.Code + " (permanent)"
	}
	return "connector: " + e.Code
}

func permanent(code string) error { return &ConnError{Code: code, Permanent: true} }
func transient(code string) error { return &ConnError{Code: code} }

// errCode extracts the machine code of an error and whether retrying cannot help.
func errCode(err error) (string, bool) {
	var ce *ConnError
	if errors.As(err, &ce) {
		return ce.Code, ce.Permanent
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", false
	}
	return "error", false
}

// classify maps a downstream HTTP status to nil, a transient or a permanent error.
func classify(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return transient(fmt.Sprintf("http_%d", status))
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return permanent("auth")
	default:
		return permanent(fmt.Sprintf("http_%d", status))
	}
}

const maxConnResponse = 1 << 20

// NewOutboundClient is the only HTTP client connectors use. Destinations are judged at connect
// time (so DNS rebinding does not help) and must be public unless allowPrivate is set for an
// in-cluster or loopback target the operator named on purpose. Redirects are not followed, proxies
// from the environment are ignored, and every call has a deadline.
func NewOutboundClient(allowPrivate bool, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		d.Control = netguard.DenyPrivate
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			DisableCompression:    true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// validateTarget checks a configured downstream URL: http(s), a host, no credentials, query or
// fragment, https unless the operator allowed plain HTTP, and (unless private targets are allowed)
// no literal non-public address, so a typo fails at start rather than at the first leaver.
func validateTarget(raw string, allowPrivate, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must be an absolute http(s) URL without credentials, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return nil, errors.New("plain http is refused (set allow_insecure_http to override)")
		}
	default:
		return nil, errors.New("must be an absolute http(s) URL")
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !allowPrivate && !netguard.Public(ip) {
		return nil, errors.New("destination is not a public address (set allow_private to override)")
	}
	return u, nil
}

// doJSON performs one outbound request and returns the status and a bounded body.
func doJSON(ctx context.Context, c *http.Client, method, target, contentType string, hdr map[string]string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return 0, nil, permanent("bad_request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json, application/scim+json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		// Never include err text: a URL with a secret query or an internal address could be in it.
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return 0, nil, transient("timeout")
		}
		var op *net.OpError
		if errors.As(err, &op) && strings.Contains(op.Error(), "not a public address") {
			return 0, nil, permanent("destination_denied")
		}
		return 0, nil, transient("network")
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxConnResponse))
	return resp.StatusCode, b, nil
}

// ConnectorSet is the configured connectors in their configured order.
type ConnectorSet struct {
	order []string
	m     map[string]Connector
}

// NewConnectorSet builds a set; names must be unique.
func NewConnectorSet(cs ...Connector) (*ConnectorSet, error) {
	s := &ConnectorSet{m: map[string]Connector{}}
	for _, c := range cs {
		if _, dup := s.m[c.Name()]; dup {
			return nil, fmt.Errorf("duplicate connector %q", c.Name())
		}
		s.m[c.Name()] = c
		s.order = append(s.order, c.Name())
	}
	return s, nil
}

// Get returns a connector by name.
func (s *ConnectorSet) Get(name string) (Connector, bool) {
	if s == nil {
		return nil, false
	}
	c, ok := s.m[name]
	return c, ok
}

// Names lists the connectors in configured order.
func (s *ConnectorSet) Names() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.order...)
}

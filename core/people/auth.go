package people

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrUnauthenticated means no valid credentials were presented.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator turns a request into a Principal (subject + roles). The seam for can-core
// SSO: IntrospectAuthenticator asks an introspection endpoint; StaticTokens serves the
// can-core service account. Implementations must fail closed.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

func bearer(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || len(tok) > 4096 {
		return "", false
	}
	return tok, true
}

func knownRoles(in []string) []string {
	var out []string
	for _, r := range in {
		switch r {
		case RoleEmployee, RoleManager, RoleHR, RoleAdmin:
			out = append(out, r)
		}
	}
	return out
}

type tokenEntry struct {
	hash [sha256.Size]byte
	p    Principal
}

// StaticTokens authenticates fixed bearer tokens in constant time.
type StaticTokens struct{ entries []tokenEntry }

// Add registers a token. Empty tokens are ignored so unset env vars cannot become credentials.
func (s *StaticTokens) Add(token string, p Principal) {
	if token == "" {
		return
	}
	p.Roles = knownRoles(p.Roles)
	s.entries = append(s.entries, tokenEntry{hash: sha256.Sum256([]byte(token)), p: p})
}

// Authenticate implements Authenticator, comparing against every entry.
func (s *StaticTokens) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := bearer(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	h := sha256.Sum256([]byte(tok))
	var found *Principal
	for i := range s.entries {
		if subtle.ConstantTimeCompare(h[:], s.entries[i].hash[:]) == 1 {
			p := s.entries[i].p
			found = &p
		}
	}
	if found == nil {
		return nil, ErrUnauthenticated
	}
	return found, nil
}

// IntrospectAuthenticator validates user tokens against an introspection endpoint
// (POST {"token": ...} -> {"active": true, "subject": "...", "roles": [...]}). Positive
// answers are cached for a short TTL; negative answers, errors and malformed responses are
// never cached or trusted.
type IntrospectAuthenticator struct {
	URL    string
	Token  string // this service's own credential for the endpoint
	Client *http.Client
	TTL    time.Duration
	Now    func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cached
}

type cached struct {
	p       Principal
	expires time.Time
}

const maxIntrospectCache = 1024

// Authenticate implements Authenticator.
func (a *IntrospectAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := bearer(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	key := sha256.Sum256([]byte(tok))
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && now().Before(c.expires) {
		a.mu.Unlock()
		p := c.p
		return &p, nil
	}
	a.mu.Unlock()

	body, _ := json.Marshal(map[string]string{"token": tok})
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnauthenticated
	}
	req.Header.Set("Content-Type", "application/json")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnauthenticated
	}
	var out struct {
		Active  bool     `json:"active"`
		Subject string   `json:"subject"`
		Roles   []string `json:"roles"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || !out.Active || !subjectRe.MatchString(out.Subject) {
		return nil, ErrUnauthenticated
	}
	p := Principal{Subject: out.Subject, Roles: knownRoles(out.Roles)}
	ttl := a.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	a.mu.Lock()
	if a.cache == nil || len(a.cache) >= maxIntrospectCache {
		a.cache = map[[sha256.Size]byte]cached{}
	}
	a.cache[key] = cached{p: p, expires: now().Add(ttl)}
	a.mu.Unlock()
	return &p, nil
}

// MultiAuth tries each authenticator in order and fails closed when none accepts.
type MultiAuth []Authenticator

// Authenticate implements Authenticator.
func (m MultiAuth) Authenticate(r *http.Request) (*Principal, error) {
	for _, a := range m {
		if p, err := a.Authenticate(r); err == nil {
			return p, nil
		}
	}
	return nil, ErrUnauthenticated
}

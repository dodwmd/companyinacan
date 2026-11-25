package board

import (
	"bytes"
	"container/list"
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

// knownRoles filters the roles an SSO introspection answer may grant: `integration` and `admin` are service
// roles and come from the static service-account tokens only.
func knownRoles(in []string) []string {
	var out []string
	for _, r := range in {
		switch r {
		case RoleDirector, RoleOfficer, RoleSecretary, RoleObserver, RoleAuditViewer, RoleEmployee:
			out = append(out, r)
		}
	}
	return out
}

func staticRoles(in []string) []string {
	var out []string
	for _, r := range in {
		if r == RoleAdmin || r == RoleIntegration {
			out = append(out, r)
		}
	}
	return append(knownRoles(in), out...)
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
	p.Roles = staticRoles(p.Roles)
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
	cache *lru
}

// lru is a bounded least-recently-used cache of introspection answers, positive and negative.
// Negative answers are kept briefly so a client replaying a bad token cannot turn every request
// into a call to the introspection endpoint.
type lru struct {
	max int
	ll  *list.List
	m   map[[sha256.Size]byte]*list.Element
}

type lruEntry struct {
	key     [sha256.Size]byte
	p       *Principal // nil: a definitive "no"
	expires time.Time
}

const (
	maxIntrospectCache = 1024
	negativeTTL        = 5 * time.Second
)

func newLRU(max int) *lru {
	return &lru{max: max, ll: list.New(), m: map[[sha256.Size]byte]*list.Element{}}
}

func (c *lru) get(k [sha256.Size]byte, now time.Time) (*lruEntry, bool) {
	e, ok := c.m[k]
	if !ok {
		return nil, false
	}
	en := e.Value.(*lruEntry)
	if !now.Before(en.expires) {
		c.ll.Remove(e)
		delete(c.m, k)
		return nil, false
	}
	c.ll.MoveToFront(e)
	return en, true
}

func (c *lru) put(k [sha256.Size]byte, p *Principal, expires time.Time) {
	if e, ok := c.m[k]; ok {
		c.ll.Remove(e)
	}
	c.m[k] = c.ll.PushFront(&lruEntry{key: k, p: p, expires: expires})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(*lruEntry).key)
	}
}

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
	if a.cache == nil {
		a.cache = newLRU(maxIntrospectCache)
	}
	if c, ok := a.cache.get(key, now()); ok {
		a.mu.Unlock()
		if c.p == nil {
			return nil, ErrUnauthenticated
		}
		p := *c.p
		return &p, nil
	}
	a.mu.Unlock()
	deny := func() (*Principal, error) {
		a.mu.Lock()
		a.cache.put(key, nil, now().Add(negativeTTL))
		a.mu.Unlock()
		return nil, ErrUnauthenticated
	}

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
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return deny() // the endpoint refused this token; do not ask again for a few seconds
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUnauthenticated
	}
	var out struct {
		Active   bool     `json:"active"`
		Subject  string   `json:"subject"`
		Roles    []string `json:"roles"`
		PersonID string   `json:"person_id"` // optional: the caller's directory person id, which links them to their own stakeholder record
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || !out.Active || !subjectRe.MatchString(out.Subject) {
		return deny()
	}
	if out.PersonID != "" && !personIDRe.MatchString(out.PersonID) {
		return deny()
	}
	p := Principal{Subject: out.Subject, Roles: knownRoles(out.Roles), PersonID: out.PersonID}
	ttl := a.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	a.mu.Lock()
	a.cache.put(key, &p, now().Add(ttl))
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

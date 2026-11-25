// Package fakecore is an in-memory stand-in for can-core's module API, for the in-house modules'
// tests: token introspection with audience binding, event publishing with source_id de-duplication,
// approvals with idempotency keys, and signed webhook delivery to a module's receiver. It is not a
// model of can-core's policies; the real behaviour is tested in core/server.
package fakecore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Token is a personal access token the fake knows.
type Token struct {
	Subject     string
	Permissions []string
	Module      string
	Revoked     bool
	Expires     time.Time
}

// Event is a published event.
type Event struct {
	Type     string
	Data     json.RawMessage
	SourceID string
	ID       string
}

// Approval is a request opened by a module.
type Approval struct {
	ID, Type, Requester, Ref, Title, Key, Status string
	Amount                                       int64
}

// Core is the fake.
type Core struct {
	Srv       *httptest.Server
	ModuleTok string // the projected token modules authenticate with
	ModuleID  string

	mu          sync.Mutex
	tokens      map[string]Token
	Events      []Event
	Approvals   []*Approval
	Introspects int
	Down        bool // answer 503 to everything

	// Launch tickets (specs/036-module-ui-sso).
	tickets     map[string]*Grant
	sessions    map[string]*session
	Redeems     int
	Revalidates int
	// Now is the fake's clock for launch sessions (default time.Now); set it to the module's.
	Now func() time.Time
}

// Grant is a launch ticket the fake knows: one use, for this module only.
type Grant struct {
	Subject, Name, ReturnPath string
	Permissions               []string
	Module                    string // default: the calling module
	Used                      bool
	// Binding is the launching browser's nonce; NewTicket makes one when empty.
	Binding string
	// SessionExpires is session_expires_at; zero means 12 hours from the fake's now.
	SessionExpires time.Time
	// AuthenticatedAt is authenticated_at; zero means the fake's now at NewTicket.
	AuthenticatedAt time.Time
	// NoAuthTime leaves authenticated_at out (can-core does not know).
	NoAuthTime bool
}

func (c *Core) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func withAuthTime(g Grant, m map[string]any) map[string]any {
	if !g.NoAuthTime {
		m["authenticated_at"] = g.AuthenticatedAt.UTC().Format(time.RFC3339)
	}
	return m
}

type session struct {
	grant Grant
	ended bool
}

// New starts a fake can-core serving the module API for one calling module.
func New(t *testing.T, moduleID string) *Core {
	t.Helper()
	c := &Core{ModuleTok: "projected-" + moduleID, ModuleID: moduleID, tokens: map[string]Token{}}
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c.mu.Lock()
			down := c.Down
			c.mu.Unlock()
			if down {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+c.ModuleTok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /v1/tokens/introspect", guard(c.introspect))
	mux.HandleFunc("POST /v1/tickets/peek", guard(c.peek))
	mux.HandleFunc("POST /v1/tickets/redeem", guard(c.redeem))
	mux.HandleFunc("POST /v1/tickets/revalidate", guard(c.revalidate))
	mux.HandleFunc("POST /v1/events", guard(c.publish))
	mux.HandleFunc("POST /v1/approvals", guard(c.createApproval))
	mux.HandleFunc("GET /v1/approvals/{id}", guard(c.getApproval))
	c.Srv = httptest.NewServer(mux)
	t.Cleanup(c.Srv.Close)
	return c
}

// Client returns a modclient.Client for the fake, with a token file in a temp dir.
func (c *Core) Client(t *testing.T) *modclient.Client {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(c.ModuleTok), 0o600); err != nil {
		t.Fatal(err)
	}
	return &modclient.Client{BaseURL: c.Srv.URL, TokenFile: p, ModuleID: c.ModuleID}
}

// AddToken registers a token valid for a day.
func (c *Core) AddToken(token string, t Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.Module == "" {
		t.Module = c.ModuleID
	}
	if t.Expires.IsZero() {
		t.Expires = time.Now().Add(24 * time.Hour)
	}
	c.tokens[token] = t
}

// Revoke marks a token revoked.
func (c *Core) Revoke(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tokens[token]
	t.Revoked = true
	c.tokens[token] = t
}

// SetDown makes every call fail with 503 (can-core outage).
func (c *Core) SetDown(down bool) {
	c.mu.Lock()
	c.Down = down
	c.mu.Unlock()
}

func (c *Core) introspect(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, Audience string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Introspects++
	t, ok := c.tokens[in.Token]
	if !ok || t.Revoked || time.Now().After(t.Expires) || t.Module != c.ModuleID || (in.Audience != "" && in.Audience != c.ModuleID) {
		_, _ = w.Write([]byte(`{"active":false}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "kind": "user", "subject": t.Subject, "permissions": t.Permissions, "expires_at": t.Expires.UTC().Format(time.RFC3339)})
}

// NewTicket registers a launch ticket valid for one redeem by this module and returns its value
// (the shape can-core mints).
func (c *Core) NewTicket(g Grant) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g.Module == "" {
		g.Module = c.ModuleID
	}
	if c.tickets == nil {
		c.tickets = map[string]*Grant{}
	}
	t := modclient.TicketPrefix + randSecret()
	if g.Binding == "" {
		g.Binding = modclient.BindingPrefix + randSecret()
	}
	if g.AuthenticatedAt.IsZero() {
		g.AuthenticatedAt = c.now()
	}
	c.tickets[t] = &g
	return t
}

func randSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// SetSessionPermissions changes what revalidation answers for every session of subject.
func (c *Core) SetSessionPermissions(subject string, perms []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		if s.grant.Subject == subject {
			s.grant.Permissions = perms
		}
	}
}

// EndSessions makes revalidation answer inactive for every session of subject (disabled user).
func (c *Core) EndSessions(subject string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		if s.grant.Subject == subject {
			s.ended = true
		}
	}
}

// Binding is the nonce the launching browser holds for a ticket made by NewTicket.
func (c *Core) Binding(ticket string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g, ok := c.tickets[ticket]; ok {
		return g.Binding
	}
	return ""
}

func (c *Core) peek(w http.ResponseWriter, r *http.Request) {
	var in struct{ Ticket, Binding string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.tickets[in.Ticket]
	if !ok || g.Used || g.Module != c.ModuleID || in.Binding != g.Binding {
		_, _ = w.Write([]byte(`{"active":false}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "subject": g.Subject, "name": g.Name})
}

func (c *Core) redeem(w http.ResponseWriter, r *http.Request) {
	var in struct{ Ticket, Binding string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Redeems++
	g, ok := c.tickets[in.Ticket]
	if !ok || g.Used || g.Module != c.ModuleID || in.Binding != g.Binding {
		_, _ = w.Write([]byte(`{"active":false}`))
		return
	}
	g.Used = true
	handle := modclient.HandlePrefix + randSecret()
	if c.sessions == nil {
		c.sessions = map[string]*session{}
	}
	c.sessions[handle] = &session{grant: *g}
	exp := g.SessionExpires
	if exp.IsZero() {
		exp = c.now().Add(12 * time.Hour)
	}
	_ = json.NewEncoder(w).Encode(withAuthTime(*g, map[string]any{"active": true, "subject": g.Subject, "name": g.Name, "permissions": g.Permissions,
		"return_path": g.ReturnPath, "session": handle, "session_expires_at": exp.UTC().Format(time.RFC3339)}))
}

func (c *Core) revalidate(w http.ResponseWriter, r *http.Request) {
	var in struct{ Session string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Revalidates++
	s, ok := c.sessions[in.Session]
	if !ok || s.ended || len(s.grant.Permissions) == 0 {
		_, _ = w.Write([]byte(`{"active":false}`))
		return
	}
	_ = json.NewEncoder(w).Encode(withAuthTime(s.grant, map[string]any{"active": true, "subject": s.grant.Subject, "name": s.grant.Name, "permissions": s.grant.Permissions}))
}

func (c *Core) publish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type     string          `json:"type"`
		Data     json.RawMessage `json:"data"`
		SourceID string          `json:"source_id"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Type == "" {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.Events {
		if in.SourceID != "" && e.SourceID == in.SourceID {
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintf(w, `{"id":%q}`, e.ID)
			return
		}
	}
	e := Event{Type: in.Type, Data: in.Data, SourceID: in.SourceID, ID: "evt_" + strconv.Itoa(len(c.Events)+1)}
	c.Events = append(c.Events, e)
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintf(w, `{"id":%q}`, e.ID)
}

// Published returns a copy of the events published so far.
func (c *Core) Published() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.Events...)
}

func (c *Core) createApproval(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type, Requester, Ref, Title string
		Amount                      int64
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	key := r.Header.Get("Idempotency-Key")
	c.mu.Lock()
	defer c.mu.Unlock()
	if key == "" || in.Requester == "" {
		http.Error(w, `{"detail":"requester and Idempotency-Key are required"}`, http.StatusBadRequest)
		return
	}
	for _, a := range c.Approvals {
		if a.Key == key {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": a.ID, "status": a.Status})
			return
		}
	}
	a := &Approval{ID: "apr_" + strconv.Itoa(len(c.Approvals)+1), Type: in.Type, Requester: in.Requester, Ref: in.Ref, Title: in.Title, Amount: in.Amount, Key: key, Status: "pending"}
	c.Approvals = append(c.Approvals, a)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": a.ID, "status": a.Status})
}

func (c *Core) getApproval(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.Approvals {
		if a.ID == r.PathValue("id") {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": a.ID, "status": a.Status})
			return
		}
	}
	http.Error(w, "no", http.StatusNotFound)
}

// Decide records an approval outcome (what the engine does when approvers decide) and returns the
// `approval.decided.v1` event a subscriber receives.
func (c *Core) Decide(id, outcome string) (modclient.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.Approvals {
		if a.ID == id {
			a.Status = outcome
			d, _ := json.Marshal(map[string]any{"request_id": a.ID, "module_id": c.ModuleID, "type": a.Type, "ref": a.Ref, "requester": a.Requester, "outcome": outcome})
			return modclient.Event{ID: "evt_dec_" + a.ID, Type: "approval.decided.v1", Time: time.Now(), Source: "can-core", Data: d}, true
		}
	}
	return modclient.Event{}, false
}

// Deliver POSTs an event to a module's receiver URL signed like can-core does, and returns the
// HTTP status.
func Deliver(ctx context.Context, url, secret string, ev modclient.Event) (int, error) {
	body, _ := json.Marshal(ev)
	ts := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Can-Event-Id", ev.ID)
	req.Header.Set("X-Can-Event-Type", ev.Type)
	req.Header.Set("X-Can-Timestamp", strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set("X-Can-Signature", modclient.Sign(secret, ts, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

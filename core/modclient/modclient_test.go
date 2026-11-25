package modclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/webhook"
)

type fakeCore struct {
	srv         *httptest.Server
	mu          sync.Mutex
	introspects int32
	answers     map[string]map[string]any // token -> introspection answer
	published   []map[string]any
	approvals   []map[string]any
	status      int
}

func newFakeCore(t *testing.T) (*fakeCore, *Client) {
	t.Helper()
	f := &fakeCore{answers: map[string]map[string]any{}}
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("POST /v1/tokens/introspect", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		atomic.AddInt32(&f.introspects, 1)
		var in struct{ Token, Audience string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		a, ok := f.answers[in.Token]
		if !ok {
			a = map[string]any{"active": false}
		}
		_ = json.NewEncoder(w).Encode(a)
	})
	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.published = append(f.published, in)
		st := f.status
		f.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"evt_1"}`))
	})
	mux.HandleFunc("POST /v1/approvals", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		in["idem"] = r.Header.Get("Idempotency-Key")
		f.mu.Lock()
		f.approvals = append(f.approvals, in)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"apr_1","status":"pending"}`))
	})
	mux.HandleFunc("GET /v1/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + r.PathValue("id") + `","status":"approved"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, &Client{BaseURL: f.srv.URL, TokenFile: tok, ModuleID: "work"}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if c, err := FromEnv(get); c != nil || err != nil {
		t.Fatalf("nothing configured must mean no client: %v %v", c, err)
	}
	env["CAN_API_URL"] = "http://can-core.can-system.svc.cluster.local:8090"
	env["CAN_TOKEN_FILE"] = "/var/run/secrets/can/token"
	env["CAN_MODULE_ID"] = "work"
	c, err := FromEnv(get)
	if err != nil || c == nil || c.ModuleID != "work" {
		t.Fatalf("%v %v", c, err)
	}
	for _, bad := range []string{"ftp://x", "http://", "not a url", "http://u:p@host"} {
		env["CAN_API_URL"] = bad
		if _, err := FromEnv(get); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	env["CAN_API_URL"] = "http://can-core:8090"
	delete(env, "CAN_TOKEN_FILE")
	if _, err := FromEnv(get); err == nil {
		t.Error("an API url without a token file must be an error, not a silently anonymous client")
	}
}

func TestIntrospectAndTheTokenIsReadFreshEachCall(t *testing.T) {
	f, c := newFakeCore(t)
	f.answers["canpat_x"] = map[string]any{"active": true, "kind": "user", "subject": "user:alice", "permissions": []string{"work.read"}}
	id, err := c.Introspect(context.Background(), "canpat_x")
	if err != nil || !id.Active || id.Subject != "user:alice" || id.Permissions[0] != "work.read" {
		t.Fatalf("%+v %v", id, err)
	}
	// the projected token rotates hourly: a changed file must be used on the next call
	if err := os.WriteFile(c.TokenFile, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Introspect(context.Background(), "canpat_x"); err == nil {
		t.Fatal("the old token file contents were cached")
	}
}

func TestIntrospectErrorsAreNeverAnActiveAnswer(t *testing.T) {
	f, c := newFakeCore(t)
	f.status = http.StatusBadGateway
	if id, err := c.Introspect(context.Background(), "canpat_x"); err == nil || id.Active {
		t.Fatalf("%+v %v", id, err)
	}
	f.status = http.StatusTooManyRequests
	if _, err := c.Introspect(context.Background(), "canpat_x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestIntrospectorCachesPositiveAndNegativeAnswersBoundedly(t *testing.T) {
	f, c := newFakeCore(t)
	f.answers["canpat_good"] = map[string]any{"active": true, "kind": "user", "subject": "user:alice", "permissions": []string{"work.read"}}
	now := time.Unix(1000, 0)
	in := &Introspector{API: c, TTL: 30 * time.Second, Now: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		if id, err := in.Lookup(context.Background(), "canpat_good"); err != nil || !id.Active {
			t.Fatalf("%v %v", id, err)
		}
	}
	if n := atomic.LoadInt32(&f.introspects); n != 1 {
		t.Fatalf("a valid token must be answered from the cache: %d calls", n)
	}
	now = now.Add(31 * time.Second)
	_, _ = in.Lookup(context.Background(), "canpat_good")
	if n := atomic.LoadInt32(&f.introspects); n != 2 {
		t.Fatalf("expired entry not refreshed: %d", n)
	}
	for i := 0; i < 5; i++ {
		if id, _ := in.Lookup(context.Background(), "canpat_bad"); id.Active {
			t.Fatal("unknown token active")
		}
	}
	if n := atomic.LoadInt32(&f.introspects); n != 3 {
		t.Fatalf("a bad token replayed must not reach can-core each time: %d", n)
	}
	// the cache is bounded
	small := &Introspector{API: c, TTL: time.Minute, MaxEntries: 8, Now: func() time.Time { return now }}
	for i := 0; i < 100; i++ {
		_, _ = small.Lookup(context.Background(), "canpat_t"+strings.Repeat("x", i%7)+string(rune('a'+i%26)))
	}
	if small.Len() > 8 {
		t.Fatalf("cache grew to %d", small.Len())
	}
}

func TestIntrospectorDoesNotCacheErrorsAndNeverTrustsAnExpiredAnswer(t *testing.T) {
	f, c := newFakeCore(t)
	f.answers["canpat_good"] = map[string]any{"active": true, "kind": "user", "subject": "user:alice", "permissions": []string{"work.read"}, "expires_at": "1970-01-01T00:16:50Z"} // 1010 s
	now := time.Unix(1000, 0)
	in := &Introspector{API: c, TTL: time.Hour, Now: func() time.Time { return now }}
	f.status = http.StatusBadGateway
	if _, err := in.Lookup(context.Background(), "canpat_good"); err == nil {
		t.Fatal("expected an error")
	}
	f.status = 0
	if id, err := in.Lookup(context.Background(), "canpat_good"); err != nil || !id.Active {
		t.Fatalf("an earlier failure was cached: %v %v", id, err)
	}
	now = time.Unix(1011, 0) // past the token's own expiry, well inside the cache TTL
	f.answers["canpat_good"] = map[string]any{"active": false}
	if id, _ := in.Lookup(context.Background(), "canpat_good"); id.Active {
		t.Fatal("an entry outlived the token's expiry")
	}
}

func TestIntrospectorCollapsesConcurrentLookupsOfOneToken(t *testing.T) {
	f, c := newFakeCore(t)
	f.answers["canpat_good"] = map[string]any{"active": true, "kind": "user", "subject": "user:alice", "permissions": []string{"work.read"}}
	in := &Introspector{API: c, TTL: time.Minute}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := in.Lookup(context.Background(), "canpat_good"); err != nil || !id.Active {
				t.Errorf("%v %v", id, err)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&f.introspects); n > 5 {
		t.Fatalf("%d calls for 50 concurrent lookups of one token", n)
	}
}

func TestOnlyTokensOfTheExpectedShapeAreAsked(t *testing.T) {
	f, c := newFakeCore(t)
	in := &Introspector{API: c}
	for _, tok := range []string{"", "x", "Bearer", strings.Repeat("a", 10_000), "has space", "canpat_a\nb"} {
		if id, err := in.Lookup(context.Background(), tok); id.Active || err != nil {
			t.Errorf("%q: %v %v", tok, id, err)
		}
	}
	if n := atomic.LoadInt32(&f.introspects); n != 0 {
		t.Fatalf("garbage reached can-core %d times", n)
	}
}

func TestPublishEnvelope(t *testing.T) {
	f, c := newFakeCore(t)
	body := []byte(`{"id":"evt_abc","type":"work.issue.created.v1","time":"2026-01-01T00:00:00Z","source":"work","data":{"issue_id":"W-1"}}`)
	if err := c.PublishEnvelope(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	got := f.published[0]
	if got["type"] != "work.issue.created.v1" || got["source_id"] != "evt_abc" || got["data"].(map[string]any)["issue_id"] != "W-1" {
		t.Fatalf("%v", got)
	}
	if _, has := got["source"]; has {
		t.Fatal("the source is the authenticated module, never client supplied")
	}
	if err := c.PublishEnvelope(context.Background(), []byte(`{"type":"bad"`)); err == nil {
		t.Fatal("malformed envelope accepted")
	}
	f.status = http.StatusForbidden
	if err := c.PublishEnvelope(context.Background(), body); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("a refused publish is permanent, not retryable-as-unavailable: %v", err)
	}
	f.status = http.StatusBadGateway
	if err := c.PublishEnvelope(context.Background(), body); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestApprovalsClient(t *testing.T) {
	f, c := newFakeCore(t)
	a, err := c.CreateApproval(context.Background(), ApprovalInput{Type: "invoice", Requester: "user:alice", Ref: "invoice:7", Title: "Invoice 7", Amount: 1200, Key: "invoice:7:1"})
	if err != nil || a.ID != "apr_1" || a.Status != "pending" {
		t.Fatalf("%+v %v", a, err)
	}
	if f.approvals[0]["idem"] != "invoice:7:1" || f.approvals[0]["requester"] != "user:alice" {
		t.Fatalf("%v", f.approvals[0])
	}
	if got, err := c.GetApproval(context.Background(), "apr_1"); err != nil || got.Status != "approved" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := c.CreateApproval(context.Background(), ApprovalInput{Type: "invoice", Requester: "user:alice"}); err == nil {
		t.Fatal("an approval without an idempotency key must be refused locally")
	}
}

func signed(secret string, ts time.Time, body []byte, id, typ string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/can/events", strings.NewReader(string(body)))
	r.Header.Set(webhook.HeaderEventID, id)
	r.Header.Set(webhook.HeaderEventType, typ)
	r.Header.Set(webhook.HeaderTimestamp, itoa(ts.Unix()))
	r.Header.Set(webhook.HeaderSignature, webhook.Sign(secret, ts, body))
	return r
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestWebhookHandlerVerifiesTheSignatureAndTheReplayWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var got []Event
	h := &Receiver{Secret: "s3cret-s3cret-s3cret", Now: func() time.Time { return now }, Handle: func(_ context.Context, e Event) error { got = append(got, e); return nil }}
	body := []byte(`{"id":"evt_1","type":"candidate.hired.v1","time":"2026-01-01T00:00:00Z","source":"jobs","data":{"application_id":"a1"}}`)

	do := func(r *http.Request) int { w := httptest.NewRecorder(); h.ServeHTTP(w, r); return w.Code }
	if c := do(signed("s3cret-s3cret-s3cret", now, body, "evt_1", "candidate.hired.v1")); c != 204 || len(got) != 1 || got[0].Type != "candidate.hired.v1" || string(got[0].Data) != `{"application_id":"a1"}` {
		t.Fatalf("%d %+v", c, got)
	}
	if c := do(signed("wrong-secret-wrong-secret", now, body, "evt_1", "candidate.hired.v1")); c != 401 {
		t.Fatalf("wrong signature %d", c)
	}
	if c := do(signed("s3cret-s3cret-s3cret", now.Add(-10*time.Minute), body, "evt_1", "candidate.hired.v1")); c != 401 {
		t.Fatalf("stale %d", c)
	}
	bad := signed("s3cret-s3cret-s3cret", now, body, "evt_1", "candidate.hired.v1")
	bad.Header.Del(webhook.HeaderSignature)
	if c := do(bad); c != 401 {
		t.Fatalf("unsigned %d", c)
	}
	tampered := signed("s3cret-s3cret-s3cret", now, body, "evt_1", "candidate.hired.v1")
	tampered.Body = io.NopCloser(strings.NewReader(strings.Replace(string(body), "a1", "a2", 1)))
	if c := do(tampered); c != 401 {
		t.Fatalf("tampered body %d", c)
	}
	if len(got) != 1 {
		t.Fatal("a rejected request reached the handler")
	}
	get := httptest.NewRequest("GET", "/v1/can/events", nil)
	if c := do(get); c != 405 {
		t.Fatalf("GET %d", c)
	}
}

func TestWebhookReceiverFailsClosedWithoutASecretAndDeduplicates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"id":"evt_9","type":"approval.decided.v1","time":"2026-01-01T00:00:00Z","source":"can-core","data":{}}`)
	h := &Receiver{Secret: "", Now: func() time.Time { return now }, Handle: func(context.Context, Event) error { t.Fatal("handled"); return nil }}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signed("", now, body, "evt_9", "approval.decided.v1"))
	if w.Code != 401 && w.Code != 503 {
		t.Fatalf("a receiver with no secret must refuse everything, got %d", w.Code)
	}

	calls := 0
	h = &Receiver{Secret: "s3cret-s3cret-s3cret", Now: func() time.Time { return now }, Handle: func(context.Context, Event) error { calls++; return nil }}
	for i := 0; i < 3; i++ {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, signed("s3cret-s3cret-s3cret", now, body, "evt_9", "approval.decided.v1"))
		if w.Code != 204 {
			t.Fatalf("%d", w.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("a redelivered event was handled %d times", calls)
	}
	// a failing handler is retried by can-core: 500, and the id is not remembered
	fails := 0
	h = &Receiver{Secret: "s3cret-s3cret-s3cret", Now: func() time.Time { return now }, Handle: func(context.Context, Event) error { fails++; return errors.New("db down") }}
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, signed("s3cret-s3cret-s3cret", now, body, "evt_9", "approval.decided.v1"))
		if w.Code != 500 {
			t.Fatalf("%d", w.Code)
		}
	}
	if fails != 2 {
		t.Fatalf("%d", fails)
	}
}

func TestWebhookBodyIsBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	h := &Receiver{Secret: "s3cret-s3cret-s3cret", Now: func() time.Time { return now }, Handle: func(context.Context, Event) error { return nil }}
	big := []byte(`{"id":"e","type":"a.b.v1","data":{"x":"` + strings.Repeat("a", 2<<20) + `"}}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signed("s3cret-s3cret-s3cret", now, big, "e", "a.b.v1"))
	if w.Code != 413 && w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
}

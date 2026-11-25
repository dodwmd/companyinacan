package finance

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	adminTok = "test-admin-token-0123456789abcdefXYZ"
	linkSec  = "test-link-secret-0123456789abcdefXYZ!"
	provSec  = "test-provider-secret-0123456789"
	webSec   = "test-webhook-secret-0123456789"
)

type env struct {
	t   *testing.T
	app *App
	h   http.Handler
	dir string
	mu  sync.Mutex
	now time.Time
	pay *FakePayments
	sig *FakeESign
}

type envOpt func(*Config, *Options)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), now: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(e.dir, "f.db")
	cfg.Webhook.URL = "http://127.0.0.1:9/never" // events are queued; the dispatcher only runs in Serve
	cfg.PaymentInstructions = "Pay to IBAN XX00 0000 0000 (fake)."
	cfg.Seller.Name, cfg.Seller.Address = "Example Seller Pty", "1 Fake Street"
	cfg.PaymentProvider, cfg.ESignProvider = "fake", "fake"
	cfg.PublicBaseURL = "https://billing.example.test"
	opt := Options{Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }}
	for _, o := range opts {
		o(cfg, &opt)
	}
	app, err := NewApp(cfg, Secrets{AdminToken: adminTok, LinkSecret: linkSec, WebhookSecret: webSec, ProviderSecret: provSec}, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	e.app, e.h = app, app.Handler()
	e.pay = &FakePayments{Secret: []byte(provSec), Now: opt.Now}
	e.sig = &FakeESign{Secret: []byte(provSec), Now: opt.Now}
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

func (e *env) req(tok, method, path string, body any, hdr ...string) (int, http.Header, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, rd)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w.Code, w.Header(), w.Body.Bytes()
}

// do returns the status and decodes a JSON body into out when out is not nil.
func (e *env) do(tok, method, path string, body any, out any, hdr ...string) int {
	e.t.Helper()
	code, _, b := e.req(tok, method, path, body, hdr...)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return code
}

func (e *env) must(want int, tok, method, path string, body any, out any, hdr ...string) {
	e.t.Helper()
	code, _, b := e.req(tok, method, path, body, hdr...)
	if code != want {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, path, code, want, b)
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
}

// token mints a stored token through the API and returns its secret.
func (e *env) token(subject string, scopes []string, parties []int64, projects []string, own bool) string {
	e.t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	e.must(201, adminTok, "POST", "/v1/tokens", map[string]any{"kind": "service", "subject": subject, "name": subject, "scopes": scopes, "parties": parties, "projects": projects, "own_only": own}, &out)
	return out.Token
}

type obj = map[string]any

func (e *env) party(tok, name, kind string) int64 {
	e.t.Helper()
	var p Party
	e.must(201, tok, "POST", "/v1/parties", obj{"name": name, "kind": kind, "email": emailOf(name), "address": "1 Fake Road"}, &p)
	return p.ID
}

func line(desc string, qtyMilli, unit int64) obj {
	return obj{"description": desc, "quantity_milli": qtyMilli, "unit_price_minor": unit}
}

func (e *env) invoice(tok string, party int64, extra obj, lines ...obj) *Invoice {
	e.t.Helper()
	body := obj{"party_id": party, "lines": lines}
	for k, v := range extra {
		body[k] = v
	}
	var v Invoice
	e.must(201, tok, "POST", "/v1/invoices", body, &v)
	return &v
}

func id(n int64) string { return itoaID(n) }

// outbox returns the queued events of a type.
func (e *env) outbox(typ string) []map[string]any {
	e.t.Helper()
	rows, err := e.app.store.db.Query(`SELECT payload FROM outbox WHERE type = ? ORDER BY id`, typ)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		var m map[string]any
		_ = json.Unmarshal([]byte(p), &m)
		out = append(out, m)
	}
	return out
}

// roles used across tests
var (
	rw  = []string{"finance.write"}
	app = []string{"finance.approve"}
)

func emailOf(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' && b.Len() > 0 && !strings.HasSuffix(b.String(), "."):
			b.WriteByte('.')
		}
	}
	return strings.Trim(b.String(), ".") + "@example.test"
}

package forms

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const harnessCfg = `
rate_limit: {per_minute: 5}
captcha: {provider: turnstile}
cors_allowed_origins: ["https://www.example.com"]
forms:
  - id: contact
    name: Contact
    redirect: /thanks/
    notify_email: [owner@example.com]
    fields:
      - {name: name, type: text, required: true, max_length: 100}
      - {name: email, type: email, required: true}
      - {name: message, type: text, required: true, max_length: 2000, multiline: true}
  - id: quote
    name: Quote
    min_fill_seconds: 0
    captcha: true
    fields:
      - {name: email, type: email, required: true}
`

const (
	adminToken = "admin-token-0123456789"
	readToken  = "read-token-0123456789"
)

type fakeMailer struct {
	mu   sync.Mutex
	sent []Email
	err  error
}

func (m *fakeMailer) Send(_ context.Context, e Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, e)
	return nil
}

func (m *fakeMailer) all() []Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Email(nil), m.sent...)
}

type fakeCaptcha struct {
	ok  bool
	err error
	got string
}

func (c *fakeCaptcha) Verify(_ context.Context, response, _ string) (bool, error) {
	c.got = response
	return c.ok, c.err
}

type harness struct {
	t       *testing.T
	app     *App
	pub     http.Handler
	api     http.Handler
	clock   *clock
	mailer  *fakeMailer
	captcha *fakeCaptcha
}

func newHarness(t *testing.T, cfgYAML string) *harness {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = t.TempDir() + "/forms.db"
	h := &harness{t: t, clock: newClock(), mailer: &fakeMailer{}, captcha: &fakeCaptcha{ok: true}}
	app, err := NewApp(cfg, Secrets{
		TokenKey:      []byte("0123456789abcdef0123456789abcdef"),
		APIToken:      adminToken,
		APIReadToken:  readToken,
		CaptchaSecret: "cs",
	}, Options{
		Mailer:  h.mailer,
		Captcha: h.captcha,
		Now:     h.clock.now,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Close(ctx)
	})
	h.app, h.pub, h.api = app, app.PublicHandler(), app.APIHandler()
	return h
}

// token fetches a fresh timing token and advances the clock past the minimum fill time.
func (h *harness) token(form string) string {
	h.t.Helper()
	rec := h.do(h.pub, http.MethodGet, "/_forms/"+form+"/token", nil, nil)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	tok := jsonField(h.t, rec.Body.String(), "token")
	h.clock.advance(5 * time.Second)
	return tok
}

func (h *harness) do(handler http.Handler, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "203.0.113.7:4321"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}

func (h *harness) postForm(path string, v url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	m := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for k, val := range hdr {
		m[k] = val
	}
	return h.do(h.pub, http.MethodPost, path, strings.NewReader(v.Encode()), m)
}

func (h *harness) apiDo(method, path, token string, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	hdr := map[string]string{}
	if token != "" {
		hdr["Authorization"] = "Bearer " + token
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
		hdr["Content-Type"] = "application/json"
	}
	return h.do(h.api, method, path, rd, hdr)
}

func goodValues(tok string) url.Values {
	return url.Values{"name": {"Ada"}, "email": {"ada@example.com"}, "message": {"Hello there"}, "_t": {tok}}
}

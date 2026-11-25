package adminui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func init() { auth.TestingParams() }

var fixedNow = time.Unix(1_700_000_000, 0).UTC()

type fakeLeads struct {
	leads    []Lead
	err      error
	listQ    []LeadQuery
	statuses map[string]string
}

func (f *fakeLeads) List(_ context.Context, q LeadQuery) (LeadPage, error) {
	f.listQ = append(f.listQ, q)
	if f.err != nil {
		return LeadPage{}, f.err
	}
	var out []Lead
	for _, l := range f.leads {
		if q.Status == "" || l.Status == q.Status {
			out = append(out, l)
		}
	}
	return LeadPage{Items: out, NextCursor: "42"}, nil
}

func (f *fakeLeads) Get(_ context.Context, id string) (Lead, error) {
	if f.err != nil {
		return Lead{}, f.err
	}
	for _, l := range f.leads {
		if l.ID == id {
			return l, nil
		}
	}
	return Lead{}, ErrLeadNotFound
}

func (f *fakeLeads) SetStatus(_ context.Context, id, status string) error {
	if f.err != nil {
		return f.err
	}
	found := false
	for _, l := range f.leads {
		found = found || l.ID == id
	}
	if !found {
		return ErrLeadNotFound
	}
	if f.statuses == nil {
		f.statuses = map[string]string{}
	}
	f.statuses[id] = status
	return nil
}

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	db     *store.DB
	audit  *audit.Log
	leads  *fakeLeads
	secret string
	pw     string
	now    *time.Time
	logs   *bytes.Buffer
	ui     *Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := fixedNow
	db.Now = func() time.Time { return now }
	h := &harness{t: t, db: db, pw: "correct horse battery staple", secret: auth.NewTOTPSecret(), now: &now, logs: &bytes.Buffer{}, leads: &fakeLeads{}}
	hash, _ := auth.HashPassword(h.pw)
	if _, err := db.CreateAdmin(ctx, "root", hash, h.secret); err != nil {
		t.Fatal(err)
	}
	h.audit = audit.New(db)
	clock := func() time.Time { return now }
	h.ui = &Server{
		Auth: auth.NewService(db, h.audit, auth.Options{Now: clock, MinLoginTime: time.Nanosecond, Sleep: func(context.Context, time.Duration) error { return nil },
			Policy: auth.Policy{HardLock: true, MaxUserFailures: 5, MaxIPFailures: 20, AttemptsPerMin: 1000, Window: 15 * time.Minute, LockFor: 15 * time.Minute}}),
		DB: db, Audit: h.audit, Registry: registry.New(db, "http://can-core"), Leads: h.leads,
		Company: Company{Name: "Acme Pty Ltd", Domain: "acme.example"},
		Log:     slog.New(slog.NewJSONHandler(h.logs, nil)), Secure: true, Now: clock,
	}
	h.srv = httptest.NewServer(h.ui.Handler(nil))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) code() string {
	key, _ := auth.DecodeTOTPSecret(h.secret)
	return auth.Code(key, auth.SHA1, 6, 30, *h.now)
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type resp struct {
	*http.Response
	body string
}

func (h *harness) do(method, path string, form url.Values, cookies ...*http.Cookie) resp {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	r, err := noRedirect.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	return resp{r, string(b)}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]*)"`)

func csrfOf(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf field in page:\n%s", body)
	}
	return m[1]
}

func cookieNamed(r *http.Response, name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// loginForm fetches the login page and returns its CSRF cookie and field.
func (h *harness) loginForm() (*http.Cookie, string) {
	h.t.Helper()
	r := h.do("GET", "/login", nil)
	c := cookieNamed(r.Response, "can_login_csrf")
	if c == nil {
		h.t.Fatal("no login csrf cookie")
	}
	return c, csrfOf(h.t, r.body)
}

type session struct {
	cookie *http.Cookie
	csrf   string
}

func (s session) with() []*http.Cookie { return []*http.Cookie{s.cookie} }

func (h *harness) login() session {
	h.t.Helper()
	c, tok := h.loginForm()
	r := h.do("POST", "/login", url.Values{"username": {"root"}, "password": {h.pw}, "totp": {h.code()}, "csrf": {tok}}, c)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		h.t.Fatalf("login: %d %s %s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	sc := cookieNamed(r.Response, "can_session")
	if sc == nil {
		h.t.Fatal("no session cookie")
	}
	s := session{cookie: &http.Cookie{Name: sc.Name, Value: sc.Value}}
	s.csrf = csrfOf(h.t, h.do("GET", "/settings", nil, s.cookie).body)
	return s
}

// seed installs fixture modules, capabilities, deliveries and audit entries.
func (h *harness) seed() {
	h.t.Helper()
	ctx := context.Background()
	var forms config.Module
	forms.ID, forms.Name, forms.Version, forms.Description = "forms", "Forms gateway", "1.2.0", "Lead capture."
	forms.Provider.Name, forms.Provider.Licence, forms.Provider.LicenceClass, forms.Provider.Type = "Company in a Can", "Apache-2.0", "osi", "in-house"
	forms.Resources.RAMMB = 32
	forms.Provides = []struct {
		Capability string `yaml:"capability"`
		Schema     string `yaml:"schema"`
		Service    string `yaml:"service"`
		Port       int    `yaml:"port"`
	}{{Capability: "leads", Schema: "leads.v1", Service: "forms-api", Port: 8081}}
	forms.Permissions = []string{"leads.read", "leads.write"}
	forms.Consumes = []string{"email.send"}

	var wiki config.Module
	wiki.ID, wiki.Name, wiki.Version = "wiki", "Wiki", "0.4.1"
	wiki.Provider.Name, wiki.Provider.Licence, wiki.Provider.LicenceClass, wiki.Provider.Upstream = "Upstream Wiki", "BUSL-1.1", "source-available", "https://example.com/wiki"
	wiki.Resources.RAMMB = 256
	wiki.Requires.Database = "postgres"

	prov := []store.Provider{{Capability: "leads", ModuleID: "forms", Service: "forms-api", Port: 8081, Schema: "leads.v1"}}
	must(h.t, h.db.UpsertModule(ctx, store.Module{ID: "forms", Version: "1.2.0", Namespace: "mod-forms", ServiceAccount: "forms", State: store.StateEnabled, HMACSecret: "s3cret", Manifest: forms}, prov, nil))
	must(h.t, h.db.UpsertModule(ctx, store.Module{ID: "wiki", Version: "0.4.1", Namespace: "mod-wiki", ServiceAccount: "wiki", State: store.StateFailed, LastError: "helm install failed: timeout", HMACSecret: "x", Manifest: wiki}, nil, nil))
	must(h.t, h.db.InsertEvent(ctx, store.Event{ID: "evt-1", Type: "lead.created.v1", Time: fixedNow, Source: "forms", DataJSON: `{"email":"private@example.com"}`}, []store.DeliveryTarget{{ModuleID: "forms", URL: "http://x"}}))
	_, err := h.audit.Append(ctx, audit.Entry{Actor: "cli", Action: "module.enable", Target: "forms"})
	must(h.t, err)
}

// killDelivery marks the first delivery dead after one recorded attempt.
func (h *harness) killDelivery() int64 {
	h.t.Helper()
	ctx := context.Background()
	ds, _ := h.db.ListDeliveries(ctx, 1)
	id := ds[0].ID
	must(h.t, h.db.RecordAttempt(ctx, id, store.Attempt{At: fixedNow, StatusCode: 502, Error: "bad gateway", Duration: 12 * time.Millisecond}, "dead", fixedNow))
	return id
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

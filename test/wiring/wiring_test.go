// Package wiring tests the in-house modules and can-core's real platform services together
// (specs/016-module-wiring): modules authenticate personal access tokens through can-core's module
// API, publish events through it, and receive signed webhooks and approval decisions from it. The
// cluster, the network and Kubernetes' TokenReview are faked; everything else is the real code.
package wiring

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

const root = "../.."

type world struct {
	t     *testing.T
	ctx   context.Context
	db    *store.DB
	reg   *registry.Registry
	rb    *rbac.Service
	ntf   *notify.Service
	eng   *approvals.Engine
	pats  *pat.Service
	core  *httptest.Server
	tr    *tokenreview.Fake
	disp  *webhook.Dispatcher
	hosts map[string]string // in-cluster service host -> test server URL
	hmac  map[string]string
	dir   string
}

type rewrite struct{ w *world }

func (r rewrite) Do(req *http.Request) (*http.Response, error) {
	target, ok := r.w.hosts[req.URL.Host]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	u := *req.URL
	t, _ := http.NewRequest(req.Method, target+u.Path, req.Body)
	t.Header = req.Header
	return http.DefaultClient.Do(t.WithContext(req.Context()))
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := &world{t: t, ctx: ctx, db: db, hosts: map[string]string{}, hmac: map[string]string{}, dir: t.TempDir(), tr: tokenreview.NewFake()}
	al := audit.New(db)
	w.reg = registry.New(db, "http://can-core.can-system.svc:8090")
	bus := events.NewBus(db)
	w.rb = rbac.New(db, al, rbac.Options{BreakGlass: true})
	w.ntf = notify.New(db, notify.Options{Bus: bus, Roles: w.rb, Log: log})
	bus.SubscribeAll(w.ntf.HandleEvent)
	w.eng = approvals.New(db, al, approvals.Deps{Roles: w.rb, Perms: w.rb, Notifier: w.ntf, Bus: bus, Log: log})
	w.pats = pat.New(db, al)
	w.pats.Holds = func(ctx context.Context, s, p string) (bool, error) { return w.rb.Check(ctx, s, p, "") }
	srv := &moduleapi.Server{Auth: modauth.New(w.tr, db), Registry: w.reg, Bus: bus, Log: log, RBAC: w.rb, Approvals: w.eng, Notify: w.ntf, PATs: w.pats}
	w.core = httptest.NewServer(srv.Handler())
	t.Cleanup(w.core.Close)
	w.disp = webhook.NewDispatcher(db, rewrite{w})
	w.disp.Log = log
	return w
}

// register installs a module from its real manifest and returns a can-core client for it.
func (w *world) register(id string) (*config.Module, *modclient.Client, string) {
	w.t.Helper()
	doc, ps := config.Load(filepath.Join(root, "modules", id, "module.yaml"))
	if doc == nil {
		w.t.Fatal(ps)
	}
	man, err := config.DecodeModule(doc)
	if err != nil {
		w.t.Fatal(err)
	}
	secret := "hmac-secret-for-" + id + "-0123456789abcdef"
	w.hmac[id] = secret
	if err := w.reg.Register(w.ctx, store.Module{ID: id, Version: man.Version, Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: secret, Manifest: *man}); err != nil {
		w.t.Fatal(err)
	}
	w.tr.Add("projected-"+id, "mod-"+id, id)
	tok := filepath.Join(w.dir, id+".token")
	if err := os.WriteFile(tok, []byte("projected-"+id), 0o600); err != nil {
		w.t.Fatal(err)
	}
	return man, &modclient.Client{BaseURL: w.core.URL, TokenFile: tok, ModuleID: id}, secret
}

// serve publishes a module's handler in the cluster under the host its subscription names.
func (w *world) serve(man *config.Module, h http.Handler) *httptest.Server {
	w.t.Helper()
	s := httptest.NewServer(h)
	w.t.Cleanup(s.Close)
	for _, sub := range man.Subscribes {
		w.hosts[sub.Service+".mod-"+man.ID+".svc.cluster.local:"+itoa(sub.Port)] = s.URL
	}
	return s
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// pump delivers every pending webhook of can-core to its subscribers.
func (w *world) pump() {
	w.t.Helper()
	for i := 0; i < 5; i++ {
		n, err := w.disp.RunOnce(w.ctx)
		if err != nil {
			w.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (w *world) token(user, module string, perms ...string) string {
	w.t.Helper()
	if _, err := w.db.AdminByUsername(w.ctx, user); err != nil {
		if _, err := w.db.CreateAdmin(w.ctx, user, "x", "y"); err != nil {
			w.t.Fatal(err)
		}
	}
	iss, err := w.pats.Issue(w.ctx, "user:"+user, pat.IssueRequest{Name: user + " in tests", Module: module, Permissions: perms, TTL: time.Hour})
	if err != nil {
		w.t.Fatal(err)
	}
	return iss.Token
}

func (w *world) grant(user, role string) {
	w.t.Helper()
	if err := w.rb.Grant(w.ctx, rbac.CLIActor, "user:"+user, role, ""); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) eventsOf(typ string) []string {
	w.t.Helper()
	rows, err := w.db.Query(`SELECT data_json FROM events WHERE type = ? ORDER BY time, id`, typ)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (w *world) noPersonalDataInCanCore(forbidden ...string) {
	w.t.Helper()
	rows, err := w.db.Query(`SELECT type, data_json FROM events`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var typ, data string
		_ = rows.Scan(&typ, &data)
		for _, f := range forbidden {
			if strings.Contains(data, f) {
				w.t.Errorf("can-core kept personal data %q in %s: %s", f, typ, data)
			}
		}
	}
}

func call(t *testing.T, h http.Handler, tok, method, path string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "192.0.2.10:1234"
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func mustCall(t *testing.T, h http.Handler, want int, tok, method, path string, body any, hdr ...string) map[string]any {
	t.Helper()
	code, m := call(t, h, tok, method, path, body, hdr...)
	if code != want {
		t.Fatalf("%s %s = %d, want %d: %v", method, path, code, want, m)
	}
	return m
}

// ---- module constructors ------------------------------------------------------------------------

func newPeople(t *testing.T, w *world) (*people.Service, *people.Dispatcher, http.Handler) {
	t.Helper()
	man, can, secret := w.register("people")
	kr, err := people.ParseKeyring("k1 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := people.Open(filepath.Join(t.TempDir(), "people.db"), people.Options{Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	st := &people.StaticTokens{}
	st.Add("svc-token-0123456789abcdefghij", people.Principal{Subject: "svc-can-core", Roles: []string{people.RoleHR, people.RoleAdmin}})
	api := &people.API{Svc: svc, Auth: people.MultiAuth{st, &people.CanAuth{In: &modclient.Introspector{API: can}}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Can: &modclient.Receiver{Secret: secret, Handle: svc.CanEventHandler()}}
	h := api.Handler()
	w.serve(man, h)
	d := people.NewDispatcher("", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Can = can
	return svc, d, h
}

type clock struct{ off time.Duration }

func (c *clock) Now() time.Time          { return time.Now().Add(c.off) }
func (c *clock) Advance(d time.Duration) { c.off += d }

func newJobs(t *testing.T, w *world, clk *clock) (*jobs.App, http.Handler) {
	t.Helper()
	man, can, secret := w.register("jobs")
	cfg := jobs.DefaultConfig()
	dir := t.TempDir()
	cfg.DBPath, cfg.ResumeDir = filepath.Join(dir, "jobs.db"), filepath.Join(dir, "resumes")
	cfg.Company.Name, cfg.Company.BaseURL = "Example Co", "https://example.test"
	cfg.RateLimit.ApplyPerHour = 1000
	cfg.Apply.ScannerRequired = false
	kr, err := jobs.ParseKeyring("k1 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n")
	if err != nil {
		t.Fatal(err)
	}
	app, err := jobs.NewApp(cfg, jobs.Secrets{AdminToken: "admin-token-0123456789abcdefABCDEF-xyz", HashKey: "hash-key-0123456789abcdef0123456789abcdef", FormSecret: "form-secret-0123456789abcdef", CanSecret: secret},
		jobs.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Keyring: kr, Can: can, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	w.serve(man, app.Handler())
	return app, app.Handler()
}

func newFinance(t *testing.T, w *world) (*finance.App, http.Handler) {
	t.Helper()
	man, can, secret := w.register("finance")
	cfg := finance.DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "finance.db")
	app, err := finance.NewApp(cfg, finance.Secrets{AdminToken: "admin-token-0123456789abcdefABCDEF-xyz", LinkSecret: "link-secret-0123456789abcdef0123456789abcdef"},
		finance.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Can: can, CanSecret: secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	w.serve(man, app.Handler())
	return app, app.Handler()
}

// ---- scenarios -----------------------------------------------------------------------------------

// lead.created (forms) -> can-core -> notification rule of the forms manifest -> the sales inbox.
func TestLeadCreatedNotifiesTheSalesRole(t *testing.T) {
	w := newWorld(t)
	_, can, _ := w.register("forms")
	cfg, err := forms.ParseConfig([]byte("forms:\n  - id: contact\n    name: Contact\n    min_fill_seconds: 0\n    fields:\n      - {name: name, type: text, required: true, max_length: 100}\n      - {name: email, type: email, required: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBPath = filepath.Join(t.TempDir(), "forms.db")
	app, err := forms.NewApp(cfg, forms.Secrets{TokenKey: []byte("0123456789abcdef0123456789abcdef"), APIToken: "admin-token-0123456789"}, forms.Options{Can: can, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	w.grant("dave", "forms:sales")

	r := httptest.NewRecorder()
	app.PublicHandler().ServeHTTP(r, httptest.NewRequest("GET", "/_forms/contact/token", nil))
	var tk struct{ Token string }
	_ = json.Unmarshal(r.Body.Bytes(), &tk)
	form := strings.NewReader("name=Ada+Lovelace&email=ada%40example.com&_t=" + tk.Token)
	req := httptest.NewRequest("POST", "/_forms/contact", form)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = httptest.NewRecorder()
	app.PublicHandler().ServeHTTP(r, req)
	if r.Code >= 400 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = app.Close(ctx) // drains the dispatcher: the event has been published

	items, err := w.ntf.List(w.ctx, "user:dave", notify.ListOptions{})
	if err != nil || len(items) != 1 || items[0].Type != "lead.created" {
		t.Fatalf("sales inbox: %+v %v", items, err)
	}
	if other, _ := w.ntf.List(w.ctx, "user:erin", notify.ListOptions{}); len(other) != 0 {
		t.Fatal("someone without the sales role was notified")
	}
	w.noPersonalDataInCanCore("Ada", "ada@example.com")
	if b, _ := json.Marshal(items); strings.Contains(string(b), "Ada") || strings.Contains(string(b), "example.com") {
		t.Fatalf("the notification copies personal data: %s", b)
	}
}

// candidate.hired (jobs) -> can-core -> signed webhook -> people creates the person and the checklist.
func TestHireHandOffStartsOnboardingInPeople(t *testing.T) {
	w := newWorld(t)
	clk := &clock{}
	svc, _, _ := newPeople(t, w)
	app, jh := newJobs(t, w, clk)
	w.grant("alice", "jobs:recruiter")
	rec := w.token("alice", "jobs", "jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview")

	job := mustCall(t, jh, 201, rec, "POST", "/v1/jobs", map[string]any{
		"title": "Engineer", "department": "Engineering", "location": map[string]any{"city": "Austin", "region": "TX", "country": "US"}, "remote": "hybrid",
		"employment_type": "full_time", "hiring_manager": "user:bob", "headcount": 1,
		"sections": []map[string]string{{"heading": "About", "body": "Do good work."}},
	})
	id, slug := job["id"].(string), job["slug"].(string)
	mustCall(t, jh, 200, rec, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"})

	// the candidate applies on the public careers page
	page := httptest.NewRecorder()
	jh.ServeHTTP(page, httptest.NewRequest("GET", "/careers/"+slug, nil))
	m := regexp.MustCompile(`name="_t" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if m == nil {
		t.Fatalf("no timing token: %s", page.Body)
	}
	clk.Advance(10 * time.Second)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"_t": m[1], "name": "Ada Example", "email": "ada@example.com", "phone": "+1 555 0100", "cover_letter": "Hello", "consent": "on"} {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	ar := httptest.NewRequest("POST", "/careers/"+slug+"/apply", &buf)
	ar.RemoteAddr = "198.51.100.7:5000"
	ar.Header.Set("Content-Type", mw.FormDataContentType())
	ap := httptest.NewRecorder()
	jh.ServeHTTP(ap, ar)
	if ap.Code != 303 {
		t.Fatalf("apply = %d %s", ap.Code, ap.Body)
	}
	apps := mustCall(t, jh, 200, rec, "GET", "/v1/applications?job_id="+id, nil)
	appID := apps["items"].([]any)[0].(map[string]any)["id"].(string)
	mustCall(t, jh, 200, rec, "POST", "/v1/applications/"+appID+"/move", map[string]any{"stage": "offer"})
	mustCall(t, jh, 200, rec, "POST", "/v1/applications/"+appID+"/hire", nil)

	if _, err := app.DeliverEvents(w.ctx); err != nil { // jobs publishes through can-core
		t.Fatal(err)
	}
	w.pump() // can-core delivers to its subscribers

	hr := people.Actor{Subject: "check", Roles: []string{people.RoleHR}}
	list, _, err := svc.ListPeople(w.ctx, hr, people.PersonFilter{Status: people.StOnboarding})
	if err != nil || len(list) != 1 {
		t.Fatalf("people in onboarding: %d %v", len(list), err)
	}
	cls, err := svc.ListChecklists(w.ctx, hr, list[0].ID)
	if err != nil || len(cls) != 1 || len(cls[0].Tasks) == 0 {
		t.Fatalf("checklist: %+v %v", cls, err)
	}
	if got := w.eventsOf("candidate.hired.v1"); len(got) != 1 {
		t.Fatalf("%d candidate.hired events in can-core", len(got))
	}
	w.noPersonalDataInCanCore("Ada", "ada@example.com", "555")
	for _, v := range []string{list[0].GivenName, list[0].FamilyName, list[0].WorkEmail} {
		if strings.Contains(v, "Ada") || strings.Contains(v, "example") {
			t.Fatalf("the person record carries candidate data: %q", v)
		}
	}
	// redelivery of the same hand-off creates nothing new
	if _, err := w.db.Exec(`UPDATE deliveries SET status='pending'`); err != nil {
		t.Fatal(err)
	}
	w.pump()
	if again, _, _ := svc.ListPeople(w.ctx, hr, people.PersonFilter{}); len(again) != 1 {
		t.Fatalf("%d people after a redelivery", len(again))
	}
}

// approval.requested / approval.decided for a requisition through the real engine; separation of duties holds.
func TestRequisitionApprovalThroughTheEngine(t *testing.T) {
	w := newWorld(t)
	clk := &clock{}
	_, jh := newJobs(t, w, clk)
	w.grant("carol", "jobs:admin")
	w.grant("alice", "jobs:recruiter")
	alice := w.token("alice", "jobs", "jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview")
	job := mustCall(t, jh, 201, alice, "POST", "/v1/jobs", map[string]any{
		"title": "Engineer", "department": "Engineering", "location": map[string]any{"city": "Austin", "region": "TX", "country": "US"}, "remote": "hybrid",
		"employment_type": "full_time", "hiring_manager": "user:bob", "headcount": 1,
		"sections": []map[string]string{{"heading": "About", "body": "Do good work."}},
	})
	id := job["id"].(string)
	if got := mustCall(t, jh, 200, alice, "POST", "/v1/jobs/"+id+"/approval-requests", nil)["approval"]; got != "pending" {
		t.Fatalf("approval = %v", got)
	}
	var reqID string
	if err := w.db.QueryRow(`SELECT id FROM approval_requests WHERE module_id='jobs'`).Scan(&reqID); err != nil {
		t.Fatalf("no request in the engine: %v", err)
	}
	// the requester cannot decide her own request; someone holding the chain's role can
	if _, err := w.eng.Decide(w.ctx, reqID, "user:alice", "approve", ""); err == nil {
		t.Fatal("the requester approved her own requisition")
	}
	if _, err := w.eng.Decide(w.ctx, reqID, "user:mallory", "approve", ""); err == nil {
		t.Fatal("someone outside the chain approved")
	}
	if _, err := w.eng.Decide(w.ctx, reqID, "user:carol", "approve", ""); err != nil {
		t.Fatal(err)
	}
	w.pump()
	if got := mustCall(t, jh, 200, alice, "GET", "/v1/jobs/"+id, nil)["approval"]; got != "approved" {
		t.Fatalf("approval after the decision = %v", got)
	}
}

// the same for a finance invoice: can-core decides, finance applies it, the approver is someone else.
func TestInvoiceApprovalThroughTheEngine(t *testing.T) {
	w := newWorld(t)
	_, fh := newFinance(t, w)
	w.grant("carol", "finance:finance")
	w.grant("alice", "finance:finance")
	w.grant("bob", "finance:approver")
	alice := w.token("alice", "finance", "finance.read", "finance.write")
	bob := w.token("bob", "finance", "finance.approve")

	party := mustCall(t, fh, 201, alice, "POST", "/v1/parties", map[string]any{"name": "Acme Pty", "kind": "customer", "email": "ap@acme.example", "address": "1 Fake Road"})
	inv := mustCall(t, fh, 201, alice, "POST", "/v1/invoices", map[string]any{"party_id": party["id"], "lines": []map[string]any{{"description": "Consulting", "quantity_milli": 1000, "unit_price_minor": 150000}}})
	invID := itoa(int(inv["id"].(float64)))
	if code, _ := call(t, fh, bob, "POST", "/v1/invoices/"+invID+"/approve", nil); code != 403 {
		t.Fatalf("approved without a can-core decision: %d", code)
	}
	mustCall(t, fh, 200, alice, "POST", "/v1/invoices/"+invID+"/request-approval", nil)
	var reqID string
	if err := w.db.QueryRow(`SELECT id FROM approval_requests WHERE module_id='finance'`).Scan(&reqID); err != nil {
		t.Fatalf("no request in the engine: %v", err)
	}
	if _, err := w.eng.Decide(w.ctx, reqID, "user:alice", "approve", ""); err == nil {
		t.Fatal("the requester approved her own invoice")
	}
	if _, err := w.eng.Decide(w.ctx, reqID, "user:carol", "approve", ""); err != nil {
		t.Fatal(err)
	}
	w.pump()
	out := mustCall(t, fh, 200, bob, "POST", "/v1/invoices/"+invID+"/approve", nil)
	if out["status"] != "approved" || out["approved_by"] != "user:bob" {
		t.Fatalf("%v", out)
	}
	// amounts and party names stay out of the events can-core keeps
	w.noPersonalDataInCanCore("Acme", "ap@acme.example")
}

// a person on leave is masked as active in what leaves the people module, all the way to can-core.
func TestLeaveDoesNotLeakThroughEvents(t *testing.T) {
	w := newWorld(t)
	svc, d, _ := newPeople(t, w)
	hr := people.Actor{Subject: "svc", Roles: []string{people.RoleHR, people.RoleAdmin}}
	p, err := svc.CreatePerson(w.ctx, hr, people.PersonInput{Kind: "employee", Status: people.StActive, GivenName: "Test", FamilyName: "Person"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChangeStatus(w.ctx, hr, p.ID, people.StLeave, "", "family matter"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverPending(w.ctx, d); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.db.QueryRow(`SELECT count(*) FROM events WHERE type='person.created.v1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("person.created.v1 in can-core: %d %v", n, err)
	}
	w.noPersonalDataInCanCore("leave", "family matter", "Test", "Person")
}

// a token bound to one module is useless against another, end to end.
func TestPersonalAccessTokenIsBoundToItsModule(t *testing.T) {
	w := newWorld(t)
	_, fh := newFinance(t, w)
	_, jh := newJobs(t, w, &clock{})
	w.grant("alice", "finance:admin")
	fin := w.token("alice", "finance", "finance.admin")
	if code, _ := call(t, fh, fin, "GET", "/v1/parties", nil); code != 200 {
		t.Fatalf("own module: %d", code)
	}
	if code, _ := call(t, jh, fin, "GET", "/v1/jobs", nil); code != 401 {
		t.Fatalf("a finance token worked against jobs: %d", code)
	}
	iss := mustCallIssue(t, w, "finance")
	if err := w.pats.Revoke(w.ctx, "user:alice", iss.ID, false); err != nil {
		t.Fatal(err)
	}
	// the module caches answers for a short time: a fresh module instance shows the revoked answer
	_, fh2 := newFinance2(t, w)
	if code, _ := call(t, fh2, iss.Token, "GET", "/v1/parties", nil); code != 401 {
		t.Fatalf("a revoked token worked: %d", code)
	}
}

func mustCallIssue(t *testing.T, w *world, module string) pat.Issued {
	t.Helper()
	iss, err := w.pats.Issue(w.ctx, "user:alice", pat.IssueRequest{Name: "x", Module: module, Permissions: []string{"finance.read"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

// newFinance2 builds a second finance instance on the same registered module (a restart).
func newFinance2(t *testing.T, w *world) (*finance.App, http.Handler) {
	t.Helper()
	can := &modclient.Client{BaseURL: w.core.URL, TokenFile: filepath.Join(w.dir, "finance.token"), ModuleID: "finance"}
	cfg := finance.DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "finance.db")
	app, err := finance.NewApp(cfg, finance.Secrets{AdminToken: "admin-token-0123456789abcdefABCDEF-xyz", LinkSecret: "link-secret-0123456789abcdef0123456789abcdef"},
		finance.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Can: can, CanSecret: w.hmac["finance"]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app, app.Handler()
}

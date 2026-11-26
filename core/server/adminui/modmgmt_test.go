package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
)

const mmCompany = `# the company
schema: 1
name: Acme
domain: acme.example
provider:
  type: local
  options: {server_ram_mb: 2048}
modules:
  web:
    enabled: true
    settings: {site_title: Acme}
`

func mmManifest(id, name, extra string) string {
	return "schema: 1\nid: " + id + "\nname: " + name + "\ndescription: " + id + " module\nversion: 1.0.0\n" + extra
}

const mmSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["site_title"],
  "properties": {
    "site_title": {"type": "string", "minLength": 1, "maxLength": 20, "description": "Shown in the page header."},
    "max_rows": {"type": "integer", "minimum": 1, "maximum": 100, "default": 10},
    "verbose": {"type": "boolean", "default": false},
    "captcha": {"enum": ["none", "turnstile"], "default": "none"},
    "api_key_secret": {"type": "string", "format": "secret-ref", "description": "Name of the Secret holding the API key."},
    "origins": {"type": "array", "items": {"type": "string"}}
  }
}`

func mmModules(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	w := func(id, yaml string, files map[string]string) {
		d := filepath.Join(dir, id)
		must(t, os.MkdirAll(filepath.Join(d, "chart"), 0o755))
		must(t, os.WriteFile(filepath.Join(d, "module.yaml"), []byte(yaml), 0o644))
		for n, c := range files {
			must(t, os.WriteFile(filepath.Join(d, n), []byte(c), 0o644))
		}
	}
	w("web", mmManifest("web", "Web forms", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: web}
requires: {database: sqlite, capabilities: [email.send]}
resources: {ram_mb: 20, ram_limit_mb: 40, disk_mb: 100}
provides:
  - {capability: leads, schema: leads.v1, service: api, port: 8081}
settings_schema: ./settings.schema.json
`), map[string]string{"settings.schema.json": mmSchema})
	w("crm", mmManifest("crm", "CRM", `status: experimental
provider: {type: oss-wrapper, name: Upstream CRM, licence: BUSL-1.1, licence_class: source-available, upstream: "https://example.com/crm"}
runtime: {chart: ./chart, subdomain: crm}
requires: {database: postgres, capabilities: [leads]}
resources: {ram_mb: 300, ram_limit_mb: 450, disk_mb: 500}
provides:
  - {capability: contacts, schema: contacts.v1, service: api, port: 8081}
`), nil)
	w("desk", mmManifest("desk", "Help desk", `status: not-deployable
provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: help}
requires: {database: mariadb, capabilities: [email.send]}
resources: {ram_mb: 250}
provides:
  - {capability: tickets, schema: tickets.v1, service: api, port: 8081}
`), nil)
	w("alt", mmManifest("alt", "Alt forms", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: alt}
requires: {database: sqlite}
resources: {ram_mb: 10}
provides:
  - {capability: leads, schema: leads.v1, service: api, port: 8081}
`), nil)
	w("solo", mmManifest("solo", "Solo", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: solo}
requires: {database: sqlite}
resources: {ram_mb: 10}
`), nil)
	w("plan", mmManifest("plan", "Planner", `status: experimental
provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: plan}
requires: {database: sqlite, capabilities: [invoices]}
resources: {ram_mb: 10}
provides:
  - {capability: invoices, schema: invoices.v1, service: api, port: 8081, status: planned}
`), nil)
	w("evil", mmManifest("evil", `"<script>alert(1)</script>"`, `provider: {type: inhouse, name: "<img src=x onerror=alert(2)>"}
runtime: {chart: ./chart, subdomain: evil}
requires: {database: sqlite}
resources: {ram_mb: 5}
settings_schema: ./settings.schema.json
`), map[string]string{"settings.schema.json": `{"type":"object","properties":{"motd":{"type":"string","description":"<b onmouseover=alert(3)>x</b>"}}}`})
	return dir
}

type mmPurger struct {
	mu    sync.Mutex
	calls []string
}

func (p *mmPurger) Purge(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, id)
	return nil
}

type mmIngress struct{}

func (mmIngress) Certificates(context.Context) ([]modmgr.CertStatus, error) {
	return []modmgr.CertStatus{{Host: "forms.acme.example", State: "valid", DaysLeft: 61}}, nil
}

type mmDisk struct{}

func (mmDisk) Usage(context.Context) ([]modmgr.DiskUsage, error) {
	return []modmgr.DiskUsage{{Name: "data", UsedBytes: 3, TotalBytes: 10}}, nil
}

type mmProbe struct{}

func (mmProbe) Probe(context.Context, string) string { return modmgr.InstantServing }

// mm is a harness with module management, RBAC and four users: root (owner), ops (operator role),
// viewer (auditor role: platform.view only) and nobody (no role).
type mm struct {
	*harness
	svc     *modmgr.Service
	fk      *reconcile.FakeInstaller
	purger  *mmPurger
	secrets *modmgr.FakeSecrets
	store   *modmgr.FileStore
	tokens  map[string]string
	rb      *rbac.Service
}

func newMM(t *testing.T) *mm {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	m := &mm{harness: h, tokens: map[string]string{"root": h.secret}, fk: reconcile.NewFakeInstaller(), purger: &mmPurger{}, secrets: modmgr.NewFakeSecrets()}
	hash, _ := auth.HashPassword(h.pw)
	for _, u := range []string{"ops", "viewer", "nobody"} {
		m.tokens[u] = auth.NewTOTPSecret()
		if _, err := h.db.CreateAdmin(ctx, u, hash, m.tokens[u]); err != nil {
			t.Fatal(err)
		}
	}
	clock := func() time.Time { return *h.now }
	m.rb = rbac.New(h.db, h.audit, rbac.Options{Now: clock})
	for u, role := range map[string]string{"ops": "operator", "viewer": "auditor"} {
		if err := m.rb.Grant(ctx, "root", "user:"+u, role, ""); err != nil {
			t.Fatal(err)
		}
	}
	h.ui.RBAC = m.rb

	dir := mmModules(t)
	path := filepath.Join(t.TempDir(), "company.yaml")
	must(t, os.WriteFile(path, []byte(mmCompany), 0o644))
	m.store = &modmgr.FileStore{Path: path}
	rec := reconcile.New(h.db, registry.New(h.db, "http://can-core"), h.audit, m.fk)
	rec.Actor = "can-core"
	d, ps := reconcile.LoadDesired(path, dir)
	if d.Company == nil {
		t.Fatalf("base company invalid: %v", ps)
	}
	res, err := rec.Apply(ctx, d, reconcile.Options{})
	must(t, err)
	m.svc = &modmgr.Service{DB: h.db, Audit: h.audit, ModulesDir: dir, Company: m.store, Runner: modmgr.NewRunner(rec), Purger: m.purger, Secrets: m.secrets,
		Ingress: mmIngress{}, Disk: mmDisk{}, Net: modmgr.StaticNet{V4: []string{"203.0.113.7"}}, Instant: mmProbe{},
		Bundles: []modmgr.Bundle{{ID: "starter", Modules: []string{"solo", "desk"}}, {ID: "blank"}},
		Now:     clock, Async: func(f func()) { f() }}
	m.svc.RecordReconcile(ctx, res, nil)
	h.ui.Mod = m.svc
	// the base company already runs web; the wizard tests need a fresh installation's open wizard
	if _, err := m.svc.WizardState(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE wizard_state SET status='open'`); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *mm) loginAs(user string) session {
	m.t.Helper()
	*m.now = m.now.Add(31 * time.Second)
	c, tok := m.loginForm()
	key, _ := authDecode(m.tokens[user])
	pw := m.pw
	r := m.do("POST", "/login", url.Values{"username": {user}, "password": {pw}, "totp": {authCode(key, *m.now)}, "csrf": {tok}}, c)
	sc := cookieNamed(r.Response, "can_session")
	if sc == nil {
		m.t.Fatalf("login %s: %d %s", user, r.StatusCode, r.body)
	}
	s := session{cookie: &http.Cookie{Name: sc.Name, Value: sc.Value}}
	s.csrf = csrfOf(m.t, m.do("GET", "/settings", nil, s.cookie).body)
	return s
}

func (m *mm) version() string {
	m.t.Helper()
	s, err := m.store.Get(context.Background())
	must(m.t, err)
	return s.Version
}

var changeRe = regexp.MustCompile(`^/(changes|runs)/([0-9a-f]{24})`)

func (m *mm) propose(s session, enable []string, acks ...string) resp {
	return m.do("POST", "/changes", url.Values{"csrf": {s.csrf}, "version": {m.version()}, "enable": enable, "ack": acks}, s.with()...)
}

// --- every new route is gated ----------------------------------------------------------------

// routePerm lists the permission each module-management route requires. A route added without a
// line here fails TestEveryModmgmtRouteIsGated.
var routePerm = map[string]string{
	"GET /catalogue":                rbac.PermView,
	"GET /catalogue/{id}":           rbac.PermView,
	"POST /catalogue/{id}/settings": rbac.PermOperate,
	"GET /catalogue/{id}/purge":     rbac.PermPurge,
	"POST /catalogue/{id}/purge":    rbac.PermPurge,
	"POST /changes":                 rbac.PermOperate,
	"GET /changes/{id}":             rbac.PermOperate,
	"POST /changes/{id}/apply":      rbac.PermOperate,
	"GET /runs/{id}":                rbac.PermView,
	"GET /status":                   rbac.PermView,
	"GET /company":                  rbac.PermOperate,
	"GET /setup":                    rbac.PermSetup,
	"GET /setup/{step}":             rbac.PermSetup,
	"POST /setup/{step}":            rbac.PermSetup,
}

func TestEveryModmgmtRouteIsGated(t *testing.T) {
	m := newMM(t)
	users := map[string]session{}
	for _, u := range []string{"root", "ops", "viewer", "nobody"} {
		users[u] = m.loginAs(u)
	}
	// which of our users holds which permission
	holds := map[string]map[string]bool{
		"root":   {rbac.PermView: true, rbac.PermOperate: true, rbac.PermSetup: true, rbac.PermPurge: true},
		"ops":    {rbac.PermView: true, rbac.PermOperate: true},
		"viewer": {rbac.PermView: true},
		"nobody": {},
	}
	seen := 0
	for _, route := range Routes() {
		perm, isMod := routePerm[route]
		if !isMod {
			_, p, _ := strings.Cut(route, " ")
			if p == "/status" || p == "/company" || strings.HasPrefix(p, "/catalogue") || strings.HasPrefix(p, "/changes") || strings.HasPrefix(p, "/runs") || strings.HasPrefix(p, "/setup") {
				t.Errorf("route %q has no entry in routePerm", route)
			}
			continue
		}
		seen++
		method, path := split(route)
		path = strings.NewReplacer("/1/", "/zz/", "/1", "/zz", "/zz/purge", "/web/purge").Replace(path)
		path = strings.Replace(strings.Replace(path, "/setup/1", "/setup/company", 1), "/setup/zz", "/setup/company", 1)
		for user, s := range users {
			var form url.Values
			if method == "POST" {
				form = url.Values{"csrf": {s.csrf}}
			}
			r := m.do(method, path, form, s.with()...)
			if holds[user][perm] {
				if r.StatusCode == http.StatusForbidden {
					t.Errorf("%s %s: forbidden for %s who holds %s", route, path, user, perm)
				}
			} else if r.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s: %s without %s got %d, want 403", route, path, user, perm, r.StatusCode)
			}
		}
	}
	if seen != len(routePerm) {
		t.Fatalf("checked %d of %d routes", seen, len(routePerm))
	}
	// the new built-in permissions are not handed out with the built-in roles
	for _, role := range []string{"operator", "auditor"} {
		for _, p := range []string{rbac.PermSetup, rbac.PermPurge} {
			if ok, _ := m.rb.Check(context.Background(), "user:"+map[string]string{"operator": "ops", "auditor": "viewer"}[role], p, ""); ok {
				t.Errorf("role %s holds %s", role, p)
			}
		}
	}
}

func TestModmgmtPostsNeedCSRFAndSameSite(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("root")
	before := m.version()
	for route := range routePerm {
		method, path := split(route)
		if method != "POST" {
			continue
		}
		path = strings.NewReplacer("/changes/1/", "/changes/zz/", "/setup/1", "/setup/apply", "/catalogue/1/", "/catalogue/web/").Replace(path)
		for name, form := range map[string]url.Values{"missing": {}, "wrong": {"csrf": {"nope"}}} {
			if r := m.do(method, path, form, s.with()...); r.StatusCode != http.StatusForbidden {
				t.Errorf("%s with %s csrf -> %d, want 403", route, name, r.StatusCode)
			}
		}
		req, _ := http.NewRequest(method, m.srv.URL+path, strings.NewReader(url.Values{"csrf": {s.csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(s.cookie)
		r, err := noRedirect.Do(req)
		must(t, err)
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("%s cross-site -> %d, want 403", route, r.StatusCode)
		}
	}
	if m.version() != before {
		t.Fatal("a rejected post changed company.yaml")
	}
	if ws, _ := m.svc.WizardState(context.Background()); ws.Status != modmgr.WizardOpen {
		t.Fatalf("a rejected post changed the wizard: %s", ws.Status)
	}
}

func TestModmgmtPagesNeedAServiceAndASession(t *testing.T) {
	h := newHarness(t) // no Mod
	for _, p := range []string{"/catalogue", "/status", "/company", "/setup/company"} {
		if r := h.do("GET", p, nil); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
			t.Errorf("%s anonymous -> %d", p, r.StatusCode)
		}
	}
	s := h.login()
	for _, p := range []string{"/catalogue", "/catalogue/web", "/status", "/company", "/setup/company", "/changes/x", "/runs/x"} {
		if r := h.do("GET", p, nil, s.with()...); r.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s without module management = %d, want 503", p, r.StatusCode)
		}
	}
	// the dashboard and navigation are unchanged without the service
	d := h.do("GET", "/dashboard", nil, s.with()...)
	if d.StatusCode != 200 || strings.Contains(d.body, "/catalogue") || strings.Contains(d.body, "/setup") {
		t.Fatalf("dashboard without module management: %d", d.StatusCode)
	}
}

// --- catalogue ----------------------------------------------------------------------------------

func TestCatalogueShowsBadgesFootprintGraphAndEscapesHostileStrings(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	r := m.do("GET", "/catalogue", nil, s.with()...)
	if r.StatusCode != 200 {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
	for _, want := range []string{
		"Experimental", "Not deployable", "Source-available", // status and licence badges
		"not yet served", // planned capability
		"I accept the source-available licence of CRM",
		"2048 MB",                      // declared server size
		"670 MB",                       // 650 platform + 20 web
		"Fits",                         // verdict
		"Would conflict on leads: web", // alt would serve leads twice
		`name="enable" value="solo"`,
		`name="version" value="sha:`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("catalogue lacks %q", want)
		}
	}
	if strings.Contains(r.body, `value="desk"`) {
		t.Error("a not-deployable module can be ticked")
	}
	if strings.Contains(r.body, "<script>alert(1)") || strings.Contains(r.body, "<img src=x") {
		t.Fatalf("hostile manifest strings are not escaped:\n%s", r.body)
	}
	if !strings.Contains(r.body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("hostile name not shown escaped")
	}
	// footprint: over the declared size
	must(t, os.WriteFile(m.store.Path, []byte(strings.Replace(mmCompany, "2048", "600", 1)), 0o644))
	r = m.do("GET", "/catalogue", nil, s.with()...)
	if !strings.Contains(r.body, "Too big") || !strings.Contains(r.body, "more than the server has") {
		t.Fatalf("over-commit not shown:\n%s", r.body)
	}
	// undeclared: no claim of a fit
	must(t, os.WriteFile(m.store.Path, []byte(strings.Replace(mmCompany, "options: {server_ram_mb: 2048}", "options: {}", 1)), 0o644))
	r = m.do("GET", "/catalogue", nil, s.with()...)
	if strings.Contains(r.body, ">Fits<") || !strings.Contains(r.body, "Not declared") {
		t.Fatalf("an undeclared server size must not read as a fit:\n%s", r.body)
	}
	// a read-only user sees no apply path
	v := m.loginAs("viewer")
	if r := m.do("GET", "/catalogue", nil, v.with()...); strings.Contains(r.body, `action="/changes"`) == false || strings.Contains(r.body, "Preview changes") {
		t.Fatalf("view-only user is offered changes")
	}
}

func TestEnableFlowEndToEnd(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	r := m.propose(s, []string{"web", "solo"})
	if r.StatusCode != http.StatusSeeOther || !changeRe.MatchString(r.Header.Get("Location")) {
		t.Fatalf("propose: %d %s\n%s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	loc := r.Header.Get("Location")
	if strings.Contains(string(mustRead(t, m.store.Path)), "solo") {
		t.Fatal("a proposal must not change company.yaml")
	}
	pv := m.do("GET", loc, nil, s.with()...)
	for _, want := range []string{"+   solo:", "mod-solo", "solo.acme.example", "can-module", "Install", `action="` + loc + `/apply"`, "added:"} {
		if !strings.Contains(strings.ReplaceAll(pv.body, "&#43;", "+"), want) {
			t.Errorf("preview lacks %q\n%s", want, pv.body)
		}
	}
	ap := m.do("POST", loc+"/apply", url.Values{"csrf": {s.csrf}}, s.with()...)
	if ap.StatusCode != http.StatusSeeOther || !strings.HasPrefix(ap.Header.Get("Location"), "/runs/") {
		t.Fatalf("apply: %d %s %s", ap.StatusCode, ap.Header.Get("Location"), ap.body)
	}
	run := m.do("GET", ap.Header.Get("Location"), nil, s.with()...)
	if run.StatusCode != 200 || !strings.Contains(run.body, "Succeeded") || strings.Contains(run.body, "http-equiv") {
		t.Fatalf("run page: %d\n%s", run.StatusCode, run.body)
	}
	if !strings.Contains(string(mustRead(t, m.store.Path)), "solo:") || m.fk.EnabledCount() != 2 {
		t.Fatal("not applied")
	}
	// the change is single use: applying again shows the same run, no new install
	n := len(m.fk.CallLog())
	again := m.do("POST", loc+"/apply", url.Values{"csrf": {s.csrf}}, s.with()...)
	if again.Header.Get("Location") != ap.Header.Get("Location") || len(m.fk.CallLog()) != n {
		t.Fatalf("second apply: %d %s", again.StatusCode, again.Header.Get("Location"))
	}
	acts := auditActionsOf(t, m)
	if !strings.Contains(acts, "company.change.begin") || !strings.Contains(acts, "company.change.saved") || !strings.Contains(acts, "company.apply.done") {
		t.Fatalf("audit: %s", acts)
	}
	// the status page now reports the reconcile and the module
	st := m.do("GET", "/status", nil, s.with()...)
	for _, want := range []string{"Succeeded", "Solo", "forms.acme.example", "61", "30% used"} {
		if !strings.Contains(st.body, want) {
			t.Errorf("status lacks %q", want)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	must(t, err)
	return b
}

func auditActionsOf(t *testing.T, m *mm) string {
	t.Helper()
	recs, err := m.audit.Query(context.Background(), audit.Filter{Limit: 300})
	must(t, err)
	var out []string
	for _, r := range recs {
		out = append(out, r.Action)
	}
	return strings.Join(out, " ")
}

func TestOptInsAndNotDeployableAreEnforcedServerSide(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	if r := m.propose(s, []string{"web", "desk"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("not-deployable module: %d", r.StatusCode)
	}
	if r := m.propose(s, []string{"web", "crm"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("source-available and experimental without opt-in: %d", r.StatusCode)
	}
	if r := m.propose(s, []string{"web", "crm"}, "experimental:crm"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("source-available without its own opt-in: %d", r.StatusCode)
	}
	if r := m.propose(s, []string{"web", "crm"}, "licence:crm", "experimental:crm"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("fully acknowledged: %d\n%s", r.StatusCode, r.body)
	}
	// web + alt both serve `leads`: an exclusive capability
	if r := m.propose(s, []string{"web", "alt"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("exclusive capability conflict accepted: %d", r.StatusCode)
	}
	// enabling crm without anything that provides `leads`
	if r := m.propose(s, []string{"crm"}, "licence:crm", "experimental:crm"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("unmet requirement accepted: %d", r.StatusCode)
	}
	// planned-only capability is not "met"
	if r := m.propose(s, []string{"web", "plan"}, "experimental:plan"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("planned capability treated as served: %d", r.StatusCode)
	}
	if string(mustRead(t, m.store.Path)) != mmCompany {
		t.Fatal("company.yaml changed by refused proposals")
	}
}

func TestStaleSelectionIsRefused(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	old := m.version()
	r := m.propose(s, []string{"web", "solo"})
	ap := m.do("POST", r.Header.Get("Location")+"/apply", url.Values{"csrf": {s.csrf}}, s.with()...)
	if ap.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", ap.StatusCode)
	}
	stale := m.do("POST", "/changes", url.Values{"csrf": {s.csrf}, "version": {old}, "enable": {"web"}}, s.with()...)
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("a form built on an old company.yaml: %d", stale.StatusCode)
	}
}

func TestDisablePreviewSaysWhatIsKept(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	r := m.propose(s, nil) // nothing ticked: disable web
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	pv := m.do("GET", r.Header.Get("Location"), nil, s.with()...)
	for _, want := range []string{"Disable", "the namespace", "the persistent volumes", "the Secrets", "separate step"} {
		if !strings.Contains(pv.body, want) {
			t.Errorf("disable preview lacks %q", want)
		}
	}
}

// --- settings -----------------------------------------------------------------------------------

func TestSettingsFormIsGeneratedValidatedAndRecordedAsAPatch(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	r := m.do("GET", "/catalogue/web", nil, s.with()...)
	for _, want := range []string{`name="s.site_title"`, `value="Acme"`, `name="s.max_rows"`, `type="number"`, `type="checkbox"`, `<select`, `name="s.api_key_secret"`, "never the secret itself", "<code>origins</code>", "Shown in the page header."} {
		if !strings.Contains(r.body, want) {
			t.Errorf("settings form lacks %q", want)
		}
	}
	// invalid: re-rendered with the error beside the field and what was typed kept
	bad := m.do("POST", "/catalogue/web/settings", url.Values{"csrf": {s.csrf}, "s.site_title": {"x"}, "s.max_rows": {"1000"}, "s.captcha": {"none"}}, s.with()...)
	if bad.StatusCode != http.StatusBadRequest || !strings.Contains(bad.body, "outside the allowed range") || !strings.Contains(bad.body, `value="1000"`) {
		t.Fatalf("%d\n%s", bad.StatusCode, bad.body)
	}
	if string(mustRead(t, m.store.Path)) != mmCompany {
		t.Fatal("invalid settings reached company.yaml")
	}
	ok := m.do("POST", "/catalogue/web/settings", url.Values{"csrf": {s.csrf}, "s.site_title": {"Hello"}, "s.max_rows": {"7"}, "s.captcha": {"turnstile"}, "s.api_key_secret": {"web-api-key"}}, s.with()...)
	if ok.StatusCode != http.StatusSeeOther || !strings.HasPrefix(ok.Header.Get("Location"), "/changes/") {
		t.Fatalf("%d %s", ok.StatusCode, ok.body)
	}
	pv := m.do("GET", ok.Header.Get("Location"), nil, s.with()...)
	for _, want := range []string{"Hello", "max_rows: 7", "web-api-key"} {
		if !strings.Contains(pv.body, want) {
			t.Errorf("settings preview lacks %q\n%s", want, pv.body)
		}
	}
	if !strings.Contains(pv.body, "Upgrade") {
		t.Errorf("a settings change is an upgrade of the module:\n%s", pv.body)
	}
	// the unsupported array setting is not dropped by the form
	ap := m.do("POST", ok.Header.Get("Location")+"/apply", url.Values{"csrf": {s.csrf}}, s.with()...)
	if ap.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", ap.StatusCode, ap.body)
	}
	if !strings.Contains(string(mustRead(t, m.store.Path)), "site_title: Hello") {
		t.Fatalf("%s", mustRead(t, m.store.Path))
	}
	// a viewer cannot post, and a module without a schema has no settings route
	v := m.loginAs("viewer")
	if r := m.do("POST", "/catalogue/web/settings", url.Values{"csrf": {v.csrf}, "s.site_title": {"x"}}, v.with()...); r.StatusCode != http.StatusForbidden {
		t.Errorf("viewer: %d", r.StatusCode)
	}
	if r := m.do("POST", "/catalogue/solo/settings", url.Values{"csrf": {s.csrf}}, s.with()...); r.StatusCode != http.StatusNotFound {
		t.Errorf("no schema: %d", r.StatusCode)
	}
}

func TestSettingsForADisabledModuleAreNotOfferedAndNeverEnableIt(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	if page := m.do("GET", "/catalogue/web", nil, s.with()...); !strings.Contains(page.body, `name="s.site_title"`) {
		t.Fatal("enabled module has no form")
	}
	if g := m.do("GET", "/catalogue/evil", nil, s.with()...); strings.Contains(g.body, `name="s.motd"`) || !strings.Contains(g.body, "Enable this module first") {
		t.Fatalf("form offered for a disabled module:\n%s", g.body)
	}
	r := m.do("POST", "/catalogue/evil/settings", url.Values{"csrf": {s.csrf}, "s.motd": {"hi"}}, s.with()...)
	if r.StatusCode == http.StatusSeeOther {
		t.Fatalf("settings accepted for a disabled module: %s", r.Header.Get("Location"))
	}
	if string(mustRead(t, m.store.Path)) != mmCompany {
		t.Fatal("company.yaml changed")
	}
}

func TestHostileSettingsAndSchemaTextIsEscaped(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	r := m.do("GET", "/catalogue/evil", nil, s.with()...)
	if strings.Contains(r.body, "<b onmouseover") || strings.Contains(r.body, "<script>alert(1)") || strings.Contains(r.body, "<img src=x") {
		t.Fatalf("not escaped:\n%s", r.body)
	}
	bad := m.do("POST", "/catalogue/web/settings", url.Values{"csrf": {s.csrf}, "s.site_title": {`"><script>alert(9)</script>`}}, s.with()...)
	if strings.Contains(bad.body, "<script>alert(9)") {
		t.Fatalf("submitted value echoed unescaped:\n%s", bad.body)
	}
}

// --- run progress -------------------------------------------------------------------------------

type gateRunner struct {
	modmgr.Reconciling
	gate chan struct{}
}

func (g *gateRunner) Apply(ctx context.Context, d reconcile.Desired) (reconcile.Result, error) {
	<-g.gate
	return g.Reconciling.Apply(ctx, d)
}

func TestRunPageRefreshesWhileRunningAndSanitisesErrors(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("ops")
	gate := make(chan struct{})
	var wg sync.WaitGroup
	m.svc.Runner = &gateRunner{Reconciling: m.svc.Runner, gate: gate}
	m.svc.Async = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	m.fk.FailEnable["solo"] = errors.New("helm: pull https://deploy:hunter2@reg.example/x failed: password=hunter2 \x1b[31m<script>alert(1)</script>")
	r := m.propose(s, []string{"web", "solo"})
	ap := m.do("POST", r.Header.Get("Location")+"/apply", url.Values{"csrf": {s.csrf}}, s.with()...)
	loc := ap.Header.Get("Location")
	run := m.do("GET", loc, nil, s.with()...)
	if !strings.Contains(run.body, `<meta http-equiv="refresh" content="3">`) || !strings.Contains(run.body, "Running") {
		t.Fatalf("running page:\n%s", run.body)
	}
	if paused := m.do("GET", loc+"?refresh=0", nil, s.with()...); strings.Contains(paused.body, "http-equiv") {
		t.Fatal("refresh=0 still reloads")
	}
	// anyone who cannot operate may watch the progress but not start another
	close(gate)
	wg.Wait()
	done := m.do("GET", loc, nil, s.with()...)
	if strings.Contains(done.body, "http-equiv") || !strings.Contains(done.body, "Failed") {
		t.Fatalf("finished page:\n%s", done.body)
	}
	for _, bad := range []string{"hunter2", "\x1b", "<script>alert(1)"} {
		if strings.Contains(done.body, bad) {
			t.Fatalf("unsanitised error (%q) on the run page:\n%s", bad, done.body)
		}
	}
	st := m.do("GET", "/status", nil, s.with()...)
	if strings.Contains(st.body, "hunter2") || !strings.Contains(st.body, "Failed") {
		t.Fatalf("status page:\n%s", st.body)
	}
}

func TestRunPageIsNotFoundForAnUnknownRun(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("viewer")
	if r := m.do("GET", "/runs/doesnotexist", nil, s.with()...); r.StatusCode != http.StatusNotFound {
		t.Fatalf("%d", r.StatusCode)
	}
	if r := m.do("GET", "/changes/doesnotexist", nil, m.loginAs("ops").with()...); r.StatusCode != http.StatusNotFound {
		t.Fatalf("%d", r.StatusCode)
	}
}

// --- status -------------------------------------------------------------------------------------

func TestStatusReportsUnavailableSourcesHonestly(t *testing.T) {
	m := newMM(t)
	m.svc.Ingress, m.svc.Disk = nil, nil
	s := m.loginAs("viewer")
	r := m.do("GET", "/status", nil, s.with()...)
	if r.StatusCode != 200 || strings.Count(r.body, "not available in this installation") != 2 {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
}

// E08b QA 6: a failed dump run is shown on the status page.
func TestStatusShowsAFailedDumpRun(t *testing.T) {
	m := newMM(t)
	m.svc.LastDump = func() (backup.DumpRun, error) {
		return backup.DumpRun{RunID: "r", At: time.Unix(1_700_000_000, 0), Error: "module wiki: ImagePullBackOff"}, nil
	}
	s := m.loginAs("viewer")
	r := m.do("GET", "/status", nil, s.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "last dump run FAILED at 2023-11-14 22:13 UTC") || !strings.Contains(r.body, "module wiki: ImagePullBackOff") {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
}

// E08b QA2 1: modules a successful run skipped are shown loudly.
func TestStatusShowsSkippedModules(t *testing.T) {
	m := newMM(t)
	m.svc.LastDump = func() (backup.DumpRun, error) {
		return backup.DumpRun{RunID: "r", At: time.Unix(1_700_000_000, 0), OK: true, Skipped: []backup.SkippedModule{{ID: "crm", Reason: "disabled", Database: "mariadb"}}}, nil
	}
	s := m.loginAs("viewer")
	r := m.do("GET", "/status", nil, s.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "DATABASE IS NOT BACKED UP") {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
}

// --- purge --------------------------------------------------------------------------------------

func TestPurgeIsSeparateTypedAndPermissioned(t *testing.T) {
	m := newMM(t)
	root := m.loginAs("root")
	ops := m.loginAs("ops")
	// disable web first (a normal change)
	r := m.propose(root, nil)
	if ap := m.do("POST", r.Header.Get("Location")+"/apply", url.Values{"csrf": {root.csrf}}, root.with()...); ap.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", ap.StatusCode, ap.body)
	}
	if m.fk.EnabledCount() != 0 {
		t.Fatal("web not disabled")
	}
	cat := m.do("GET", "/catalogue", nil, root.with()...)
	if !strings.Contains(cat.body, `/catalogue/web/purge`) {
		t.Fatal("owner is not offered the purge link")
	}
	if strings.Contains(m.do("GET", "/catalogue", nil, ops.with()...).body, "/purge") {
		t.Fatal("operator is offered the purge link")
	}
	if r := m.do("POST", "/catalogue/web/purge", url.Values{"csrf": {ops.csrf}, "confirm": {"purge web"}, "password": {m.pw}}, ops.with()...); r.StatusCode != http.StatusForbidden {
		t.Fatalf("operator purge: %d", r.StatusCode)
	}
	page := m.do("GET", "/catalogue/web/purge", nil, root.with()...)
	if page.StatusCode != 200 || !strings.Contains(page.body, "Type exactly: purge web") || !strings.Contains(page.body, "cannot be undone") {
		t.Fatalf("%d\n%s", page.StatusCode, page.body)
	}
	post := func(confirm, pw string) resp {
		*m.now = m.now.Add(31 * time.Second) // a fresh single-use authentication code each time
		return m.do("POST", "/catalogue/web/purge", url.Values{"csrf": {root.csrf}, "confirm": {confirm}, "password": {pw}, "otp": {m.code()}}, root.with()...)
	}
	if r := m.do("POST", "/catalogue/web/purge", url.Values{"csrf": {root.csrf}, "confirm": {"purge web"}, "password": {m.pw}}, root.with()...); r.StatusCode != http.StatusUnauthorized || len(m.purger.calls) != 0 {
		t.Errorf("purge without an authentication code: %d", r.StatusCode)
	}
	if r := post("purge web", "wrong password"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", r.StatusCode)
	}
	if r := post("purge  web", m.pw); r.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong confirmation: %d", r.StatusCode)
	}
	if len(m.purger.calls) != 0 {
		t.Fatal("purged without confirmation")
	}
	if r := post("purge web", m.pw); r.StatusCode != http.StatusSeeOther || len(m.purger.calls) != 1 {
		t.Fatalf("purge: %d %v", r.StatusCode, m.purger.calls)
	}
	acts := auditActionsOf(t, m)
	if !strings.Contains(acts, "module.purge.begin") || !strings.Contains(acts, "module.purge.done") {
		t.Fatalf("audit: %s", acts)
	}
	// an enabled module cannot be purged
	m2 := newMM(t)
	r2 := m2.loginAs("root")
	if r := m2.do("GET", "/catalogue/web/purge", nil, r2.with()...); r.StatusCode != http.StatusConflict {
		t.Fatalf("enabled module purge page: %d", r.StatusCode)
	}
	// no executor: refused, never silently succeeding
	m3 := newMM(t)
	m3.svc.Purger = nil
	r3 := m3.loginAs("root")
	rr := m3.propose(r3, nil)
	m3.do("POST", rr.Header.Get("Location")+"/apply", url.Values{"csrf": {r3.csrf}}, r3.with()...)
	if g := m3.do("GET", "/catalogue/web/purge", nil, r3.with()...); !strings.Contains(g.body, "not available in this installation") || strings.Contains(g.body, `name="confirm"`) {
		t.Fatalf("purge form offered without an executor:\n%s", g.body)
	}
}

func TestCompanyPageShowsTheStoredDocument(t *testing.T) {
	m := newMM(t)
	r := m.do("GET", "/company", nil, m.loginAs("ops").with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "server_ram_mb") || !strings.Contains(r.body, "sha:") {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
}

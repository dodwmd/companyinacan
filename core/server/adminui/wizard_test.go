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

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
)

func (m *mm) post(s session, path string, form url.Values) resp {
	m.t.Helper()
	form.Set("csrf", s.csrf)
	return m.do("POST", path, form, s.with()...)
}

func TestFirstLoginRedirectsOnlyTheOwnerToTheWizard(t *testing.T) {
	m := newMM(t)
	root, ops := m.loginAs("root"), m.loginAs("ops")
	r := m.do("GET", "/", nil, root.with()...)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/setup" {
		t.Fatalf("owner: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := m.do("GET", "/", nil, ops.with()...); r.StatusCode != 200 {
		t.Fatalf("operator is redirected: %d", r.StatusCode)
	}
	if r := m.do("GET", "/setup", nil, ops.with()...); r.StatusCode != http.StatusForbidden {
		t.Fatalf("operator in the wizard: %d", r.StatusCode)
	}
	if d := m.do("POST", "/setup/dismiss", url.Values{"csrf": {root.csrf}}, root.with()...); d.StatusCode != http.StatusSeeOther || d.Header.Get("Location") != "/?flash=flash.wizard_dismissed" {
		t.Fatalf("dismiss: %d %s", d.StatusCode, d.Header.Get("Location"))
	}
	dash := m.do("GET", "/", nil, root.with()...)
	if dash.StatusCode != 200 || strings.Contains(dash.body, `href="/setup"`) {
		t.Fatalf("after dismissal: %d", dash.StatusCode)
	}
	if !strings.Contains(auditActionsOf(t, m), "wizard.dismiss") {
		t.Fatal("not audited")
	}
}

func TestWizardStepsAreSavedAndResumable(t *testing.T) {
	m := newMM(t)
	root := m.loginAs("root")
	if r := m.post(root, "/setup/company", url.Values{"name": {"Acme Two"}, "domain": {"Acme-Two.Example"}, "server_ram_mb": {"4096"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	// a different session (a different browser, later) sees the saved answers
	again := m.loginAs("root")
	r := m.do("GET", "/setup/company", nil, again.with()...)
	for _, want := range []string{`value="Acme Two"`, `value="acme-two.example"`, `value="4096"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("saved answer %s not shown", want)
		}
	}
	// the same post twice changes nothing more
	rev := func() int64 { ws, _ := m.svc.WizardState(context.Background()); return ws.Revision }
	before := rev()
	m.post(root, "/setup/company", url.Values{"name": {"Acme Two"}, "domain": {"acme-two.example"}, "server_ram_mb": {"4096"}})
	if rev() != before {
		t.Fatal("an identical resubmission changed the wizard")
	}
	for name, form := range map[string]url.Values{
		"bad domain": {"name": {"A"}, "domain": {"not a domain"}},
		"no name":    {"name": {""}, "domain": {"a.example"}},
		"bad ram":    {"name": {"A"}, "domain": {"a.example"}, "server_ram_mb": {"12"}},
		"ram text":   {"name": {"A"}, "domain": {"a.example"}, "server_ram_mb": {"lots"}},
	} {
		if r := m.post(root, "/setup/company", form); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	if r := m.do("GET", "/setup/nope", nil, root.with()...); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown step: %d", r.StatusCode)
	}
	if r := m.post(root, "/setup/nope", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown post step: %d", r.StatusCode)
	}
}

func TestStarterTemplateSelectsDeployableModulesAndExplainsTheRest(t *testing.T) {
	m := newMM(t)
	root := m.loginAs("root")
	page := m.do("GET", "/setup/template", nil, root.with()...)
	if !strings.Contains(page.body, "desk is left out: not deployable yet") || !strings.Contains(page.body, "<code>solo</code>") {
		t.Fatalf("%s", page.body)
	}
	if r := m.post(root, "/setup/template", url.Values{"template": {"starter"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d", r.StatusCode)
	}
	ws, _ := m.svc.WizardState(context.Background())
	if strings.Join(ws.Data.Modules, ",") != "solo,web" || ws.Data.Template != "starter" {
		t.Fatalf("%+v", ws.Data)
	}
	// a template never switches a module off
	m.post(root, "/setup/template", url.Values{"template": {"blank"}})
	ws, _ = m.svc.WizardState(context.Background())
	if strings.Join(ws.Data.Modules, ",") != "web" {
		t.Fatalf("%+v", ws.Data)
	}
	if r := m.post(root, "/setup/template", url.Values{"template": {"nope"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("%d", r.StatusCode)
	}
	if r := m.post(root, "/setup/modules", url.Values{"enable": {"web", "desk"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a not-deployable module was saved: %d", r.StatusCode)
	}
	if r := m.post(root, "/setup/modules", url.Values{"enable": {"web", "ghost"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown module was saved: %d", r.StatusCode)
	}
}

func TestEmailRelayPasswordIsNeverEchoedStoredOrAudited(t *testing.T) {
	m := newMM(t)
	root := m.loginAs("root")
	const pw = "Zx9-very-secret-relay-password"
	r := m.post(root, "/setup/email", url.Values{"host": {"smtp.relay.example"}, "port": {"587"}, "username": {"apikey"}, "password": {pw}, "from": {"hello@acme.example"}})
	if r.StatusCode != http.StatusSeeOther || strings.Contains(r.body, pw) {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	if got := m.secrets.Get("can-system", "can-smtp-relay"); got["password"] != pw || got["host"] != "smtp.relay.example" {
		t.Fatalf("%v", got)
	}
	for _, step := range []string{"company", "template", "modules", "email", "dns", "account", "review"} {
		if p := m.do("GET", "/setup/"+step, nil, root.with()...); strings.Contains(p.body, pw) {
			t.Fatalf("password echoed on %s", step)
		}
	}
	if p := m.do("GET", "/setup/email", nil, root.with()...); !strings.Contains(p.body, "A relay is saved: smtp.relay.example port 587") || strings.Contains(p.body, `name="password" type="password" maxlength="1024" autocomplete="new-password" required value`) {
		t.Fatalf("%s", p.body)
	}
	recs, _ := m.audit.Query(context.Background(), audit.Filter{Limit: 300})
	for _, rc := range recs {
		if strings.Contains(rc.Detail, pw) || strings.Contains(rc.Target, pw) {
			t.Fatalf("password in the audit log: %+v", rc)
		}
	}
	var blob string
	must(t, m.db.QueryRowContext(context.Background(), `SELECT data_json FROM wizard_state`).Scan(&blob))
	if strings.Contains(blob, pw) {
		t.Fatal("password in the wizard state")
	}
	// a failing Secret write is an error, not a silent success, and shows no detail
	m.secrets.Err = errors.New("kube says: password " + pw)
	bad := m.post(root, "/setup/email", url.Values{"host": {"smtp.relay.example"}, "port": {"587"}, "password": {pw}, "from": {"hello@acme.example"}})
	if bad.StatusCode != http.StatusBadGateway || strings.Contains(bad.body, pw) {
		t.Fatalf("%d %s", bad.StatusCode, bad.body)
	}
	for _, form := range []url.Values{
		{"host": {"smtp://x"}, "port": {"587"}, "password": {"p"}, "from": {"a@b.example"}},
		{"host": {"smtp.example"}, "port": {"99999"}, "password": {"p"}, "from": {"a@b.example"}},
		{"host": {"smtp.example"}, "port": {"587"}, "password": {""}, "from": {"a@b.example"}},
		{"host": {"smtp.example"}, "port": {"587"}, "password": {"p"}, "from": {"nope"}},
	} {
		m.secrets.Err = nil
		if r := m.post(root, "/setup/email", form); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%v: %d", form, r.StatusCode)
		}
	}
}

func TestDNSAndAccountSteps(t *testing.T) {
	m := newMM(t)
	root := m.loginAs("root")
	d := m.do("GET", "/setup/dns", nil, root.with()...)
	for _, want := range []string{"acme.example", "admin.acme.example", "forms.acme.example", "203.0.113.7", "https://203-0-113-7.sslip.io", "Answering", "volunteer-run"} {
		if !strings.Contains(d.body, want) {
			t.Errorf("dns step lacks %q", want)
		}
	}
	m.svc.Net = nil
	if d := m.do("GET", "/setup/dns", nil, root.with()...); !strings.Contains(d.body, "public address of this server is not known") {
		t.Errorf("unknown address not stated")
	}
	a := m.do("GET", "/setup/account", nil, root.with()...)
	if !strings.Contains(a.body, "can backup kit") || strings.Count(a.body, "To do") < 2 {
		t.Fatalf("%s", a.body)
	}
	if _, err := m.audit.Append(context.Background(), audit.Entry{Actor: "root", Action: "admin.password.change", Target: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.audit.Append(context.Background(), audit.Entry{Actor: "root", Action: "admin.totp.rotate", Target: "admin"}); err != nil {
		t.Fatal(err)
	}
	a = m.do("GET", "/setup/account", nil, root.with()...)
	if strings.Count(a.body, `badge ok">Done`) < 2 {
		t.Fatalf("hardening not detected from the audit log:\n%s", a.body)
	}
	m.post(root, "/setup/account", url.Values{"kit": {"on"}})
	if ws, _ := m.svc.WizardState(context.Background()); !ws.Data.KitAck {
		t.Fatal("kit acknowledgement not saved")
	}
}

func TestWizardReviewAndApplyIsIdempotent(t *testing.T) {
	m := newMM(t)
	root := m.loginAs("root")
	m.post(root, "/setup/company", url.Values{"name": {"Acme Ltd"}, "domain": {"acme.example"}, "server_ram_mb": {"2048"}})
	m.post(root, "/setup/modules", url.Values{"enable": {"web", "solo"}})
	rev := m.do("GET", "/setup/review", nil, root.with()...)
	for _, want := range []string{"Acme Ltd", "mod-solo", `action="/setup/apply"`, "Install"} {
		if !strings.Contains(rev.body, want) {
			t.Errorf("review lacks %q", want)
		}
	}
	if string(mustRead(t, m.store.Path)) != mmCompany {
		t.Fatal("review wrote company.yaml")
	}
	a1 := m.post(root, "/setup/apply", url.Values{})
	if a1.StatusCode != http.StatusSeeOther || !strings.HasPrefix(a1.Header.Get("Location"), "/runs/") {
		t.Fatalf("%d %s %s", a1.StatusCode, a1.Header.Get("Location"), a1.body)
	}
	calls := len(m.fk.CallLog())
	a2 := m.post(root, "/setup/apply", url.Values{})
	if a2.Header.Get("Location") != a1.Header.Get("Location") || len(m.fk.CallLog()) != calls {
		t.Fatalf("second apply did work: %s vs %s", a2.Header.Get("Location"), a1.Header.Get("Location"))
	}
	if !strings.Contains(string(mustRead(t, m.store.Path)), "name: Acme Ltd") || m.fk.EnabledCount() != 2 {
		t.Fatalf("%s", mustRead(t, m.store.Path))
	}
	if ws, _ := m.svc.WizardState(context.Background()); ws.Status != modmgr.WizardCompleted {
		t.Fatalf("%+v", ws)
	}
	if d := m.do("GET", "/", nil, root.with()...); d.StatusCode != 200 {
		t.Fatalf("dashboard still redirects after completion: %d", d.StatusCode)
	}
	if r := m.post(root, "/setup/company", url.Values{"name": {"X"}, "domain": {"x.example"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a completed wizard accepted an edit: %d", r.StatusCode)
	}
}

func TestWizardReviewRefusesWhatCannotWork(t *testing.T) {
	m := newMM(t)
	root := m.loginAs("root")
	m.post(root, "/setup/modules", url.Values{"enable": {"web", "alt"}})
	rev := m.do("GET", "/setup/review", nil, root.with()...)
	if !strings.Contains(rev.body, "cannot be applied") || strings.Contains(rev.body, `action="/setup/apply"`) {
		t.Fatalf("a conflicting selection can be applied:\n%s", rev.body)
	}
	if r := m.post(root, "/setup/apply", url.Values{}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("%d", r.StatusCode)
	}
	if string(mustRead(t, m.store.Path)) != mmCompany {
		t.Fatal("company.yaml changed")
	}
}

// --- golden pages and the accessibility checklist -------------------------------------------------

var (
	reID  = regexp.MustCompile(`/(changes|runs)/[0-9a-f]{24}`)
	reCSV = regexp.MustCompile(`Request id: <code>[0-9a-f]+</code>`)
)

func normaliseMM(s string) string {
	s = csrfRe.ReplaceAllString(s, `name="csrf" value="CSRF"`)
	s = reID.ReplaceAllString(s, "/$1/ID")
	s = reCSV.ReplaceAllString(s, `Request id: <code>REQID</code>`)
	return reSecret.ReplaceAllString(s, "SECRET")
}

func renderModmgmt(t *testing.T) map[string]string {
	t.Helper()
	m := newMM(t)
	root := m.loginAs("root")
	out := map[string]string{}
	get := func(name, path string, status int) {
		t.Helper()
		r := m.do("GET", path, nil, root.with()...)
		if r.StatusCode != status {
			t.Fatalf("%s: %d, want %d\n%s", name, r.StatusCode, status, r.body)
		}
		out[name] = normaliseMM(r.body)
	}
	get("catalogue", "/catalogue", 200)
	get("catalogue-module-web", "/catalogue/web", 200)
	get("catalogue-module-crm", "/catalogue/crm", 200)
	get("catalogue-module-evil", "/catalogue/evil", 200)
	get("catalogue-module-solo", "/catalogue/solo", 200)
	get("catalogue-module-missing", "/catalogue/zzz", 404)
	get("company", "/company", 200)
	get("status", "/status", 200)

	bad := m.post(root, "/catalogue/web/settings", url.Values{"s.site_title": {`x"><i>`}, "s.max_rows": {"1000"}, "s.captcha": {"none"}})
	out["settings-invalid"] = normaliseMM(bad.body)

	ch := m.propose(root, []string{"web", "solo"}).Header.Get("Location")
	get("change-enable", ch, 200)
	dis := m.propose(root, nil).Header.Get("Location") // would disable web
	get("change-disable", dis, 200)
	set := m.post(root, "/catalogue/web/settings", url.Values{"s.site_title": {"Hello"}, "s.max_rows": {"7"}, "s.captcha": {"turnstile"}}).Header.Get("Location")
	get("change-settings", set, 200)
	get("change-missing", "/changes/nope", 404)
	run := m.post(root, ch+"/apply", url.Values{}).Header.Get("Location")
	get("run-succeeded", run, 200)
	get("change-applied", ch, 200)
	get("change-stale", dis, 200)
	get("status-after-apply", "/status", 200)

	// a failing install, and a run that is still going
	failCh := m.propose(root, []string{"web", "solo", "alt"}) // alt conflicts with web: refused
	out["propose-conflict"] = normaliseMM(failCh.body)

	get("purge-enabled-module", "/catalogue/web/purge", 409)
	m.svc.Purger = nil
	out["purge-unavailable"] = normaliseMM(forceDisabledPurgePage(t, m, root))
	m.svc.Purger = m.purger
	get("purge", "/catalogue/solo/purge", 200)

	for _, st := range []string{"company", "template", "modules", "email", "dns", "account", "review"} {
		get("setup-"+st, "/setup/"+st, 200)
	}
	m.post(root, "/setup/email", url.Values{"host": {"smtp.relay.example"}, "port": {"587"}, "username": {"apikey"}, "password": {"pw-123"}, "from": {"hello@acme.example"}})
	get("setup-email-configured", "/setup/email", 200)
	out["setup-company-invalid"] = normaliseMM(m.post(root, "/setup/company", url.Values{"name": {"A"}, "domain": {"bad domain"}}).body)
	m.post(root, "/setup/template", url.Values{"template": {"starter"}})
	get("setup-template-selected", "/setup/template", 200)
	m.post(root, "/setup/apply", url.Values{})
	get("setup-review-completed", "/setup/review", 200)
	get("setup-company-completed", "/setup/company", 200)

	// a run that is still going, then the same run failed with a hostile error text
	g := newMM(t)
	gs := g.loginAs("root")
	gate := make(chan struct{})
	var wg sync.WaitGroup
	g.svc.Runner = &gateRunner{Reconciling: g.svc.Runner, gate: gate}
	g.svc.Async = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	g.fk.FailEnable["solo"] = errors.New("helm: pull https://u:pw@reg.example failed: password=hunter2 <script>alert(1)</script>")
	loc := g.post(gs, g.propose(gs, []string{"web", "solo"}).Header.Get("Location")+"/apply", url.Values{}).Header.Get("Location")
	out["run-running"] = normaliseMM(g.do("GET", loc, nil, gs.with()...).body)
	close(gate)
	wg.Wait()
	out["run-failed"] = normaliseMM(g.do("GET", loc, nil, gs.with()...).body)
	out["status-failed"] = normaliseMM(g.do("GET", "/status", nil, gs.with()...).body)

	// status with nothing configured behind it
	n := newMM(t)
	n.svc.Ingress, n.svc.Disk = nil, nil
	nr := n.loginAs("root")
	out["status-unavailable"] = normaliseMM(n.do("GET", "/status", nil, nr.with()...).body)
	return out
}

// forceDisabledPurgePage disables solo and renders its purge page while no executor exists.
func forceDisabledPurgePage(t *testing.T, m *mm, s session) string {
	t.Helper()
	r := m.propose(s, []string{"web"})
	if r.StatusCode == http.StatusSeeOther {
		m.do("POST", r.Header.Get("Location")+"/apply", url.Values{"csrf": {s.csrf}}, s.with()...)
	}
	return m.do("GET", "/catalogue/solo/purge", nil, s.with()...).body
}

func TestGoldenModmgmtPages(t *testing.T) {
	pages := renderModmgmt(t)
	dir := filepath.Join("testdata", "golden")
	if *update {
		_ = os.MkdirAll(dir, 0o755)
	}
	for name, got := range pages {
		file := filepath.Join(dir, "mm-"+name+".html")
		if *update {
			must(t, os.WriteFile(file, []byte(got), 0o644))
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: no golden file (run go test ./core/server/adminui -run Golden -update): %v", name, err)
			continue
		}
		if strings.TrimSpace(string(want)) != strings.TrimSpace(got) {
			t.Errorf("%s differs from %s; run `go test ./core/server/adminui -run Golden -update` and review the diff", name, file)
		}
	}
}

func TestModmgmtPagesPassStaticAccessibilityChecklist(t *testing.T) {
	pages := renderModmgmt(t)
	if len(pages) < 30 {
		t.Fatalf("only %d pages rendered", len(pages))
	}
	for name, html := range pages {
		for _, p := range a11yProblems(html) {
			t.Errorf("%s: %s", name, p)
		}
		if strings.Contains(html, "<script") {
			t.Errorf("%s: contains a script element", name)
		}
	}
}

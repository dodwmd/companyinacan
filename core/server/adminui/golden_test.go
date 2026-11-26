package adminui

import (
	"errors"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden HTML files")

type page struct {
	name   string
	method string
	path   string
	form   url.Values
	status int
	anon   bool
}

var (
	reRequestID = regexp.MustCompile(`Request id: <code>[0-9a-f]+</code>`)
	reSID       = regexp.MustCompile(`/settings/sessions/[0-9a-f]{16}/revoke`)
	reSecret    = regexp.MustCompile(`[A-Z2-7]{16,}`)
)

func normalise(s string) string {
	s = csrfRe.ReplaceAllString(s, `name="csrf" value="CSRF"`)
	s = reRequestID.ReplaceAllString(s, `Request id: <code>REQID</code>`)
	s = reSID.ReplaceAllString(s, `/settings/sessions/SID/revoke`)
	s = reSecret.ReplaceAllString(s, "SECRET")
	return s
}

// renderAll drives every page of the UI through the real handlers over fixed fixtures and returns
// the normalised HTML by page name.
func renderAll(t *testing.T) (map[string]string, []page) {
	t.Helper()
	h := newHarness(t)
	h.seed()
	dead := h.killDelivery()
	h.leads.leads = []Lead{
		{ID: "7", FormID: "contact", Status: "new", Email: "ann@example.com", Name: "Ann", Note: "Call back", Fields: map[string]any{"message": "Hello there", "budget": 5000}, CreatedAt: fixedNow, UpdatedAt: fixedNow},
		{ID: "8", FormID: "contact", Status: "closed", Email: "bob@example.com", Name: "Bob", CreatedAt: fixedNow, UpdatedAt: fixedNow},
	}
	s := h.login()
	id := itoa(dead)
	pages := []page{
		{name: "login", method: "GET", path: "/login", status: 200, anon: true},
		{name: "dashboard", method: "GET", path: "/dashboard", status: 200},
		{name: "modules", method: "GET", path: "/modules", status: 200},
		{name: "module-forms", method: "GET", path: "/modules/forms", status: 200},
		{name: "module-wiki", method: "GET", path: "/modules/wiki", status: 200},
		{name: "audit", method: "GET", path: "/audit", status: 200},
		{name: "audit-filtered-empty", method: "GET", path: "/audit?actor=nobody", status: 200},
		{name: "audit-verify", method: "POST", path: "/audit/verify", form: url.Values{"csrf": {s.csrf}}, status: 200},
		{name: "events", method: "GET", path: "/events", status: 200},
		{name: "event-detail", method: "GET", path: "/events/" + id, status: 200},
		{name: "leads", method: "GET", path: "/leads", status: 200},
		{name: "lead-detail", method: "GET", path: "/leads/7", status: 200},
		{name: "settings", method: "GET", path: "/settings", status: 200},
		{name: "settings-totp", method: "POST", path: "/settings/totp", form: url.Values{"csrf": {s.csrf}, "current": {h.pw}}, status: 200},
		{name: "settings-password-error", method: "POST", path: "/settings/password", form: url.Values{"csrf": {s.csrf}, "current": {h.pw}, "otp": {"123456"}, "new": {"a new long password"}, "confirm": {"nope"}}, status: 400},
		{name: "not-found", method: "GET", path: "/nope", status: 404},
		{name: "module-not-found", method: "GET", path: "/modules/zzz", status: 404},
		{name: "requeue-conflict", method: "POST", path: "/events/" + id + "/requeue", form: url.Values{"csrf": {s.csrf}}, status: 303},
	}
	out := map[string]string{}
	for _, p := range pages {
		var cs = s.with()
		if p.anon {
			cs = nil
		}
		r := h.do(p.method, p.path, p.form, cs...)
		if r.StatusCode != p.status {
			t.Fatalf("%s: status %d, want %d\n%s", p.name, r.StatusCode, p.status, r.body)
		}
		if r.StatusCode == 200 || r.StatusCode >= 400 {
			out[p.name] = normalise(r.body)
		}
	}
	// pages that need other fixture states
	out["dashboard-after-requeue"] = normalise(h.do("GET", "/dashboard", nil, s.with()...).body)
	h.leads.err = ErrLeadsUnavailable
	out["leads-unavailable"] = normalise(h.do("GET", "/leads", nil, s.with()...).body)
	h.leads.err = errors.New("boom")
	out["error-gateway"] = normalise(h.do("GET", "/leads", nil, s.with()...).body)
	// a clean database: empty states
	e := newHarness(t)
	es := e.login()
	out["dashboard-empty"] = normalise(e.do("GET", "/dashboard", nil, es.with()...).body)
	out["modules-empty"] = normalise(e.do("GET", "/modules", nil, es.with()...).body)
	out["events-empty"] = normalise(e.do("GET", "/events", nil, es.with()...).body)
	out["login-failed"] = func() string {
		c, tok := e.loginForm()
		return normalise(e.do("POST", "/login", url.Values{"username": {"root"}, "password": {"wrong password!!"}, "totp": {"000000"}, "csrf": {tok}}, c).body)
	}()
	return out, pages
}

func TestGoldenPages(t *testing.T) {
	pages, _ := renderAll(t)
	dir := filepath.Join("testdata", "golden")
	if *update {
		_ = os.MkdirAll(dir, 0o755)
	}
	for name, got := range pages {
		file := filepath.Join(dir, name+".html")
		if *update {
			if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: no golden file (run go test -run Golden -update): %v", name, err)
			continue
		}
		if strings.TrimSpace(string(want)) != strings.TrimSpace(got) {
			t.Errorf("%s differs from %s; run `go test ./core/server/adminui -run Golden -update` and review the diff", name, file)
		}
	}
}

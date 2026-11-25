package wiring

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/ticket"
	"github.com/dodwmd/companyinacan/core/work"
)

// Single sign-on from the admin UI into a module (specs/036-module-ui-sso) with the real ticket
// service, module API, module client, manifests and tracker: the fake can-core of the module tests
// cannot drift from these without this failing.
func TestLaunchTicketSignsInToTheRealTracker(t *testing.T) {
	w := newWorld(t)
	tickets := &ticket.Service{DB: w.db, Audit: audit.New(w.db), Holds: w.pats.Holds,
		Launcher: ticket.Launcher{Domain: "example.org", Overrides: map[string]string{
			"work": "https://work.example.org", "people": "https://people.example.org", "jobs": "https://jobs.example.org", "finance": "https://finance.example.org"}}}
	core := httptest.NewServer((&moduleapi.Server{Auth: modauth.New(w.tr, w.db), Registry: w.reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Tickets: tickets}).Handler())
	t.Cleanup(core.Close)
	for _, id := range []string{"people", "jobs", "finance"} {
		w.register(id)
	}
	_, can, _ := w.register("work")
	can.BaseURL = core.URL

	cfg := work.DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "work.db")
	app, err := work.NewApp(cfg, work.Secrets{AdminToken: "admin-token-0123456789abcdefABCDEF-xyz"},
		work.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Can: can})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	module := httptest.NewServer(app.Handler())
	t.Cleanup(module.Close)

	// The first administrator is the owner; the person under test is a second one with a role.
	for _, u := range []string{"root", "alice"} {
		if _, err := w.db.CreateAdmin(w.ctx, u, "x", "y"); err != nil {
			t.Fatal(err)
		}
	}
	w.grant("alice", "work:member")

	// Every in-house manifest declares web_login, so each module the person may use is listed.
	apps, err := tickets.Apps(w.ctx, "user:alice")
	if err != nil || len(apps) != 1 || apps[0].ID != "work" {
		t.Fatalf("alice's apps: %+v %v", apps, err)
	}
	if all, err := tickets.Apps(w.ctx, "user:root"); err != nil || len(all) == 0 {
		t.Fatalf("root's apps: %+v %v", all, err)
	}

	launched, err := tickets.Launch(w.ctx, "user:alice", "work", "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(launched.URL)
	if err != nil || u.Host != "work.example.org" || u.Path != "/ui/login/ticket" {
		t.Fatalf("launch URL %q: the work manifest's base path must be applied", launched.URL)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	read := func(resp *http.Response, err error) (*http.Response, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, page := read(browser.Get(module.URL + u.Path + "?" + u.RawQuery))
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
	if resp.StatusCode != 200 || csrf == nil {
		t.Fatalf("interstitial %d %.300s", resp.StatusCode, page)
	}
	form := url.Values{"csrf": {csrf[1]}, "ticket": {u.Query().Get("t")}}
	resp, page = read(browser.PostForm(module.URL+"/ui/login/ticket", form))
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/ui/dashboard" {
		t.Fatalf("redeem %d %q %.300s", resp.StatusCode, resp.Header.Get("Location"), page)
	}
	resp, page = read(browser.Get(module.URL + "/ui/dashboard"))
	if resp.StatusCode != 200 || !strings.Contains(page, "alice") {
		t.Fatalf("signed-in page %d %.300s", resp.StatusCode, page)
	}
	// Replayed in another browser: nothing.
	other, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	second := &http.Client{Jar: other, CheckRedirect: browser.CheckRedirect}
	_, page = read(second.Get(module.URL + u.Path + "?" + u.RawQuery))
	csrf = regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
	resp, _ = read(second.PostForm(module.URL+"/ui/login/ticket", url.Values{"csrf": {csrf[1]}, "ticket": {u.Query().Get("t")}}))
	if resp.StatusCode != 401 {
		t.Fatalf("replay %d", resp.StatusCode)
	}
}

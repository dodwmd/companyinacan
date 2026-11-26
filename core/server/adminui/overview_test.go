package adminui

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/overview"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
)

type providers []registry.Provider

func (p providers) Resolve(context.Context, string) (registry.Resolution, error) {
	return registry.Resolution{Providers: p}, nil
}

// newOverviewUI: finance answers, work is down; carol may see both modules and the platform, alice
// only work, dave nothing.
func newOverviewUI(t *testing.T) (*pui, *int) {
	t.Helper()
	p := newPUI(t)
	ctx := context.Background()
	for _, id := range []string{"finance", "work"} {
		m := config.Module{ID: id, Name: strings.ToUpper(id[:1]) + id[1:], Version: "1.0.0", Permissions: []string{id + ".read"}}
		m.Runtime.Subdomain = id
		m.Runtime.WebLogin = &config.WebLogin{ViaSubdomain: true}
		if err := p.db.UpsertModule(ctx, store.Module{ID: id, Version: "1.0.0", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s", Manifest: m}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	asked := new(int)
	fin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*asked++
		_, _ = w.Write([]byte(`{"version":1,"figures":[{"key":"invoices.overdue","count":3,"amounts":[{"currency":"AUD","minor":123456},{"currency":"JPY","minor":900}]},
			{"key":"invoices.outstanding","count":5,"amounts":[{"currency":"AUD","minor":999900}]},{"key":"contracts.expiring","count":0}]}`))
	}))
	t.Cleanup(fin.Close)
	rights := map[string]bool{"user:carol finance.read": true, "user:carol work.read": true, "user:alice work.read": true}
	tickets := &ticket.Service{DB: p.db, Audit: p.audit, Now: func() time.Time { return *p.now }, Launcher: ticket.Launcher{Domain: "example.org"},
		Holds: func(_ context.Context, subject, perm string) (bool, error) { return rights[subject+" "+perm], nil }}
	p.ui.Tickets = tickets
	p.ui.Overview = &overview.Service{Registry: providers{{ModuleID: "finance", URL: fin.URL}, {ModuleID: "work", URL: "http://127.0.0.1:1"}},
		DB: p.db, Grants: tickets, Now: p.ui.Now, Log: p.ui.Log}
	p.ui.Mod = &modmgr.Service{LastDump: func() (backup.DumpRun, error) { return backup.DumpRun{}, fs.ErrNotExist }}
	cli := rbac.New(p.db, p.audit, rbac.Options{BreakGlass: true})
	if err := cli.Grant(ctx, rbac.CLIActor, "user:carol", "auditor", ""); err != nil {
		t.Fatal(err)
	}
	return p, asked
}

func TestOverviewShowsEachPersonOnlyWhatTheyMaySee(t *testing.T) {
	p, asked := newOverviewUI(t)
	carol, alice, dave := p.loginAs("carol"), p.loginAs("alice"), p.loginAs("dave")
	get := func(s session) string {
		t.Helper()
		r := p.do("GET", "/overview", nil, s.with()...)
		if r.StatusCode != 200 {
			t.Fatalf("%d\n%s", r.StatusCode, r.body)
		}
		if probs := a11yProblems(r.body); len(probs) > 0 {
			t.Errorf("accessibility problems: %v", probs)
		}
		return r.body
	}

	c := get(carol)
	for _, want := range []string{"Needs attention", "Overdue invoices", "AUD 1234.56", "JPY 900", "AUD 9999.00", "Receivables",
		`action="/apps/finance/launch"`, `name="return" value="/invoices"`, "Work is unavailable", "Backups", "No dump has run yet", `href="/status"`, "Approvals waiting for you"} {
		if !strings.Contains(c, want) {
			t.Errorf("carol's overview lacks %q", want)
		}
	}
	// A zero count needs no attention.
	attention := c[strings.Index(c, `id="h-attention"`):strings.Index(c, `id="h-ov-approvals"`)]
	if !strings.Contains(attention, "Overdue invoices") || strings.Contains(attention, "Contracts ending") || strings.Contains(attention, "Receivables") {
		t.Errorf("needs attention:\n%s", attention)
	}

	a := get(alice)
	d := get(dave)
	for name, body := range map[string]string{"alice": a, "dave": d} {
		for _, never := range []string{"Overdue invoices", "AUD", "Receivables", "Finance", "Backups"} {
			if strings.Contains(body, never) {
				t.Errorf("%s sees %q", name, never)
			}
		}
	}
	if !strings.Contains(a, "Work is unavailable") || strings.Contains(d, "Work") {
		t.Errorf("alice has a work right and dave none:\n%s", d)
	}
	if *asked != 1 {
		t.Errorf("finance was asked %d times: only for the one who may see it", *asked)
	}
	if r := p.do("GET", "/overview", nil); r.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous: %d", r.StatusCode)
	}
}

func TestOverviewBackupStates(t *testing.T) {
	p, _ := newOverviewUI(t)
	carol := p.loginAs("carol")
	for want, run := range map[string]backup.DumpRun{
		"Succeeded at":  {OK: true, At: *p.now},
		"FAILED at":     {At: *p.now},
		"Overdue: the":  {OK: true, At: p.now.Add(-48 * time.Hour)},
		"without: wiki": {OK: true, At: *p.now, Skipped: []backup.SkippedModule{{ID: "wiki"}}},
	} {
		p.ui.Mod.LastDump = func() (backup.DumpRun, error) { return run, nil }
		if r := p.do("GET", "/overview", nil, carol.with()...); !strings.Contains(r.body, want) {
			t.Errorf("no %q:\n%s", want, r.body)
		}
	}
}

func TestOverviewWithoutModuleServicesShowsWhatCanCoreKnows(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	r := h.do("GET", "/overview", nil, s.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "Nothing needs your attention") {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
}

func TestEveryOverviewFigureHasALabel(t *testing.T) {
	for _, f := range overview.Figures {
		if _, ok := messagesEN["ov.fig_"+strings.ReplaceAll(f.Key, ".", "_")]; !ok {
			t.Errorf("no label for %s", f.Key)
		}
	}
}

func TestSignInLandsOnTheOverview(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	r := h.do("GET", "/", nil, s.with()...)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/overview" {
		t.Fatalf("%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// A known flash message follows; anything else in the query does not.
	if r := h.do("GET", "/?flash=flash.wizard_done", nil, s.with()...); r.Header.Get("Location") != "/overview?flash=flash.wizard_done" {
		t.Errorf("%s", r.Header.Get("Location"))
	}
	if r := h.do("GET", "/?flash=//evil.example", nil, s.with()...); r.Header.Get("Location") != "/overview" {
		t.Errorf("%s", r.Header.Get("Location"))
	}
	page := h.do("GET", "/overview", nil, s.with()...)
	if !strings.Contains(page.body, `href="/dashboard"`) {
		t.Error("the dashboard is not reachable from the overview")
	}
	if r := h.do("GET", "/dashboard", nil, s.with()...); r.StatusCode != http.StatusOK {
		t.Errorf("dashboard: %d", r.StatusCode)
	}
}

func TestNothingNeedsAttentionOnlyWhenEverythingWasRead(t *testing.T) {
	p, _ := newOverviewUI(t)
	// alice may see work only, and work is down.
	alice := p.loginAs("alice")
	body := p.do("GET", "/overview", nil, alice.with()...).body
	if strings.Contains(body, "Nothing needs your attention") || !strings.Contains(body, "may be incomplete") {
		t.Errorf("with a module unavailable:\n%s", body)
	}
	// dave has nothing to see and nothing is missing.
	dave := p.loginAs("dave")
	body = p.do("GET", "/overview", nil, dave.with()...).body
	if !strings.Contains(body, "Nothing needs your attention") || strings.Contains(body, "may be incomplete") {
		t.Errorf("with nothing missing:\n%s", body)
	}
}

func TestUnreadableRightsShowUnavailableOnTheOverview(t *testing.T) {
	p, asked := newOverviewUI(t)
	carol := p.loginAs("carol")
	p.ui.Tickets.Holds = func(context.Context, string, string) (bool, error) { return false, errors.New("database is locked") }
	r := p.do("GET", "/overview", nil, carol.with()...)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("%d", r.StatusCode)
	}
	for _, want := range []string{"Finance is unavailable", "Work is unavailable", "may be incomplete"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if *asked != 0 {
		t.Error("a module was asked without knowing the person's rights")
	}
}

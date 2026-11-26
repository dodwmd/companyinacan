package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func shipped(t *testing.T, id string) *config.Module {
	t.Helper()
	doc, ps := config.Load("../../../modules/" + id + "/module.yaml")
	if doc == nil {
		t.Fatal(ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func providers(ls []config.Link) string {
	var s []string
	for _, l := range ls {
		s = append(s, l.Provider)
	}
	return strings.Join(s, ",")
}

func reasonOf(as []Action, id string) string {
	for _, a := range as {
		if a.Module == id {
			return a.Reason
		}
	}
	return ""
}

// directory writes company.yaml's opt-in into d.
func directory(d Desired) Desired {
	e := d.Company.Modules["equity"]
	e.Settings = map[string]any{"identity": "directory"}
	d.Company.Modules["equity"] = e
	return d
}

// equity stays in attestation mode until company.yaml asks for directory mode, and the plan says
// what each change does before it happens.
func TestEquityDirectoryModeIsOptIn(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	equity, people := shipped(t, "equity"), shipped(t, "people")
	apply := func(d Desired) {
		t.Helper()
		if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
			t.Fatal(err)
		}
		if again, _ := g.r.Plan(ctx, d); len(again) != 0 {
			t.Fatalf("not settled: %+v", again)
		}
	}
	connected := func() string {
		t.Helper()
		s, ok := g.fk.EnabledSpec("equity")
		if !ok {
			t.Fatal("equity is not running")
		}
		rec, _ := g.db.GetModule(ctx, "equity")
		if providers(rec.Manifest.Runtime.Wired) != providers(s.Links) {
			t.Fatalf("recorded %+v, installed %+v", rec.Manifest.Runtime.Wired, s.Links)
		}
		return providers(s.Links)
	}

	apply(desired(nil, nil, equity))
	// enabling people changes nothing for equity: new and existing installations stay in attestation
	both := desired(nil, nil, equity, people)
	plan, err := g.r.Plan(ctx, both)
	if err != nil || kinds(plan) != "install:people" {
		t.Fatalf("plan %q %v", kinds(plan), err)
	}
	apply(both)
	if got := connected(); got != "" {
		t.Fatalf("connected without the opt-in to %q", got)
	}

	optIn := directory(desired(nil, nil, equity, people))
	plan, _ = g.r.Plan(ctx, optIn)
	if r := reasonOf(plan, "equity"); kinds(plan) != "upgrade:equity" || !strings.Contains(r, "directory mode") || !strings.Contains(r, "verify-person") {
		t.Fatalf("%+v", plan)
	} else if len([]rune(r)) > 240 {
		t.Fatalf("the admin UI would truncate the reason (%d runes)", len([]rune(r)))
	}
	apply(optIn)
	if got := connected(); got != "people" {
		t.Fatalf("links %q", got)
	}
	if s, _ := g.fk.EnabledSpec("equity"); s.Links[0].Service != "people-api" || s.Links[0].Port != 8081 {
		t.Fatalf("links %+v", s.Links)
	}

	back := desired(nil, map[string]bool{"people": false}, equity, people)
	plan, _ = g.r.Plan(ctx, back)
	if kinds(plan) != "disable:people,upgrade:equity" || !strings.Contains(reasonOf(plan, "equity"), "attestation mode") {
		t.Fatalf("%+v", plan)
	}
	apply(back)
	if got := connected(); got != "" {
		t.Fatalf("still connected to %q", got)
	}
}

// What a company opted into is asked of the installer whatever the provider's state, so that the
// installer refuses rather than runs the module in the other mode. Without an opt-in, a provider
// that never ran connects nobody, and one that fails later takes no connection away.
func TestFailedProvider(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	equity, people, prov := shipped(t, "equity"), shipped(t, "people"), shipped(t, "provisioning")
	d := directory(desired(nil, nil, equity, people, prov))
	g.fk.FailEnable["people"] = errors.New("no image")
	if _, err := g.r.Apply(ctx, d, Options{}); err == nil {
		t.Fatal("people installed")
	}
	if s, _ := g.fk.EnabledSpec("equity"); providers(s.Links) != "people" {
		t.Fatalf("equity asked for without the connection it opted into: %+v", s.Links)
	}
	if s, _ := g.fk.EnabledSpec("provisioning"); len(s.Links) != 0 {
		t.Fatalf("connected to a module that never ran: %+v", s.Links)
	}
	delete(g.fk.FailEnable, "people")
	for range 2 {
		if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if s, _ := g.fk.EnabledSpec("provisioning"); providers(s.Links) != "people" {
		t.Fatalf("links %+v", s.Links)
	}
	if err := g.db.SetModuleState(ctx, "people", "failed", "broken later"); err != nil {
		t.Fatal(err)
	}
	g.fk.FailEnable["people"] = errors.New("still broken")
	if plan, _ := g.r.Plan(ctx, d); kinds(plan) != "install:people" {
		t.Fatalf("a failed provider changes its consumers: %+v", plan)
	}
}

func TestModeNotes(t *testing.T) {
	company := func(on bool, settings map[string]any) *config.Company {
		return &config.Company{Modules: map[string]config.ModuleEntry{"equity": {Enabled: &on, Settings: settings}}}
	}
	dir := map[string]any{"identity": "directory"}
	for name, c := range map[string]struct {
		was, next *config.Company
		want      string
	}{
		"opting in":       {company(true, nil), company(true, dir), "verify-person"},
		"first apply":     {nil, company(true, dir), "verify-person"},
		"opting out":      {company(true, dir), company(true, nil), "attestation mode"},
		"break-glass":     {company(true, nil), company(true, map[string]any{"break_glass_until": "2026-12-01"}), "break-glass until 2026-12-01"},
		"unchanged":       {company(true, dir), company(true, dir), ""},
		"equity disabled": {company(true, dir), company(false, nil), ""},
		"never opted in":  {nil, company(true, nil), ""},
	} {
		got := strings.Join(ModeNotes(c.was, c.next), "; ")
		if (c.want == "" && got != "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// Only the pairs can-core grants are connected, and only to a provider that is the capability's
// primary when one is designated.
func TestOnlyGrantedPairsAreConnected(t *testing.T) {
	equity, people, work := shipped(t, "equity"), shipped(t, "people"), shipped(t, "work")
	work.Consumes = append(work.Consumes, "directory")
	d := directory(desired(nil, nil, equity, people, work, shipped(t, "provisioning")))
	for id, want := range map[string]string{"equity": "people", "provisioning": "people", "work": "", "people": ""} {
		if got := providers(links(d, nil, id, true)); got != want {
			t.Errorf("%s -> %q, want %q", id, got, want)
		}
	}
	d.Company.Primary = map[string]string{"directory": "ldap"}
	if got := links(d, nil, "equity", true); len(got) != 0 {
		t.Errorf("connected to a provider that is not primary: %+v", got)
	}
	order := installOrder(d.enabled())
	if indexOf(order, "people") > indexOf(order, "equity") || indexOf(order, "people") > indexOf(order, "provisioning") {
		t.Errorf("order %v", order)
	}
}

func indexOf(l []string, v string) int {
	for i, x := range l {
		if x == v {
			return i
		}
	}
	return -1
}

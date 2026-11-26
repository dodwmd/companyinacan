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

// equity is installed alone, people is enabled later, then disabled again: each time the plan says
// what happens to equity before it happens.
func TestEquityFollowsPeople(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	equity, people := shipped(t, "equity"), shipped(t, "people")
	apply := func(d Desired) Result {
		t.Helper()
		res, err := g.r.Apply(ctx, d, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	spec := func(id string) Spec {
		s, ok := g.fk.EnabledSpec(id)
		if !ok {
			t.Fatalf("%s is not running", id)
		}
		return s
	}

	apply(desired(nil, nil, equity))
	if got := providers(spec("equity").Links); got != "" {
		t.Fatalf("equity without people is connected to %q", got)
	}

	both := desired(nil, nil, equity, people)
	plan, err := g.r.Plan(ctx, both)
	if err != nil || kinds(plan) != "install:people,upgrade:equity" {
		t.Fatalf("plan %q %v", kinds(plan), err)
	}
	if r := reasonOf(plan, "equity"); !strings.Contains(r, "directory mode") || !strings.Contains(r, "verify-person") {
		t.Fatalf("the plan does not say equity changes mode: %q", r)
	} else if len([]rune(r)) > 240 {
		t.Fatalf("the admin UI would truncate the reason (%d runes)", len([]rune(r)))
	}
	apply(both)
	if got := spec("equity").Links; providers(got) != "people" || got[0].Service != "people-api" || got[0].Port != 8081 {
		t.Fatalf("links %+v", got)
	}
	rec, _ := g.db.GetModule(ctx, "equity")
	if providers(rec.Manifest.Runtime.Wired) != "people" {
		t.Fatalf("recorded %+v", rec.Manifest.Runtime.Wired)
	}
	if again, _ := g.r.Plan(ctx, both); len(again) != 0 {
		t.Fatalf("not settled: %+v", again)
	}

	alone := desired(nil, map[string]bool{"people": false}, equity, people)
	plan, _ = g.r.Plan(ctx, alone)
	if kinds(plan) != "disable:people,upgrade:equity" || !strings.Contains(reasonOf(plan, "equity"), "attestation mode") {
		t.Fatalf("%+v", plan)
	}
	apply(alone)
	if got := providers(spec("equity").Links); got != "" {
		t.Fatalf("still connected to %q", got)
	}
	if again, _ := g.r.Plan(ctx, alone); len(again) != 0 {
		t.Fatalf("not settled: %+v", again)
	}
}

// A people module that cannot be installed connects nobody; one that fails later does not take the
// connection away (equity then fails closed rather than falling back to the weaker mode).
func TestFailedProviderDoesNotChangeTheConsumer(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	equity, people := shipped(t, "equity"), shipped(t, "people")
	both := desired(nil, nil, equity, people)

	g.fk.FailEnable["people"] = errors.New("no image")
	if _, err := g.r.Apply(ctx, both, Options{}); err == nil {
		t.Fatal("people installed")
	}
	if s, _ := g.fk.EnabledSpec("equity"); len(s.Links) != 0 {
		t.Fatalf("connected to a module that never ran: %+v", s.Links)
	}
	plan, _ := g.r.Plan(ctx, both)
	if kinds(plan) != "install:people" {
		t.Fatalf("plan %q", kinds(plan))
	}
	delete(g.fk.FailEnable, "people")
	for range 2 {
		if _, err := g.r.Apply(ctx, both, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if s, _ := g.fk.EnabledSpec("equity"); providers(s.Links) != "people" {
		t.Fatalf("links %+v", s.Links)
	}

	if err := g.db.SetModuleState(ctx, "people", "failed", "broken later"); err != nil {
		t.Fatal(err)
	}
	g.fk.FailEnable["people"] = errors.New("still broken")
	if plan, _ := g.r.Plan(ctx, both); kinds(plan) != "install:people" {
		t.Fatalf("a failed provider changes its consumer: %+v", plan)
	}
}

// Only the pairs can-core grants are connected, and only to a provider that is the capability's
// primary when one is designated.
func TestOnlyGrantedPairsAreConnected(t *testing.T) {
	equity, people, work := shipped(t, "equity"), shipped(t, "people"), shipped(t, "work")
	work.Consumes = append(work.Consumes, "directory")
	d := desired(nil, nil, equity, people, work, shipped(t, "provisioning"))
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

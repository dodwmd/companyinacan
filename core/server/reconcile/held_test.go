package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// heldRig has "work" installed and an upgrade of it waiting for approval; d is the desired state.
func heldRig(t *testing.T) (*rig, Desired) {
	t.Helper()
	g := newRig(t)
	g.r.RequireApproval = true
	apply(t, g, desired(nil, nil, mf("work", "1", nil)))
	next := mf("work", "2", nil)
	next.Permissions = []string{"work.export"}
	d := desired(nil, nil, next)
	if res := apply(t, g, d); len(res.Held) != 1 || len(res.Actions) != 0 {
		t.Fatalf("not held, or something to apply although nothing changed: %+v", res)
	}
	return g, d
}

// While an upgrade waits, rotation and exposure removal reach the running release, and the record of
// what is installed (manifest, content, held upgrade) stays.
func TestAHeldUpgradeDoesNotBlockRotationOrExposureRemoval(t *testing.T) {
	ctx := context.Background()
	g, d := heldRig(t)
	before, _ := g.db.GetModule(ctx, "work")
	if err := g.db.RequestRotation(ctx, "work", modsecrets.WebhookKey, "test"); err != nil {
		t.Fatal(err)
	}
	off := false
	d.Company.Modules["work"] = config.ModuleEntry{Expose: &config.Expose{Public: &off}}

	res := apply(t, g, d)
	if kinds(res.Actions) != "restrict:work" || len(res.Held) != 1 || !strings.Contains(res.Actions[0].Reason, "secret rotation") || !strings.Contains(res.Actions[0].Reason, "exposure") {
		t.Fatalf("%+v", res)
	}
	spec := g.fk.Restricted["work"]
	if spec.Manifest.Version != "1" || len(spec.Manifest.Permissions) != 0 || spec.Expose.PublicOn() || !slices.Equal(spec.Rotate, []string{modsecrets.WebhookKey}) {
		t.Fatalf("not given the installed release: %+v", spec)
	}
	after, _ := g.db.GetModule(ctx, "work")
	if spec.RecordSecret != before.HMACSecret || spec.HMACSecret == before.HMACSecret || after.HMACSecret != spec.HMACSecret {
		t.Fatal("the signing secret was not rotated")
	}
	if after.Version != "1" || after.Content != before.Content || after.Pending == "" {
		t.Fatalf("the record changed: %+v", after)
	}
	if pend, _ := g.db.PendingRotations(ctx, "work"); len(pend) != 0 {
		t.Fatalf("rotation still pending: %v", pend)
	}
	if res := apply(t, g, d); len(res.Actions) != 0 || len(res.Held) != 1 {
		t.Fatalf("applied again: %+v", res)
	}
}

func TestAHeldUpgradeDoesNotBlockMailRelayOptOut(t *testing.T) {
	g := newRig(t)
	g.r.RequireApproval = true
	var relay string
	for _, id := range []string{"crm", "support", "newsletter", "wiki", "status"} {
		if modsecrets.WantsRelay(id) {
			relay = id
			break
		}
	}
	if relay == "" {
		t.Fatal("no module takes the relay")
	}
	on := true
	with := func(version string, optIn bool) Desired {
		m := mf(relay, version, []string{"email.send"})
		if version != "1" {
			m.Permissions = []string{"x.new"}
		}
		d := desired(nil, nil, m)
		d.Company.Email = &config.Email{Relay: &config.EmailRelay{Host: "smtp.a.example"}, Modules: map[string]config.EmailModule{}}
		if optIn {
			e := d.Company.Email.Modules[relay]
			e.Enabled = &on
			d.Company.Email.Modules[relay] = e
		}
		return d
	}
	apply(t, g, with("1", true))
	if res := apply(t, g, with("2", true)); len(res.Actions) != 0 || len(res.Held) != 1 {
		t.Fatalf("%+v", res)
	}
	res := apply(t, g, with("2", false))
	if kinds(res.Actions) != "restrict:"+relay || RelayOptIn(g.fk.Restricted[relay].Email, relay) {
		t.Fatalf("%+v", res)
	}
}

type onlyInstaller struct{ ModuleInstaller }

func TestAFailedOrImpossibleRestrictionIsSaid(t *testing.T) {
	ctx := context.Background()
	g, d := heldRig(t)
	if err := g.db.RequestRotation(ctx, "work", modsecrets.WebhookKey, "test"); err != nil {
		t.Fatal(err)
	}
	g.fk.FailEnable["work"] = errors.New("no release record")
	if _, err := g.r.Apply(ctx, d, Options{}); err == nil {
		t.Fatal("a failed rotation was not reported")
	}
	rec, _ := g.db.GetModule(ctx, "work")
	if rec.State != store.StateEnabled || !strings.Contains(rec.LastError, "could not be applied to the running release") {
		t.Fatalf("%+v", rec)
	}
	g.r.Installer = onlyInstaller{g.fk}
	res, err := g.r.Apply(ctx, d, Options{})
	if err != nil || len(res.Actions) != 0 || len(res.Held) != 1 || !strings.Contains(res.Held[0].Reason, "NOT applied until you approve: secret rotation") {
		t.Fatalf("%+v %v", res, err)
	}
}

// setEgress sets runtime.internet_egress when this build's manifest has it.
func setEgress(m *config.Module, on bool) bool {
	v := reflect.ValueOf(&m.Runtime).Elem().FieldByName("InternetEgress")
	if !v.IsValid() {
		return false
	}
	v.SetBool(on)
	return true
}

// Before manifests had internet_egress every module reached the internet: a module that now declares
// it asks for nothing new.
func TestEgressIsNotNewForARecordThatPredatesTheField(t *testing.T) {
	declared := mf("crm", "1", nil)
	if !setEgress(declared, true) {
		// This build cannot declare egress, and records say nothing about it.
		if (Desired{Manifests: map[string]*config.Module{"crm": declared}}).content("crm").Egress != nil {
			t.Fatal("recorded something about egress")
		}
		t.Skip("this build's manifest has no runtime.internet_egress")
	}
	without, _ := json.Marshal(Content{Manifest: "x"})
	closed := false
	asClosed, _ := json.Marshal(Content{Manifest: "x", Egress: &closed})
	for name, c := range map[string]struct {
		recorded string
		want     bool
	}{"no content record": {"", false}, "content without egress": {string(without), false}, "installed without access": {string(asClosed), true}} {
		if got := newInternetAccess(c.recorded, declared); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}

	// End to end: a legacy record upgrades unheld; one installed without access waits.
	ctx := context.Background()
	g := newRig(t)
	g.r.RequireApproval = true
	old := mf("crm", "1", nil)
	apply(t, g, desired(nil, nil, old))
	if _, err := g.db.ExecContext(ctx, `UPDATE modules SET content_json=''`); err != nil {
		t.Fatal(err)
	}
	next := mf("crm", "2", nil)
	setEgress(next, true)
	if res := apply(t, g, desired(nil, nil, next)); len(res.Held) != 0 || kinds(res.Actions) != "upgrade:crm" {
		t.Fatalf("a record from before the field: %+v", res)
	}
	g2 := newRig(t)
	g2.r.RequireApproval = true
	apply(t, g2, desired(nil, nil, old))
	if res := apply(t, g2, desired(nil, nil, next)); len(res.Held) != 1 || !slices.Equal(res.Held[0].Widens, []string{"internet access"}) {
		t.Fatalf("installed without access: %+v", res)
	}
}

func TestWideningCoversApprovalsNotificationsHooksAndData(t *testing.T) {
	decode := func(y string) *config.Module {
		doc, ps := config.LoadBytes("module.yaml", []byte(y))
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	old := decode(`
approvals: [{type: expense, default_chain: [manager, finance]}, {type: grant, default_chain: [board]}]
notifications: [{event: a.v1, recipients: ["role:sales"]}]
hooks: {image: "ghcr.io/x/hooks:1", backup: ./b.sh}
data_classes: [{class: contact, retention: 3y, sensitivity: standard}]
`)
	next := decode(`
approvals: [{type: expense, default_chain: [manager]}]
notifications: [{event: a.v1, recipients: ["role:sales", "channel:slack"]}]
hooks: {image: "ghcr.io/y/hooks:1", command: /can-x, backup: backup}
runtime: {web_login: {base_path: ui}}
requires: {sso: oidc}
data_classes: [{class: contact, retention: 1y, sensitivity: sensitive}]
`)
	want := []string{
		"approval expense no longer needs finance",
		"hooks run in the image ghcr.io/y/hooks",
		"hooks run the command /can-x",
		"keeps data of class contact (sensitive) for 1y",
		"no longer declares the approval grant",
		"notifies channel:slack about a.v1",
		"runs the backup hook backup",
		"signs users into its web UI at /ui",
		"uses single sign-on: oidc",
	}
	if got := Widening(old, next); !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	newTag := decode(`hooks: {image: "ghcr.io/x/hooks:2", backup: ./b.sh}`)
	same := decode(`hooks: {image: "ghcr.io/x/hooks:1", backup: ./b.sh}`)
	if got := Widening(same, newTag); len(got) != 0 {
		t.Fatalf("a new tag of the hooks image is an update: %q", got)
	}
}

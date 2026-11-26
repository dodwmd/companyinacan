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

// heldRig has "work" installed with its web UI routed and an upgrade of it waiting for approval.
func heldRig(t *testing.T) (*rig, Desired) {
	t.Helper()
	g := newRig(t)
	g.r.RequireApproval = true
	exposed := func(d Desired, x config.Expose) Desired {
		d.Company.Modules["work"] = config.ModuleEntry{Expose: &x}
		return d
	}
	apply(t, g, exposed(desired(nil, nil, mf("work", "1", nil)), config.Expose{UI: true}))
	next := mf("work", "2", nil)
	next.Permissions = []string{"work.export"}
	d := exposed(desired(nil, nil, next), config.Expose{UI: true})
	res := apply(t, g, d)
	if len(res.Held) != 1 || len(res.Actions) != 0 || len(res.Held[0].NotApplied) != 0 || res.Held[0].Urgent {
		t.Fatalf("nothing was asked for: %+v", res)
	}
	return g, d
}

// A held module gets no change at all; each change asked of it is named, and taking something away
// is marked urgent.
func TestAHeldUpgradeNamesWhatIsNotApplied(t *testing.T) {
	ctx := context.Background()
	g, d := heldRig(t)
	before, _ := g.db.GetModule(ctx, "work")
	calls := len(g.fk.CallLog())
	if err := g.db.RequestRotation(ctx, "work", modsecrets.WebhookKey, "test"); err != nil {
		t.Fatal(err)
	}
	off := false
	d.Company.Modules["work"] = config.ModuleEntry{Expose: &config.Expose{Public: &off}, Settings: map[string]any{"a": 1}}

	res := apply(t, g, d)
	if len(res.Actions) != 0 || len(res.Held) != 1 || !res.Held[0].Urgent || len(g.fk.CallLog()) != calls {
		t.Fatalf("something reached the module: %+v %v", res, g.fk.CallLog())
	}
	got := strings.Join(res.Held[0].NotApplied, "\n")
	for _, want := range []string{"secret rotation of CAN_WEBHOOK_SECRET", "public routes (they stay public)", "web UI route (it stays public)", "changed settings"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q: %s", want, got)
		}
	}
	rec, _ := g.db.GetModule(ctx, "work")
	p, ok := ParsePending(rec.Pending)
	if !ok || !p.Urgent || !slices.Equal(p.NotApplied, res.Held[0].NotApplied) {
		t.Fatalf("not recorded for status and the admin UI: %q", rec.Pending)
	}
	if rec.HMACSecret != before.HMACSecret || rec.Applied != before.Applied || rec.SettingsHash != before.SettingsHash {
		t.Fatal("the record of what is installed changed")
	}
	if pend, _ := g.db.PendingRotations(ctx, "work"); len(pend) != 1 {
		t.Fatalf("the rotation request was dropped: %v", pend)
	}
}

// A record from before this knew what was applied: unchanged company.yaml is recognised and kept as
// the baseline; otherwise it cannot be told, which is said and treated as urgent.
func TestNotAppliedForAnOldRecord(t *testing.T) {
	ctx := context.Background()
	g, d := heldRig(t)
	g.r.Installer = exposureRouting{g.fk}
	// Recorded again by an installer whose hash covers the exposure.
	if _, err := g.db.ExecContext(ctx, `UPDATE modules SET settings_hash=?`, specHash(d.Company, "work", mf("work", "1", nil), `{"ui":true}`, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.ExecContext(ctx, `UPDATE modules SET applied_json='', content_json=''`); err != nil {
		t.Fatal(err)
	}
	if res := apply(t, g, d); len(res.Held[0].NotApplied) != 0 || res.Held[0].Urgent {
		t.Fatalf("unchanged: %+v", res.Held[0])
	}
	d.Company.Modules["work"] = config.ModuleEntry{}
	res := apply(t, g, d)
	if got := strings.Join(res.Held[0].NotApplied, "\n"); !res.Held[0].Urgent || !strings.Contains(got, "web UI route") {
		t.Fatalf("the baseline was not kept: %+v", res.Held[0])
	}
	if _, err := g.db.ExecContext(ctx, `UPDATE modules SET applied_json=''`); err != nil {
		t.Fatal(err)
	}
	res = apply(t, g, d)
	if got := strings.Join(res.Held[0].NotApplied, "\n"); !res.Held[0].Urgent || !strings.Contains(got, "cannot tell") {
		t.Fatalf("unknown: %+v", res.Held[0])
	}
}

// exposureRouting puts the exposure into the settings hash, as the real installer does.
type exposureRouting struct{ *FakeInstaller }

func (exposureRouting) Routing(_ context.Context, s Spec) (Routing, error) {
	b, err := json.Marshal(s.Expose)
	return Routing{Fingerprint: string(b)}, err
}

// signedInstaller keeps what a cluster keeps of the signing secret: the release record is signed
// with it, and the installer accepts the record only with the current or the pending one.
type signedInstaller struct {
	*FakeInstaller
	signedWith string
	failAfter  error // returned after the record was saved
	crash      bool  // panic after the record was saved
	failBefore error
}

func (f *signedInstaller) Enable(ctx context.Context, s Spec) error {
	if f.failBefore != nil {
		return f.failBefore
	}
	if f.signedWith != "" && f.signedWith != s.HMACSecret && f.signedWith != s.RecordSecret {
		return errors.New("the recorded release fails its HMAC check")
	}
	f.signedWith = s.HMACSecret
	if f.crash {
		panic("crash")
	}
	if f.failAfter != nil {
		return f.failAfter
	}
	return f.FakeInstaller.Enable(ctx, s)
}

// A rotation of the signing secret survives a failure at any point: the pending secret is stored
// before the cluster is touched and every retry reuses it.
func TestWebhookRotationDoesNotWedge(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		firstOnly bool // the module has never been installed successfully
		breakIt   func(*signedInstaller)
	}{
		{name: "failure after the rollout", breakIt: func(f *signedInstaller) { f.failAfter = errors.New("applying routes failed") }},
		{name: "failed upgrade before the record", breakIt: func(f *signedInstaller) { f.failBefore = errors.New("rollout failed") }},
		{name: "crash between save and record", breakIt: func(f *signedInstaller) { f.crash = true }},
		{name: "failed first install", firstOnly: true, breakIt: func(f *signedInstaller) { f.failAfter = errors.New("routes") }},
		{name: "crash during first install", firstOnly: true, breakIt: func(f *signedInstaller) { f.crash = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			inst := &signedInstaller{FakeInstaller: g.fk}
			g.r.Installer = inst
			d := desired(nil, nil, mf("work", "1", nil))
			if !tc.firstOnly {
				apply(t, g, d)
			}
			old := inst.signedWith
			if !tc.firstOnly {
				if err := g.db.RequestRotation(ctx, "work", modsecrets.WebhookKey, "test"); err != nil {
					t.Fatal(err)
				}
			}
			tc.breakIt(inst)
			var used []string
			for range 2 { // fails, and fails again on the retry
				func() {
					defer func() { _ = recover() }()
					_, _ = g.r.Apply(ctx, d, Options{})
				}()
				used = append(used, inst.signedWith)
			}
			if used[0] != used[1] {
				t.Fatalf("a retry used another secret")
			}
			if rec, err := g.db.GetModule(ctx, "work"); err == nil && !tc.firstOnly && rec.HMACSecret != old {
				t.Fatal("an unfinished rotation was recorded")
			}
			inst.crash, inst.failAfter, inst.failBefore = false, nil, nil
			apply(t, g, d)
			rec, err := g.db.GetModule(ctx, "work")
			if err != nil || rec.State != store.StateEnabled || rec.HMACSecret != inst.signedWith || (!tc.firstOnly && rec.HMACSecret == old) {
				t.Fatalf("not recovered: %+v %v", rec.State, err)
			}
			if pend, _ := g.db.PendingRotations(ctx, "work"); len(pend) != 0 {
				t.Fatal("rotation still pending")
			}
			// The next rotation makes a new secret, not the one just used.
			last := rec.HMACSecret
			if err := g.db.RequestRotation(ctx, "work", modsecrets.WebhookKey, "test"); err != nil {
				t.Fatal(err)
			}
			apply(t, g, d)
			if rec, _ := g.db.GetModule(ctx, "work"); rec.HMACSecret == last {
				t.Fatal("the second rotation reused the secret")
			}
		})
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

package modmgr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/audit"
)

func wizRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.svc.Bundles = []Bundle{{ID: "starter", Modules: []string{"web", "desk", "ghost"}}, {ID: "blank"}}
	forceOpen(t, r)
	return r
}

// forceOpen turns the seeded wizard into a fresh install's (the base company already runs web).
func forceOpen(t *testing.T, r *rig) {
	t.Helper()
	if _, err := r.svc.WizardState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(context.Background(), `UPDATE wizard_state SET status='open'`); err != nil {
		t.Fatal(err)
	}
}

func TestWizardIsNotOpenedForAnInstallationThatAlreadyRunsModules(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if need, _ := r.svc.WizardNeeded(ctx); need {
		t.Fatal("an existing installation was sent to the wizard")
	}
	if err := r.svc.SetRelay(ctx, "root", RelayInput{Host: "smtp.example", Port: 587, Username: "a", Password: "p", From: "a@b.example"}); err == nil {
		t.Fatal("relay Secret overwritten although setup is closed")
	}
	if len(r.sec.Data) != 0 {
		t.Fatal("a Secret was written")
	}
	r2 := newRig(t)
	snap, _ := r2.store.Get(ctx)
	if _, err := r2.store.Put(ctx, []byte("schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n"), snap.Version); err != nil {
		t.Fatal(err)
	}
	if need, _ := r2.svc.WizardNeeded(ctx); !need {
		t.Fatal("a fresh installation is not sent to the wizard")
	}
}

func TestWizardStateIsSeededFromCompanyAndResumable(t *testing.T) {
	r := wizRig(t)
	ctx := context.Background()
	ws, err := r.svc.WizardState(ctx)
	must(t, err)
	if ws.Status != WizardOpen || ws.Data.Name != "Acme" || ws.Data.Domain != "acme.example" || ws.Data.ServerRAMMB != 2048 {
		t.Fatalf("%+v", ws)
	}
	_, err = r.svc.WizardUpdate(ctx, "root", func(d *WizardData) error { d.Name = "Acme Two"; d.Template = "starter"; return nil })
	must(t, err)
	// "resume": a new Service over the same database sees the saved data, not a fresh seed
	again, err := r.svc.WizardState(ctx)
	must(t, err)
	if again.Data.Name != "Acme Two" || again.Data.Template != "starter" || again.Revision == 0 {
		t.Fatalf("%+v", again)
	}
	// saving identical data does not bump the revision (idempotent steps)
	rev := again.Revision
	ws, _ = r.svc.WizardUpdate(ctx, "root", func(d *WizardData) error { d.Name = "Acme Two"; return nil })
	if ws.Revision != rev {
		t.Fatalf("revision %d -> %d", rev, ws.Revision)
	}
}

func TestWizardBundleSelectsDeployableModulesOnly(t *testing.T) {
	r := wizRig(t)
	ctx := context.Background()
	sel, skipped, err := r.svc.BundleModules(ctx, "starter")
	must(t, err)
	if strings.Join(sel, ",") != "web" {
		t.Fatalf("selected %v", sel)
	}
	if len(skipped) != 2 || skipped[0].ID != "desk" || skipped[0].Reason != SkipNotDeployable || skipped[1].ID != "ghost" || skipped[1].Reason != SkipUnknown {
		t.Fatalf("%+v", skipped)
	}
	if _, _, err := r.svc.BundleModules(ctx, "nope"); err == nil {
		t.Fatal("unknown bundle accepted")
	}
}

func TestWizardApplyIsIdempotentAndCompletes(t *testing.T) {
	r := wizRig(t)
	ctx := context.Background()
	_, err := r.svc.WizardUpdate(ctx, "root", func(d *WizardData) error {
		d.Name, d.Domain, d.Modules = "Acme Ltd", "acme.example", []string{"web", "alt"}
		return nil
	})
	must(t, err)
	// web and alt both serve leads (web is already enabled): the review must say so and apply must refuse
	pv, err := r.svc.WizardPreview(ctx)
	must(t, err)
	if len(pv.Analysis.Conflicts) == 0 {
		t.Fatalf("exclusive capability conflict not reported: %+v", pv.Analysis)
	}
	if _, err := r.svc.WizardApply(ctx, "root"); err == nil {
		t.Fatal("applied a conflicting selection")
	}
	_, _ = r.svc.WizardUpdate(ctx, "root", func(d *WizardData) error {
		d.Modules = []string{"web", "plan"}
		d.Acks = []string{"experimental:plan"}
		return nil
	})
	// plan requires a capability only it plans: blocked as well
	if _, err := r.svc.WizardApply(ctx, "root"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("%v", err)
	}
	_, _ = r.svc.WizardUpdate(ctx, "root", func(d *WizardData) error { d.Modules = []string{"web"}; d.Acks = nil; return nil })
	run, err := r.svc.WizardApply(ctx, "root")
	must(t, err)
	if run.State != RunSucceeded {
		t.Fatalf("%+v", run)
	}
	ws, _ := r.svc.WizardState(ctx)
	if ws.Status != WizardCompleted {
		t.Fatalf("%+v", ws)
	}
	snap, _ := r.store.Get(ctx)
	if !strings.Contains(string(snap.Data), "name: Acme Ltd") {
		t.Fatalf("%s", snap.Data)
	}
	calls := len(r.fk.CallLog())
	again, err := r.svc.WizardApply(ctx, "root")
	must(t, err)
	if again.ID != run.ID || len(r.fk.CallLog()) != calls {
		t.Fatalf("second apply started new work: %+v", again)
	}
}

func TestWizardDismissAndCompletionStopTheRedirect(t *testing.T) {
	r := wizRig(t)
	ctx := context.Background()
	if need, _ := r.svc.WizardNeeded(ctx); !need {
		t.Fatal("a fresh install needs the wizard")
	}
	must(t, r.svc.WizardDismiss(ctx, "root"))
	if need, _ := r.svc.WizardNeeded(ctx); need {
		t.Fatal("dismissed wizard still needed")
	}
	if !strings.Contains(strings.Join(auditActions(t, r), "|"), "wizard.dismiss") {
		t.Fatal("dismissal not audited")
	}
}

func TestRelayPasswordGoesToASecretAndIsNeverStoredOrAudited(t *testing.T) {
	r := wizRig(t)
	ctx := context.Background()
	err := r.svc.SetRelay(ctx, "root", RelayInput{Host: "smtp.relay.example", Port: 587, Username: "apikey", Password: "s3cr3t-pass", From: "hello@acme.example"})
	must(t, err)
	sec := r.sec.Get(RelayNamespace, RelaySecretName)
	if sec["password"] != "s3cr3t-pass" || sec["host"] != "smtp.relay.example" {
		t.Fatalf("%v", sec)
	}
	ws, _ := r.svc.WizardState(ctx)
	if !ws.Data.Relay.Configured || ws.Data.Relay.Host != "smtp.relay.example" {
		t.Fatalf("%+v", ws.Data.Relay)
	}
	var blob string
	must(t, r.db.QueryRowContext(ctx, `SELECT data_json FROM wizard_state`).Scan(&blob))
	if strings.Contains(blob, "s3cr3t-pass") {
		t.Fatal("password stored in the wizard state")
	}
	recs, _ := r.audit.Query(ctx, audit.Filter{Limit: 500})
	for _, rc := range recs {
		if strings.Contains(rc.Detail, "s3cr3t-pass") || strings.Contains(rc.Target, "s3cr3t-pass") {
			t.Fatalf("password in audit: %+v", rc)
		}
	}
	// validation and fail-closed
	for name, in := range map[string]RelayInput{
		"no host":     {Port: 587, Username: "a", Password: "p", From: "a@b.example"},
		"bad port":    {Host: "smtp.example", Port: 0, Username: "a", Password: "p", From: "a@b.example"},
		"host w/ url": {Host: "smtp://x", Port: 587, Username: "a", Password: "p", From: "a@b.example"},
		"bad from":    {Host: "smtp.example", Port: 587, Username: "a", Password: "p", From: "nope"},
		"no password": {Host: "smtp.example", Port: 587, Username: "a", From: "a@b.example"},
		"no username": {Host: "smtp.example", Port: 587, Password: "p", From: "a@b.example"},
	} {
		if err := r.svc.SetRelay(ctx, "root", in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	r.sec.Err = errors.New("kube down")
	if err := r.svc.SetRelay(ctx, "root", RelayInput{Host: "smtp.example", Port: 587, Username: "a", Password: "p2", From: "a@b.example"}); err == nil {
		t.Fatal("secret write failure ignored")
	}
}

func TestDNSPlanListsRecordsAndInstantURL(t *testing.T) {
	r := wizRig(t)
	ctx := context.Background()
	r.svc.Net = fakeNet{v4: []string{"203.0.113.7"}, v6: []string{"2001:db8::7"}}
	r.svc.Instant = fakeProbe{state: InstantServing}
	d, err := r.svc.DNS(ctx)
	must(t, err)
	want := map[string]bool{
		"A acme.example 203.0.113.7": false, "AAAA acme.example 2001:db8::7": false,
		"A admin.acme.example 203.0.113.7": false, "A forms.acme.example 203.0.113.7": false,
	}
	for _, rec := range d.Records {
		k := rec.Type + " " + rec.Name + " " + rec.Value
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing record %q in %+v", k, d.Records)
		}
	}
	if d.InstantURL != "https://203-0-113-7.sslip.io" || d.Instant != InstantServing {
		t.Fatalf("%+v", d)
	}
	// unknown address: say so rather than invent records
	r.svc.Net = fakeNet{}
	d, _ = r.svc.DNS(ctx)
	if len(d.Records) != 0 || d.NoAddress == false {
		t.Fatalf("%+v", d)
	}
	r.svc.Net = nil
	if d, _ = r.svc.DNS(ctx); !d.NoAddress {
		t.Fatal("no NetInfo must read as unknown")
	}
}

type fakeNet struct{ v4, v6 []string }

func (f fakeNet) PublicIPs(context.Context) ([]string, []string, error) { return f.v4, f.v6, nil }

type fakeProbe struct{ state string }

func (f fakeProbe) Probe(context.Context, string) string { return f.state }

func TestStatusAggregatesInterfaces(t *testing.T) {
	r := wizRig(t)
	ctx := context.Background()
	r.svc.Ingress = fakeIngress{}
	r.svc.Disk = fakeDisk{}
	st, err := r.svc.Status(ctx)
	must(t, err)
	if len(st.Modules) != 1 || st.Modules[0].ID != "web" || st.Modules[0].Ready != ReadyYes {
		t.Fatalf("%+v", st.Modules)
	}
	if st.Reconcile == nil || !st.Reconcile.OK {
		t.Fatalf("%+v", st.Reconcile)
	}
	if len(st.Certs) != 1 || st.Certs[0].Host != "forms.acme.example" || st.CertsNote != "" {
		t.Fatalf("%+v", st.Certs)
	}
	if len(st.Disk) != 1 || st.Disk[0].UsedPercent != 50 {
		t.Fatalf("%+v", st.Disk)
	}
	// interfaces absent or failing: reported as unavailable, never as healthy
	r.svc.Ingress, r.svc.Disk = nil, failingDisk{}
	st, _ = r.svc.Status(ctx)
	if st.CertsNote == "" || st.DiskNote == "" || len(st.Certs) != 0 {
		t.Fatalf("%+v", st)
	}
}

type fakeIngress struct{}

func (fakeIngress) Certificates(context.Context) ([]CertStatus, error) {
	return []CertStatus{{Host: "forms.acme.example", State: "valid", DaysLeft: 60}}, nil
}

type fakeDisk struct{}

func (fakeDisk) Usage(context.Context) ([]DiskUsage, error) {
	return []DiskUsage{{Name: "data", UsedBytes: 5, TotalBytes: 10}}, nil
}

type failingDisk struct{}

func (failingDisk) Usage(context.Context) ([]DiskUsage, error) {
	return nil, errors.New("statfs: boom")
}

// QA 6: the status page carries the outcome of the last backup dump run.
func TestStatusCarriesTheLastDumpRun(t *testing.T) {
	r := newRig(t)
	r.svc.LastDump = func() (backup.DumpRun, error) {
		return backup.DumpRun{RunID: "r", Error: "module wiki: boom"}, nil
	}
	h, err := r.svc.Status(context.Background())
	if err != nil || h.Dump == nil || h.Dump.OK || !strings.Contains(h.Dump.Error, "boom") {
		t.Fatalf("%+v %v", h.Dump, err)
	}
}

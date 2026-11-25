package modmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type rig struct {
	svc   *Service
	db    *store.DB
	fk    *reconcile.FakeInstaller
	store *FileStore
	path  string
	audit *audit.Log
	purge *fakePurger
	sec   *FakeSecrets
}

type fakePurger struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakePurger) Purge(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return f.err
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	al := audit.New(db)
	fk := reconcile.NewFakeInstaller()
	rec := reconcile.New(db, registry.New(db, "http://can-core"), al, fk)
	rec.Actor = "can-core"
	dir := writeModules(t)
	path := filepath.Join(t.TempDir(), "company.yaml")
	must(t, os.WriteFile(path, []byte(baseCompany), 0o644))
	fs := &FileStore{Path: path}
	d, ps := reconcile.LoadDesired(path, dir)
	if len(ps) > 0 && d.Company == nil {
		t.Fatalf("base company invalid: %v", ps)
	}
	baseRes, err := rec.Apply(ctx, d, reconcile.Options{}) // web is installed before the test starts
	must(t, err)
	r := &rig{db: db, fk: fk, store: fs, path: path, audit: al, purge: &fakePurger{}, sec: NewFakeSecrets()}
	r.svc = &Service{DB: db, Audit: al, ModulesDir: dir, Company: fs, Runner: NewRunner(rec), Purger: r.purge, Secrets: r.sec,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }, Async: func(f func()) { f() }}
	r.svc.RecordReconcile(ctx, baseRes, nil)
	return r
}

func auditActions(t *testing.T, r *rig) []string {
	t.Helper()
	recs, err := r.audit.Query(context.Background(), audit.Filter{Limit: 200})
	must(t, err)
	var out []string
	for i := len(recs) - 1; i >= 0; i-- {
		out = append(out, recs[i].Action+" "+recs[i].Target)
	}
	return out
}

func TestProposeApplyEnablesModuleAndAuditsBeforeAndAfter(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ch, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("solo", true)}}, Summary: "enable alt"})
	must(t, err)
	pv, err := r.svc.Preview(ctx, ch.ID)
	must(t, err)
	if pv.Stale || len(pv.Actions) != 1 || pv.Actions[0].Kind != reconcile.Install || pv.Actions[0].Module != "solo" {
		t.Fatalf("%+v", pv)
	}
	if !strings.Contains(joinDiff(pv.Diff), "+  solo:") {
		t.Fatalf("diff lacks the change:\n%s", joinDiff(pv.Diff))
	}
	rp := pv.Resources[0]
	if rp.Namespace != "mod-solo" || rp.Host != "solo.acme.example" || len(rp.Secrets) == 0 {
		t.Fatalf("%+v", rp)
	}
	run, err := r.svc.Apply(ctx, "root", ch.ID)
	must(t, err)
	if run.State != RunSucceeded || r.fk.EnabledCount() != 2 {
		t.Fatalf("run %+v enabled %d", run, r.fk.EnabledCount())
	}
	snap, _ := r.store.Get(ctx)
	if !strings.Contains(string(snap.Data), "solo:") {
		t.Fatalf("company.yaml not updated:\n%s", snap.Data)
	}
	acts := strings.Join(auditActions(t, r), "|")
	b, a := strings.Index(acts, "company.change.begin"), strings.Index(acts, "company.change.saved")
	if b < 0 || a < 0 || b > a {
		t.Fatalf("audit must record the intent before the write and the result after: %s", acts)
	}
	// applying again is idempotent: the same run, no second install
	again, err := r.svc.Apply(ctx, "root", ch.ID)
	must(t, err)
	if again.ID != run.ID || len(r.fk.CallLog()) != 2 {
		t.Fatalf("not idempotent: %+v %v", again, r.fk.CallLog())
	}
}

func joinDiff(d []DiffLine) string {
	var s []string
	for _, l := range d {
		s = append(s, string(l.Op)+l.Text)
	}
	return strings.Join(s, "\n")
}

func TestAuditFailureBlocksTheWrite(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ch, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("solo", true)}}})
	must(t, err)
	r.svc.Audit = failingAudit{}
	if _, err := r.svc.Apply(ctx, "root", ch.ID); err == nil {
		t.Fatal("apply succeeded without an audit record")
	}
	snap, _ := r.store.Get(ctx)
	if strings.Contains(string(snap.Data), "solo:") {
		t.Fatal("company.yaml written although the audit entry could not be")
	}
}

type failingAudit struct{}

func (failingAudit) Append(context.Context, audit.Entry) (audit.Record, error) {
	return audit.Record{}, errors.New("audit down")
}
func (failingAudit) Query(context.Context, audit.Filter) ([]audit.Record, error) { return nil, nil }

func TestNotDeployableAndAcknowledgements(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("desk", true)}}}); !errors.Is(err, ErrNotDeployable) {
		t.Fatalf("not-deployable module accepted: %v", err)
	}
	ops := Patch{Ops: []Op{SetEnabled("crm", true)}}
	_, err := r.svc.Propose(ctx, "root", Proposal{Patch: ops, Acks: []string{"experimental:crm"}})
	var ae *AckError
	if !errors.As(err, &ae) || len(ae.Missing) != 1 || ae.Missing[0] != "licence:crm" {
		t.Fatalf("source-available needs its own opt-in: %v", err)
	}
	// crm requires `leads`, which web (enabled in the base company) serves
	if _, err = r.svc.Propose(ctx, "root", Proposal{Patch: ops, Acks: []string{"licence:crm", "experimental:crm"}}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	// an unrelated acknowledgement does not satisfy another module
	if _, err := r.svc.Propose(ctx, "root", Proposal{Patch: ops, Acks: []string{"licence:web", "experimental:web"}}); err == nil {
		t.Fatal("acks for another module accepted")
	}
}

func TestEnablingWithoutAnUnmetRequirementIsBlocked(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// remove web (the only leads provider) from the base company, then try crm alone
	data := "schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\n"
	_, err := r.store.Put(ctx, []byte(data), mustVersion(t, r))
	must(t, err)
	_, err = r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("crm", true)}}, Acks: []string{"licence:crm", "experimental:crm"}})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("crm without a leads provider must be blocked: %v", err)
	}
}

func mustVersion(t *testing.T, r *rig) string {
	t.Helper()
	s, err := r.store.Get(context.Background())
	must(t, err)
	return s.Version
}

func TestSettingsChangesAreValidatedAgainstTheModuleSchema(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetSettings("web", map[string]any{"site_title": "x", "max_rows": int64(1000)})}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("schema violation accepted: %v", err)
	}
	_, err = r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetSettings("solo", map[string]any{"anything": "x"})}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("settings for a module without a schema must be refused: %v", err)
	}
	ch, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetSettings("web", map[string]any{"site_title": "Hello"})}}})
	must(t, err)
	pv, _ := r.svc.Preview(ctx, ch.ID)
	if len(pv.Diff) == 0 {
		t.Fatal("no diff")
	}
}

func TestStaleChangeIsRefusedAndNothingIsWritten(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("solo", true)}}})
	must(t, err)
	b, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetString("name", "Other")}}})
	must(t, err)
	if _, err := r.svc.Apply(ctx, "root", b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Apply(ctx, "root", a.ID); !errors.Is(err, ErrStale) {
		t.Fatalf("stale change applied: %v", err)
	}
	pv, _ := r.svc.Preview(ctx, a.ID)
	if !pv.Stale {
		t.Fatal("preview does not say stale")
	}
	if len(r.fk.CallLog()) != 1 { // only the initial install of web
		t.Fatalf("installer called: %v", r.fk.CallLog())
	}
}

func TestExpiredChangeCannotBeApplied(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ch, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("solo", true)}}})
	must(t, err)
	r.svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).Add(ChangeTTL + time.Minute) }
	if _, err := r.svc.Apply(ctx, "root", ch.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("%v", err)
	}
}

func TestOnlyOneApplyRunsAtATime(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	gate := make(chan struct{})
	r.svc.Runner = &blockingRunner{Reconciling: r.svc.Runner, gate: gate}
	var wg sync.WaitGroup
	r.svc.Async = func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	a, _ := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("solo", true)}}})
	run, err := r.svc.Apply(ctx, "root", a.ID)
	must(t, err)
	if run.State != RunRunning {
		t.Fatalf("%+v", run)
	}
	b, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetString("name", "Busy")}}})
	must(t, err)
	if _, err := r.svc.Apply(ctx, "root", b.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("second apply while one runs: %v", err)
	}
	if s, _ := r.store.Get(ctx); strings.Contains(string(s.Data), "Busy") {
		t.Fatal("a refused apply wrote company.yaml")
	}
	close(gate)
	wg.Wait()
	got, _ := r.svc.Run(ctx, run.ID)
	if got.State != RunSucceeded {
		t.Fatalf("%+v", got)
	}
}

type blockingRunner struct {
	Reconciling
	gate chan struct{}
}

func (b *blockingRunner) Apply(ctx context.Context, d reconcile.Desired) (reconcile.Result, error) {
	<-b.gate
	return b.Reconciling.Apply(ctx, d)
}

func TestFailedApplyShowsASanitisedError(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fk.FailEnable["solo"] = errors.New("helm: pull https://deploy:hunter2@registry.example/x failed: password=hunter2 token: abc123\x00\x1b[31m " + strings.Repeat("long ", 200))
	ch, _ := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("solo", true)}}})
	run, err := r.svc.Apply(ctx, "root", ch.ID)
	must(t, err)
	if run.State != RunFailed || len(run.Failed) != 1 {
		t.Fatalf("%+v", run)
	}
	for _, s := range []string{run.Error, run.Modules[0].LastError} {
		if strings.Contains(s, "hunter2") || strings.Contains(s, "abc123") || strings.ContainsAny(s, "\x00\x1b") || len([]rune(s)) > MaxErrorRunes+3 {
			t.Fatalf("unsanitised error: %q", s)
		}
	}
	st, _ := r.svc.Status(ctx)
	if st.Reconcile == nil || st.Reconcile.OK || st.Reconcile.Failed != 1 {
		t.Fatalf("%+v", st.Reconcile)
	}
}

func TestSanitise(t *testing.T) {
	for in, banned := range map[string]string{
		"postgres://u:pw123@db/x":      "pw123",
		"Authorization: Bearer abcdef": "abcdef",
		"api_key=zzz secret: yyy":      "zzz",
		"a\r\nb\tc":                    "\r",
	} {
		if got := Sanitise(in); strings.Contains(got, banned) {
			t.Errorf("Sanitise(%q) = %q", in, got)
		}
	}
	if got := Sanitise(strings.Repeat("é", 1000)); len([]rune(got)) > MaxErrorRunes+3 {
		t.Fatal("not truncated")
	}
}

func TestPreviewDescribesWhatDisableKeeps(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ch, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("web", false)}}})
	must(t, err)
	pv, _ := r.svc.Preview(ctx, ch.ID)
	if len(pv.Resources) != 1 || pv.Resources[0].Action != string(reconcile.Disable) {
		t.Fatalf("%+v", pv.Resources)
	}
	keeps := strings.Join(pv.Resources[0].Keeps, "|")
	for _, w := range []string{"namespace", "pvc", "secrets"} {
		if !strings.Contains(keeps, w) {
			t.Errorf("disable preview must say it keeps %s: %q", w, keeps)
		}
	}
	run, _ := r.svc.Apply(ctx, "root", ch.ID)
	if run.State != RunSucceeded || r.fk.EnabledCount() != 0 {
		t.Fatalf("%+v", run)
	}
}

func TestPendingChangesAreBounded(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < MaxPendingChanges+10; i++ {
		_, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetString("name", "N"+strings.Repeat("x", i%5))}}})
		must(t, err)
	}
	var n int
	must(t, r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM company_changes WHERE state='proposed'`).Scan(&n))
	if n > MaxPendingChanges {
		t.Fatalf("%d pending changes", n)
	}
}

func TestPurgeNeedsDisabledModuleAndTypedConfirmation(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.svc.Purge(ctx, "root", "web", "purge web"); !errors.Is(err, ErrNotPurgeable) {
		t.Fatalf("an enabled module was purged: %v", err)
	}
	ch, _ := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetEnabled("web", false)}}})
	if _, err := r.svc.Apply(ctx, "root", ch.ID); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "purge", "PURGE web", "purge web ", "purge alt", "delete web"} {
		if err := r.svc.Purge(ctx, "root", "web", bad); !errors.Is(err, ErrConfirmation) {
			t.Errorf("confirmation %q accepted: %v", bad, err)
		}
	}
	if len(r.purge.calls) != 0 {
		t.Fatal("purger called without confirmation")
	}
	var forgotten []string
	r.svc.OnPurged = func(id string) error { forgotten = append(forgotten, id); return nil }
	must(t, r.svc.Purge(ctx, "root", "web", "purge web"))
	if strings.Join(forgotten, ",") != "web" {
		t.Fatalf("a purged module's backup dumps must be deleted: %v", forgotten)
	}
	if strings.Join(r.purge.calls, ",") != "web" {
		t.Fatalf("%v", r.purge.calls)
	}
	acts := strings.Join(auditActions(t, r), "|")
	if !strings.Contains(acts, "module.purge.begin web") || !strings.Contains(acts, "module.purge.done web") {
		t.Fatalf("audit: %s", acts)
	}
	if _, err := r.db.GetModule(ctx, "web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recorded module still present after purge: %v", err)
	}
	// a failing purger is audited and reported
	r.purge.err = errors.New("boom")
	r.purge.calls = nil
	err := r.svc.Purge(ctx, "root", "web", "purge web")
	if err == nil {
		t.Fatal("purge of an already purged module should fail")
	}
}

func TestPurgeWithoutAnExecutorFailsClosed(t *testing.T) {
	r := newRig(t)
	r.svc.Purger = nil
	if err := r.svc.Purge(context.Background(), "root", "web", "purge web"); !errors.Is(err, ErrNoPurger) {
		t.Fatalf("%v", err)
	}
}

func TestSettingsPreviewForADisabledModuleIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, err := r.svc.Propose(ctx, "root", Proposal{Patch: Patch{Ops: []Op{SetSettings("solo", map[string]any{})}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("settings for a module that is not enabled: %v", err)
	}
	s, _ := r.store.Get(ctx)
	if string(s.Data) != baseCompany {
		t.Fatal("company.yaml changed")
	}
}

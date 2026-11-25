package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func scrapeText(reg *obs.Registry) string {
	var b strings.Builder
	_, _ = reg.WriteTo(&b)
	return b.String()
}

func TestPlatformGauges(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	al := audit.New(db)
	for i := 0; i < 3; i++ {
		if _, err := al.Append(ctx, audit.Entry{Actor: "user:alice@example.com", Action: "test.action", Target: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO approval_requests(id,module_id,type,requester,status,idem_key,request_hash,policy_json,created_at) VALUES
		('a1','m','t','user:bob','pending','k1','h','{}',1),('a2','m','t','user:bob','approved','k2','h','{}',1)`)
	mustExec(`INSERT INTO modules(id,version,namespace,service_account,state,settings_hash,hmac_secret,manifest_json,updated_at) VALUES ('m1','1','ns','sa','enabled','h','s','{}',1)`)
	mustExec(`INSERT INTO events(id,type,time,source,data_json) VALUES ('e1','x.v1',1,'m1','{}'),('e2','x.v1',1,'m1','{}')`)
	mustExec(`INSERT INTO deliveries(event_id,module_id,url,status,attempts,next_attempt_at) VALUES ('e1','m1','http://x','dead',8,1),('e2','m1','http://x','pending',1,1)`)

	reg := obs.NewRegistry()
	pm := newPlatformMetrics(reg)
	pm.collect(ctx, db, al, time.Unix(1_700_000_000, 0))
	out := scrapeText(reg)
	for _, want := range []string{
		"can_approvals_pending 1",
		"can_audit_entries 3",
		"can_webhook_dead_letters 1",
		"can_webhook_pending 1",
		"can_audit_verify_ok 1",
		"can_audit_verify_last_run_timestamp_seconds 1700000000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "alice") || strings.Contains(out, "bob") {
		t.Fatal("personal data in metrics")
	}

	// A tampered chain flips the verify gauge.
	mustExec(`UPDATE audit_log SET detail='{"forged":true}' WHERE seq=2`)
	pm.verifyEvery = 0
	pm.collect(ctx, db, al, time.Unix(1_700_000_100, 0))
	if out := scrapeText(reg); !strings.Contains(out, "can_audit_verify_ok 0") {
		t.Fatalf("tampering not reported:\n%s", out)
	}
}

func TestReconcileMetricsAndHealth(t *testing.T) {
	reg := obs.NewRegistry()
	h := obs.NewHealth()
	h.SetTTL(0)
	rm := newReconcileMetrics(reg, h)
	rm.observe(nil, time.Unix(100, 0))
	rm.observe(errors.New("kube said password=hunter2"), time.Unix(200, 0))
	rm.observe(errors.New("again"), time.Unix(300, 0))
	out := scrapeText(reg)
	for _, want := range []string{
		`can_reconcile_runs_total{result="ok"} 1`,
		`can_reconcile_runs_total{result="error"} 2`,
		"can_reconcile_last_success_timestamp_seconds 100",
		"can_reconcile_consecutive_failures 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Fatal("error text in metrics")
	}
	if rep := h.Check(context.Background()); rep.Status != obs.StatusDegraded {
		t.Fatalf("failing reconcile must degrade readiness, not fail it: %+v", rep)
	}
	rm.observe(nil, time.Unix(400, 0))
	if rep := h.Check(context.Background()); rep.Status != obs.StatusOK {
		t.Fatalf("recovered: %+v", rep)
	}
}

func TestAuditVerifyGaugeIsAbsentUntilARunCompletesAndRetriesSoonAfterAnError(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	al := audit.New(db)
	reg := obs.NewRegistry()
	pm := newPlatformMetrics(reg)
	if out := scrapeText(reg); hasSample(out, "can_audit_verify_ok") || hasSample(out, "can_audit_verify_last_run_timestamp_seconds") {
		t.Fatalf("verify gauges must not exist (and so cannot read 0) before the first verification:\n%s", out)
	}
	fail := true
	pm.verify = func(context.Context) (audit.Result, error) {
		if fail {
			return audit.Result{}, errors.New("database is locked")
		}
		return audit.Result{Entries: 1}, nil
	}
	t0 := time.Unix(1_700_000_000, 0)
	pm.collect(ctx, db, al, t0)
	if out := scrapeText(reg); hasSample(out, "can_audit_verify_ok") {
		t.Fatalf("an errored verification is not evidence of anything:\n%s", out)
	}
	// not yet due after 1 minute, due after 5 (retry), not the full hour
	fail = false
	pm.collect(ctx, db, al, t0.Add(time.Minute))
	if hasSample(scrapeText(reg), "can_audit_verify_ok") {
		t.Fatal("retried too soon")
	}
	pm.collect(ctx, db, al, t0.Add(6*time.Minute))
	if out := scrapeText(reg); !strings.Contains(out, "can_audit_verify_ok 1") || !strings.Contains(out, "can_audit_verify_last_run_timestamp_seconds 1700000360") {
		t.Fatalf("retry after an error did not run:\n%s", out)
	}
}

// hasSample reports whether the exposition has a sample line (not a HELP or TYPE comment) for name.
func hasSample(out, name string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, name+" ") || strings.HasPrefix(l, name+"{") {
			return true
		}
	}
	return false
}

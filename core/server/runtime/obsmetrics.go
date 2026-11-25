package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// platformMetrics holds the few business gauges can-core exports. They are refreshed by a
// background loop, never at scrape time, so a scrape cannot queue behind the single SQLite
// connection. Nothing here is per user: only counts and timestamps.
type platformMetrics struct {
	approvalsPending *obs.Gauge
	auditEntries     *obs.Gauge
	deadLetters      *obs.Gauge
	pending          *obs.Gauge
	verifyOK         *obs.GaugeVec // series created by the first completed run, so it can never read 0 before one
	verifyAt         *obs.GaugeVec

	verifyEvery time.Duration
	retryEvery  time.Duration
	verify      func(context.Context) (audit.Result, error)
	mu          sync.Mutex
	lastVerify  time.Time
}

func newPlatformMetrics(reg *obs.Registry) *platformMetrics {
	g := func(name, help string) *obs.Gauge { return reg.Gauge(name, help).With() }
	return &platformMetrics{
		approvalsPending: g("can_approvals_pending", "Approval requests waiting for a decision."),
		auditEntries:     g("can_audit_entries", "Entries in the tamper-evident audit chain (its length)."),
		deadLetters:      g("can_webhook_dead_letters", "Webhook deliveries that exhausted their retries and need an operator."),
		pending:          g("can_webhook_pending", "Webhook deliveries waiting for a first or repeated attempt."),
		verifyOK:         reg.Gauge("can_audit_verify_ok", "1 when the last full audit chain verification passed, 0 when it found a violation. Absent until the first verification completes."),
		verifyAt:         reg.Gauge("can_audit_verify_last_run_timestamp_seconds", "Unix time of the last completed full audit chain verification. Absent until one completes."),
		verifyEvery:      time.Hour,
		retryEvery:       5 * time.Minute,
	}
}

// collect refreshes the gauges; the audit chain is fully re-verified at most every verifyEvery.
func (m *platformMetrics) collect(ctx context.Context, db *store.DB, al *audit.Log, now time.Time) {
	count := func(g *obs.Gauge, q string) {
		var n int64
		if err := db.QueryRowContext(ctx, q).Scan(&n); err == nil {
			g.Set(float64(n))
		}
	}
	count(m.approvalsPending, `SELECT COUNT(*) FROM approval_requests WHERE status='pending'`)
	count(m.deadLetters, `SELECT COUNT(*) FROM deliveries WHERE status='dead'`)
	count(m.pending, `SELECT COUNT(*) FROM deliveries WHERE status='pending'`)
	if h, err := al.Head(ctx); err == nil {
		m.auditEntries.Set(float64(h.Seq))
	}
	m.mu.Lock()
	due := m.lastVerify.IsZero() || now.Sub(m.lastVerify) >= m.verifyEvery
	if due {
		m.lastVerify = now
	}
	m.mu.Unlock()
	if !due {
		return
	}
	verify := m.verify
	if verify == nil {
		verify = func(ctx context.Context) (audit.Result, error) { return al.Verify(ctx, audit.VerifyOptions{}) }
	}
	res, err := verify(ctx)
	if err != nil {
		// A database error is not evidence of tampering: keep the last answer and try again soon
		// instead of waiting a whole interval.
		m.mu.Lock()
		m.lastVerify = now.Add(m.retryEvery - m.verifyEvery)
		m.mu.Unlock()
		return
	}
	if res.OK() {
		m.verifyOK.With().Set(1)
	} else {
		m.verifyOK.With().Set(0)
	}
	m.verifyAt.With().Set(float64(now.Unix()))
}

func (m *platformMetrics) run(ctx context.Context, db *store.DB, al *audit.Log, log *slog.Logger) {
	m.collect(ctx, db, al, time.Now())
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.collect(ctx, db, al, time.Now())
		}
	}
}

// reconcileMetrics counts reconcile runs and exposes recovery state. A failing reconcile degrades
// readiness (the service still serves the admin UI and module API) but never fails it.
type reconcileMetrics struct {
	runs    *obs.CounterVec
	lastOK  *obs.Gauge
	streak  *obs.Gauge
	mu      sync.Mutex
	failing bool
}

func newReconcileMetrics(reg *obs.Registry, h *obs.Health) *reconcileMetrics {
	m := &reconcileMetrics{
		runs:   reg.Counter("can_reconcile_runs_total", "Reconcile runs by result.", "result"),
		lastOK: reg.Gauge("can_reconcile_last_success_timestamp_seconds", "Unix time of the last successful reconcile.").With(),
		streak: reg.Gauge("can_reconcile_consecutive_failures", "Reconcile runs that failed in a row.").With(),
	}
	h.Add("reconcile", false, func(context.Context) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.failing {
			return errors.New("last reconcile failed")
		}
		return nil
	})
	return m
}

// observe records one reconcile outcome. The error text is deliberately not recorded.
func (m *reconcileMetrics) observe(err error, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.runs.With("error").Inc()
		m.streak.Add(1)
		m.failing = true
		return
	}
	m.runs.With("ok").Inc()
	m.lastOK.Set(float64(now.Unix()))
	m.streak.Set(0)
	m.failing = false
}

package provision

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Revoker is can-core's kill switch for the tokens and sessions of an offboarded person.
// *modclient.Client implements it; can-core verifies the offboarding itself.
type Revoker interface {
	RevokeIdentity(ctx context.Context, partyID, subject string) (modclient.RevokeResult, error)
}

// Engine runs the workflows. All state lives in the Store: a restart resumes where it stopped.
type Engine struct {
	Store     *Store
	Dir       Directory
	Conns     *ConnectorSet
	Tasks     Tasks
	Core      Revoker
	Approvals modclient.Approvals
	Policy    Policy
	Log       *slog.Logger
	Now       func() time.Time

	mu       sync.Mutex // one pass of the worker at a time
	wake     chan struct{}
	wakeOnce sync.Once
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// Wake nudges the worker (coalesced).
func (e *Engine) Wake() <-chan struct{} {
	e.wakeOnce.Do(func() { e.wake = make(chan struct{}, 1) })
	return e.wake
}

func (e *Engine) kick() {
	_ = e.Wake()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func newRunID(now time.Time) string {
	var b [4]byte
	u := uint32(now.Unix())
	b[0], b[1], b[2], b[3] = byte(u>>24), byte(u>>16), byte(u>>8), byte(u)
	return "run_" + hex.EncodeToString(b[:]) + randHex(8)
}

// StartRequest asks for a run.
type StartRequest struct {
	Kind    Kind
	PartyID string
	// Trigger is the event type or "api"; Key makes the request idempotent (an event id or a
	// client-supplied Idempotency-Key).
	Trigger   string
	Key       string
	Initiator string // an account subject, empty for events
	Urgent    bool
	DryRun    bool
	// EffectiveDate (YYYY-MM-DD) is when a leaver's access ends; empty = the directory's end date.
	EffectiveDate string
}

// PlanResult is a dry-run answer: the steps a run would have, nothing executed or stored.
type PlanResult struct {
	Kind          Kind   `json:"kind"`
	PartyID       string `json:"party_id"`
	Privileged    bool   `json:"privileged"`
	Urgent        bool   `json:"urgent"`
	EffectiveDate string `json:"effective_date,omitempty"`
	Steps         []Step `json:"steps"`
}

// verify reads the person from the directory and checks that the requested workflow fits their
// status. Events are only hints: nothing starts on the strength of an event payload.
func (e *Engine) verify(ctx context.Context, kind Kind, party string) (Person, error) {
	if !partyRe.MatchString(party) || !kind.Valid() {
		return Person{}, ErrInvalid
	}
	p, err := e.Dir.Get(ctx, party)
	if err != nil {
		return Person{}, err
	}
	switch kind {
	case KindJoiner, KindMover:
		if !p.Employed() {
			return p, fmt.Errorf("%w: the person is not employed (status %s)", ErrConflict, p.Status)
		}
		if p.Status == "offboarding" {
			return p, fmt.Errorf("%w: the person is offboarding", ErrConflict)
		}
	case KindLeaver:
		if p.Status != "offboarding" && p.Status != "former" {
			return p, fmt.Errorf("%w: the person is not offboarding or former (status %s)", ErrConflict, p.Status)
		}
	}
	return p, nil
}

// dayStart is when a leaver's access may be removed: the start of the last day in the configured
// time zone, or (leaver_cutoff: end_of_day) the start of the day after it.
func (e *Engine) dayStart(date string) (time.Time, error) {
	d, err := time.ParseInLocation("2006-01-02", date, e.Policy.TZ)
	if err == nil && e.Policy.EndOfDay {
		d = d.AddDate(0, 0, 1)
	}
	return d, err
}

// buildPlan assembles planner input from the directory person and the module's own state.
func (e *Engine) buildPlan(ctx context.Context, req StartRequest) (Person, planInput, []Step, string, error) {
	p, err := e.verify(ctx, req.Kind, req.PartyID)
	if err != nil {
		return p, planInput{}, nil, "", err
	}
	groups, err := e.Store.personGroups(ctx, p.ID)
	if err != nil {
		return p, planInput{}, nil, "", err
	}
	accts, err := e.Store.activeAccounts(ctx, p.ID)
	if err != nil {
		return p, planInput{}, nil, "", err
	}
	applied, err := e.Store.appliedGroups(ctx, p.ID)
	if err != nil {
		return p, planInput{}, nil, "", err
	}
	urgent := req.Urgent
	if !urgent && req.Kind == KindLeaver {
		if urgent, err = e.Store.isUrgent(ctx, p.ID); err != nil {
			return p, planInput{}, nil, "", err
		}
	}
	in := planInput{Kind: req.Kind, Person: p, Urgent: urgent, Privileged: e.Policy.anyPrivileged(groups), Accounts: accts, Applied: applied, Groups: groups}
	eff := ""
	if req.Kind == KindLeaver {
		eff = firstNonEmpty(req.EffectiveDate, p.EndDate)
		now := e.now()
		in.NotBefore = now
		if eff != "" {
			if !dateRe.MatchString(eff) {
				return p, in, nil, "", fmt.Errorf("%w: effective date", ErrInvalid)
			}
			d, err := e.dayStart(eff)
			if err != nil {
				return p, in, nil, "", fmt.Errorf("%w: effective date", ErrInvalid)
			}
			in.NotBefore = d
		} else if p.Status == "offboarding" {
			// No last day on record: access must not end before one is set. The offboarded event
			// (or an administrator) releases the run.
			in.NotBefore = now.AddDate(10, 0, 0)
		}
	}
	steps, err := e.Policy.plan(e.Conns, in)
	return p, in, steps, eff, err
}

// Plan returns what a run would do, without storing or executing anything.
func (e *Engine) Plan(ctx context.Context, req StartRequest) (PlanResult, error) {
	_, in, steps, eff, err := e.buildPlan(ctx, req)
	if err != nil {
		return PlanResult{}, err
	}
	return PlanResult{Kind: req.Kind, PartyID: req.PartyID, Privileged: in.Privileged, Urgent: in.Urgent, EffectiveDate: eff, Steps: steps}, nil
}

// Start creates a run (or returns the live or already-created one). created reports a new run.
func (e *Engine) Start(ctx context.Context, req StartRequest) (Run, bool, error) {
	if !keyRe.MatchString(req.Key) {
		return Run{}, false, fmt.Errorf("%w: idempotency key", ErrInvalid)
	}
	key := string(req.Kind) + ":" + req.PartyID + ":" + req.Key
	if r, ok, err := e.Store.runByKey(ctx, key); err != nil || ok {
		return r, false, err
	}
	if live, ok, err := e.Store.liveRun(ctx, req.PartyID, req.Kind); err != nil || ok {
		if ok && req.Kind == KindLeaver {
			err = e.releaseLeaver(ctx, live, req)
		}
		return live, false, err
	}
	// A repeat trigger for a workflow that already completed does nothing, unless something else
	// happened to the person since (a joiner after a leaver is a rehire and does run).
	if last, ok, err := e.Store.lastRun(ctx, req.PartyID); err != nil {
		return Run{}, false, err
	} else if ok && last.Kind == req.Kind && last.State == RunSucceeded && req.Trigger != "api" {
		return last, false, nil
	}
	_, in, steps, eff, err := e.buildPlan(ctx, req)
	if err != nil {
		return Run{}, false, err
	}
	now := e.now()
	state := RunPending
	if req.Kind == KindLeaver && !in.NotBefore.IsZero() && in.NotBefore.After(now) && !in.Urgent {
		state = RunScheduled
	}
	run := Run{ID: newRunID(now), Kind: req.Kind, PartyID: req.PartyID, State: state, Trigger: req.Trigger, Urgent: in.Urgent,
		DryRun: req.DryRun || e.Policy.DryRun, Privileged: in.Privileged, EffectiveDate: eff, Initiator: req.Initiator, key: key}
	err = e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := e.Store.insertRun(ctx, tx, run, steps); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, firstNonEmpty(req.Initiator, "event:"+req.Trigger), "run.start", run.ID,
			map[string]any{"kind": string(run.Kind), "party_id": run.PartyID, "steps": len(steps), "urgent": run.Urgent, "dry_run": run.DryRun, "privileged": run.Privileged})
	})
	if err != nil {
		if isUnique(err) { // a parallel trigger won: return its run
			if live, ok, e2 := e.Store.liveRun(ctx, req.PartyID, req.Kind); e2 == nil && ok {
				return live, false, nil
			}
		}
		return Run{}, false, err
	}
	e.kick()
	r, err := e.Store.GetRun(ctx, run.ID)
	return r, true, err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// releaseLeaver moves a waiting leaver run forward when the trigger carries an earlier effective
// date (the person.offboarded event) or the termination was flagged urgent. It never delays one.
func (e *Engine) releaseLeaver(ctx context.Context, live Run, req StartRequest) error {
	urgent := req.Urgent
	if !urgent {
		var err error
		if urgent, err = e.Store.isUrgent(ctx, live.PartyID); err != nil {
			return err
		}
	}
	nb := int64(0)
	if !urgent {
		eff := req.EffectiveDate
		if eff == "" || !dateRe.MatchString(eff) {
			return nil
		}
		d, err := e.dayStart(eff)
		if err != nil {
			return nil
		}
		nb = d.Unix()
	}
	return e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// only the access-removal steps move; deletion keeps its own, later date
		res, err := tx.ExecContext(ctx, `UPDATE steps SET not_before=? WHERE run_id=? AND state IN ('pending','running','awaiting_approval') AND op<>? AND (not_before>? OR ?=0)`, nb, live.ID, opDeleteAccount, nb, nb)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 && live.Urgent == urgent {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET urgent=urgent OR ?, effective_date=CASE WHEN ?<>'' THEN ? ELSE effective_date END, state=CASE WHEN state='scheduled' THEN 'pending' ELSE state END, updated_at=? WHERE id=?`,
			b2i(urgent), req.EffectiveDate, req.EffectiveDate, e.now().Unix(), live.ID); err != nil {
			return err
		}
		e.kick()
		return e.Store.audit(ctx, tx, firstNonEmpty(req.Initiator, "event:"+req.Trigger), "run.release", live.ID, map[string]any{"steps": n, "urgent": urgent})
	})
}

// backoff is base * 2^(attempt-1), capped.
func (p Policy) backoff(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 1; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	return d
}

// Tick advances every active run once. It is bounded: at most MaxStepsPerTick steps run per pass
// and every connector call has its own deadline.
func (e *Engine) Tick(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids, err := e.Store.activeRunIDs(ctx, 50)
	if err != nil {
		return 0, err
	}
	budget, n := e.Policy.MaxStepsPerTick, 0
	for _, id := range ids {
		if budget <= 0 || ctx.Err() != nil {
			break
		}
		used, err := e.advance(ctx, id, budget)
		budget -= used
		n += used
		if err != nil {
			e.log().Error("provision: advancing a run failed", "run_id", id, "error", err)
		}
	}
	return n, nil
}

// advance runs the next due steps of one run, at most max, and reports how many executed.
func (e *Engine) advance(ctx context.Context, id string, max int) (int, error) {
	var wake int64 // when the run is next worth examining; 0 = as soon as the worker is free
	used, err := e.advanceRun(ctx, id, max, &wake)
	// Every pass records that the run was examined, and a run that can only wait (an effective
	// date, a backoff, an approval) is not selected again until then: waiting runs can never
	// crowd due ones out of the worker's window.
	if terr := e.Store.touchRun(ctx, id, wake); terr != nil && err == nil {
		err = terr
	}
	return used, err
}

func (e *Engine) advanceRun(ctx context.Context, id string, max int, wake *int64) (int, error) {
	used := 0
	for used < max {
		run, steps, err := e.Store.getRunRows(ctx, id)
		if err != nil {
			return used, err
		}
		if terminalRun(run.State) {
			return used, nil
		}
		now := e.now().Unix()
		if run.State == RunCompensating {
			done, ran, err := e.compensateOne(ctx, run, steps, now)
			used += ran
			if ran == 0 && !done {
				*wake = now + 5
			}
			if err != nil || done || ran == 0 {
				return used, err
			}
			continue
		}
		st := nextStep(steps)
		if st == nil {
			return used, e.finish(ctx, run, steps)
		}
		if st.notBefore > now {
			*wake = st.notBefore
			return used, e.setRunState(ctx, run, RunScheduled)
		}
		if st.nextAt > now {
			*wake = st.nextAt
			return used, nil
		}
		if e.needsApproval(run, *st) && !st.approved {
			gerr := e.gate(ctx, run, st)
			// still waiting: the decision arrives as an event, polling only catches a missed refusal
			if r2, err := e.Store.GetRun(ctx, id); err == nil && r2.State == RunAwaiting {
				*wake = now + 30
			}
			return used, gerr
		}
		if run.State != RunRunning {
			if err := e.setRunState(ctx, run, RunRunning); err != nil {
				return used, err
			}
		}
		ok, err := e.execute(ctx, run, st)
		used++
		if err != nil {
			return used, err
		}
		if !ok {
			return used, nil // backed off, dead-lettered or the run is compensating: next pass decides
		}
	}
	return used, nil
}

func nextStep(steps []stepRow) *stepRow {
	for i := range steps {
		switch steps[i].State {
		case StepPending, StepRunning, StepAwaiting:
			return &steps[i]
		}
	}
	return nil
}

// needsApproval: a sensitive step waits for a second person, except dry runs (nothing is
// executed). The planner marks only grants and deletions sensitive: removal never waits.
func (e *Engine) needsApproval(run Run, st stepRow) bool {
	if !st.Sensitive || run.DryRun {
		return false
	}
	if run.Kind == KindLeaver && st.Op != opDeleteAccount {
		return false // belt and braces: nothing but deletion of a leaver's account is ever gated
	}
	return true
}

func (e *Engine) setRunState(ctx context.Context, run Run, state string) error {
	if run.State == state {
		return nil
	}
	return e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE runs SET state=?, updated_at=? WHERE id=? AND state NOT IN ('succeeded','failed','compensated','cancelled')`, state, e.now().Unix(), run.ID)
		return err
	})
}

// finish closes a run whose steps are all settled.
func (e *Engine) finish(ctx context.Context, run Run, steps []stepRow) error {
	return e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		skipped, dead := 0, 0
		for _, s := range steps {
			switch s.State {
			case StepSkipped:
				skipped++
			case StepDead, StepRejected:
				dead++
			}
		}
		final := RunSucceeded
		if dead > 0 {
			final = RunFailed
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state=?, updated_at=? WHERE id=?`, final, e.now().Unix(), run.ID); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, "engine", "run."+final, run.ID, map[string]any{"kind": string(run.Kind), "party_id": run.PartyID, "steps": len(steps), "skipped": skipped, "dead": dead})
	})
}

// gate opens (or checks) the approval of a sensitive step. The step proceeds only after a signed
// approval.decided.v1 with approvers that are not the person and not whoever started the run.
func (e *Engine) gate(ctx context.Context, run Run, st *stepRow) error {
	if st.ApprovalID == "" {
		if e.Approvals == nil {
			// Fail closed: no approvals engine, no sensitive step.
			return e.markStep(ctx, run, st, StepRejected, "no_approvals", "run.step.rejected", true)
		}
		a, err := e.Approvals.CreateApproval(ctx, modclient.ApprovalInput{
			Type: approvalType(run.Kind), Requester: "party:" + run.PartyID, Ref: fmt.Sprintf("%s#%d", run.ID, st.Ord),
			Title: fmt.Sprintf("%s %s for %s", st.Op, st.Connector, run.PartyID), Key: fmt.Sprintf("prov:%s:%d:%d", run.ID, st.Ord, st.gen),
		})
		if errors.Is(err, modclient.ErrApprovalRefused) {
			return e.markStep(ctx, run, st, StepRejected, "approval_refused", "run.step.rejected", true)
		}
		if err != nil {
			return err // unavailable: try again next pass
		}
		return e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE steps SET state='awaiting_approval', approval_id=? WHERE run_id=? AND ord=?`, a.ID, run.ID, st.Ord); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='awaiting_approval', updated_at=? WHERE id=?`, e.now().Unix(), run.ID); err != nil {
				return err
			}
			return e.Store.audit(ctx, tx, "engine", "approval.requested", run.ID, map[string]any{"step": st.Ord, "approval_id": a.ID, "type": approvalType(run.Kind)})
		})
	}
	// Waiting: the decision arrives as a signed event; polling only catches a refusal that was
	// never delivered. An approval is never taken as granted from a poll.
	if e.Approvals != nil {
		if a, err := e.Approvals.GetApproval(ctx, st.ApprovalID); err == nil && (a.Status == "rejected" || a.Status == "cancelled" || a.Status == "expired") {
			return e.markStep(ctx, run, st, StepRejected, "approval_"+a.Status, "run.step.rejected", true)
		}
	}
	return e.setRunState(ctx, run, RunAwaiting)
}

// markStep settles a step without running it. fail=true also fails the run.
func (e *Engine) markStep(ctx context.Context, run Run, st *stepRow, state, code, action string, fail bool) error {
	return e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET state=?, error_code=? WHERE run_id=? AND ord=?`, state, code, run.ID, st.Ord); err != nil {
			return err
		}
		if err := e.Store.audit(ctx, tx, "engine", action, run.ID, map[string]any{"step": st.Ord, "op": st.Op, "connector": st.Connector, "code": code}); err != nil {
			return err
		}
		if fail {
			if err := e.Store.enqueue(ctx, tx, "provision.failed.v1", failedEvent(run, st, "rejected", e.now())); err != nil {
				return err
			}
			if run.Kind == KindLeaver {
				return nil // the other access-removal steps still run; finish marks the run failed
			}
			return e.failRun(ctx, tx, run)
		}
		return nil
	})
}

// failRun marks the run failed, or starts compensation when it created things worth undoing.
func (e *Engine) failRun(ctx context.Context, tx *sql.Tx, run Run) error {
	n := 0
	if run.Kind != KindLeaver { // a leaver is never "rolled back": access is not given back automatically
		res, err := tx.ExecContext(ctx, `UPDATE steps SET state='comp_pending', attempts=0, next_at=0 WHERE run_id=? AND state='done' AND comp_op<>''`, run.ID)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n = int(c)
	}
	state := RunFailed
	if n > 0 && !run.DryRun {
		state = RunCompensating
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET state=?, updated_at=? WHERE id=?`, state, e.now().Unix(), run.ID); err != nil {
		return err
	}
	return e.Store.audit(ctx, tx, "engine", "run."+state, run.ID, map[string]any{"kind": string(run.Kind), "party_id": run.PartyID, "compensating": n})
}

func completedEvent(run Run, st *stepRow, at time.Time) map[string]any {
	return map[string]any{"run_id": run.ID, "party_id": run.PartyID, "kind": string(run.Kind), "step": st.Ord, "op": st.Op, "connector": st.Connector, "completed_at": at.UTC().Format(time.RFC3339)}
}

func failedEvent(run Run, st *stepRow, reason string, at time.Time) map[string]any {
	return map[string]any{"run_id": run.ID, "party_id": run.PartyID, "kind": string(run.Kind), "step": st.Ord, "op": st.Op, "connector": st.Connector, "reason": reason, "failed_at": at.UTC().Format(time.RFC3339)}
}

// execute performs one step: persist "running" and the attempt, call, persist the outcome (with the
// audit entry and the event in the same transaction). It reports whether the run may continue.
func (e *Engine) execute(ctx context.Context, run Run, st *stepRow) (bool, error) {
	attempt := st.Attempts + 1
	if err := e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE steps SET state='running', attempts=? WHERE run_id=? AND ord=?`, attempt, run.ID, st.Ord)
		return err
	}); err != nil {
		return false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, e.Policy.StepTimeout)
	ext, skipCode, err := e.call(callCtx, run, st, fmt.Sprintf("%s:%d", run.ID, st.Ord), false)
	cancel()
	if ctx.Err() != nil {
		return false, ctx.Err() // shutting down mid-call: the step stays "running" and is retried on restart
	}
	now := e.now()
	if err == nil {
		return true, e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			state, code := StepDone, ""
			if skipCode != "" {
				state, code = StepSkipped, skipCode
			}
			if run.DryRun {
				state, code = StepSkipped, "dry_run"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE steps SET state=?, error_code=?, external_id=?, completed_at=? WHERE run_id=? AND ord=?`, state, code, ext, now.Unix(), run.ID, st.Ord); err != nil {
				return err
			}
			if state == StepDone {
				if err := e.recordEffect(ctx, tx, run, st, ext, false); err != nil {
					return err
				}
				if err := e.Store.enqueue(ctx, tx, "provision.step.completed.v1", completedEvent(run, st, now)); err != nil {
					return err
				}
			}
			return e.Store.audit(ctx, tx, "engine", "step."+state, run.ID, map[string]any{"step": st.Ord, "op": st.Op, "connector": st.Connector, "party_id": run.PartyID, "code": code, "attempt": attempt})
		})
	}
	code, perm := errCode(err)
	if errors.Is(err, ErrUnavailable) {
		code = "dependency_unavailable"
	}
	waiting := !perm && (code == "dependency_unavailable" || code == "core_unavailable" || code == "not_offboarded_yet")
	waits := st.waits
	if waiting {
		// can-core or the directory being down, slow or rate limiting (429), or the directory still
		// saying "offboarding", is not the step's fault: it never uses up an attempt (so a mass
		// offboarding cannot dead-letter itself) and backs off on its own counter, exponentially up
		// to max_delay, for as long as it lasts.
		attempt = st.Attempts
		waits++
	}
	if perm || attempt >= e.Policy.MaxAttempts {
		reason := "retries_exhausted"
		if perm {
			reason = "permanent"
		}
		return false, e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			state, action := StepDead, "step.dead"
			if st.Optional {
				state, action = StepSkipped, "step.skipped"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE steps SET state=?, error_code=? WHERE run_id=? AND ord=?`, state, code, run.ID, st.Ord); err != nil {
				return err
			}
			if err := e.Store.audit(ctx, tx, "engine", action, run.ID, map[string]any{"step": st.Ord, "op": st.Op, "connector": st.Connector, "party_id": run.PartyID, "code": code, "attempt": attempt}); err != nil {
				return err
			}
			if st.Optional {
				return nil
			}
			if err := e.Store.enqueue(ctx, tx, "provision.failed.v1", failedEvent(run, st, reason, now)); err != nil {
				return err
			}
			if run.Kind == KindLeaver {
				return nil // one dead step must not keep the other accounts enabled; finish marks the run failed
			}
			return e.failRun(ctx, tx, run)
		})
	}
	delay := e.Policy.backoff(attempt)
	if waiting {
		delay = e.Policy.backoff(waits)
	}
	return false, e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET state='pending', attempts=?, waits=?, error_code=?, next_at=? WHERE run_id=? AND ord=?`, attempt, waits, code, now.Add(delay).Unix(), run.ID, st.Ord); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='running', updated_at=? WHERE id=?`, now.Unix(), run.ID); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, "engine", "step.retry", run.ID, map[string]any{"step": st.Ord, "op": st.Op, "connector": st.Connector, "code": code, "attempt": attempt, "retry_in_s": int(delay.Seconds())})
	})
}

// call dispatches a step to its connector. It returns the external id of a created account, or a
// skip code for steps that have nothing to do (not an error).
func (e *Engine) call(ctx context.Context, run Run, st *stepRow, key string, comp bool) (ext, skip string, err error) {
	op := st.Op
	if comp {
		op = st.CompOp
	}
	if run.DryRun {
		return "", "", nil
	}
	switch st.Connector {
	case connCore:
		return e.revokeCore(ctx, run)
	case connTasks:
		return e.createTasks(ctx, run, st)
	}
	c, ok := e.Conns.Get(st.Connector)
	if !ok {
		return "", "", permanent("unknown_connector")
	}
	acctExt, state, err := e.Store.accountFor(ctx, run.PartyID, st.Connector)
	if err != nil {
		return "", "", err
	}
	if op != opCreateAccount && (acctExt == "" || state == "deleted") {
		return "", "no_account", nil // nothing to act on: already gone
	}
	if op == opCreateAccount {
		p, err := e.Dir.Get(ctx, run.PartyID)
		if err != nil {
			return "", "", err // fail closed: no directory, no account
		}
		if !p.Employed() {
			return "", "", permanent("not_employed")
		}
		created, err := c.CreateAccount(ctx, key, p.Identity())
		return created, "", err
	}
	switch op {
	case opUpdateAccount:
		p, err := e.Dir.Get(ctx, run.PartyID)
		if err != nil {
			return "", "", err
		}
		return acctExt, "", c.UpdateAccount(ctx, key, acctExt, p.Identity())
	case opDisableAccount:
		return acctExt, "", c.DisableAccount(ctx, key, acctExt)
	case opDeleteAccount:
		return acctExt, "", c.DeleteAccount(ctx, key, acctExt)
	case opAddGroup:
		return acctExt, "", c.AddToGroup(ctx, key, acctExt, st.Arg)
	case opRemoveGroup:
		return acctExt, "", c.RemoveFromGroup(ctx, key, acctExt, st.Arg)
	}
	return "", "", permanent("unknown_op")
}

// revokeCore is the kill switch: it ends the person's can-core tokens and sessions. can-core
// verifies that the people module reported the person offboarded and that the subject is theirs.
func (e *Engine) revokeCore(ctx context.Context, run Run) (string, string, error) {
	if e.Core == nil {
		return "", "no_core", nil // standalone: there is no can-core to revoke in
	}
	p, err := e.Dir.Get(ctx, run.PartyID)
	if err != nil {
		return "", "", err
	}
	if p.Status != "former" {
		// can-core refuses until the people module reports the person offboarded; wait for it.
		return "", "", transient("not_offboarded_yet")
	}
	if !strings.HasPrefix(p.SSOSubject, "user:") {
		return "", "no_core_account", nil // the person has no can-core account to revoke
	}
	if _, err := e.Core.RevokeIdentity(ctx, run.PartyID, p.SSOSubject); err != nil {
		if errors.Is(err, modclient.ErrUnavailable) {
			return "", "", transient("core_unavailable")
		}
		if errors.Is(err, modclient.ErrRevokeRefused) {
			return "", "", transient("core_refused") // bounded by the retry limit, then dead-lettered
		}
		return "", "", transient("core_error")
	}
	return "", "", nil
}

func (e *Engine) createTasks(ctx context.Context, run Run, st *stepRow) (string, string, error) {
	if e.Tasks == nil || st.Arg == "" {
		return "", "no_tasks", nil
	}
	if _, err := e.Tasks.Ensure(ctx, run.PartyID, st.Arg, ""); err != nil {
		if errors.Is(err, ErrInvalid) {
			return "", "", permanent("tasks_refused")
		}
		return "", "", transient("tasks_unavailable")
	}
	return "", "", nil
}

// recordEffect updates the module's own account and group records after a successful step.
func (e *Engine) recordEffect(ctx context.Context, tx *sql.Tx, run Run, st *stepRow, ext string, comp bool) error {
	op := st.Op
	if comp {
		op = st.CompOp
	}
	now := e.now().Unix()
	var err error
	switch op {
	case opCreateAccount:
		_, err = tx.ExecContext(ctx, `INSERT INTO accounts(party_id,connector,external_id,state,updated_at) VALUES (?,?,?,'active',?)
			ON CONFLICT(party_id,connector) DO UPDATE SET external_id=excluded.external_id, state='active', updated_at=excluded.updated_at`, run.PartyID, st.Connector, ext, now)
	case opDisableAccount:
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET state='disabled', updated_at=? WHERE party_id=? AND connector=?`, now, run.PartyID, st.Connector)
	case opDeleteAccount:
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET state='deleted', updated_at=? WHERE party_id=? AND connector=?`, now, run.PartyID, st.Connector); err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM applied_groups WHERE party_id=? AND connector=?`, run.PartyID, st.Connector)
		}
	case opAddGroup:
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO applied_groups(party_id,connector,grp) VALUES (?,?,?)`, run.PartyID, st.Connector, st.Arg)
	case opRemoveGroup:
		_, err = tx.ExecContext(ctx, `DELETE FROM applied_groups WHERE party_id=? AND connector=? AND grp=?`, run.PartyID, st.Connector, st.Arg)
	}
	return err
}

// compensateOne undoes one completed step of a failed run, newest first. done reports that the
// run has been closed; ran is 1 when a connector was called.
func (e *Engine) compensateOne(ctx context.Context, run Run, steps []stepRow, now int64) (done bool, ran int, err error) {
	var st *stepRow
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].State == StepCompPending {
			st = &steps[i]
			break
		}
	}
	if st == nil {
		failed := false
		for _, s := range steps {
			failed = failed || s.State == StepCompFailed
		}
		final := RunCompensated
		if failed {
			final = RunFailed
		}
		return true, 0, e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET state=?, updated_at=? WHERE id=?`, final, now, run.ID); err != nil {
				return err
			}
			return e.Store.audit(ctx, tx, "engine", "run."+final, run.ID, map[string]any{"kind": string(run.Kind), "party_id": run.PartyID})
		})
	}
	if st.nextAt > now {
		return false, 0, nil
	}
	attempt := st.Attempts + 1
	callCtx, cancel := context.WithTimeout(ctx, e.Policy.StepTimeout)
	_, _, cerr := e.call(callCtx, run, st, fmt.Sprintf("%s:%d:c", run.ID, st.Ord), true)
	cancel()
	if ctx.Err() != nil {
		return false, 1, ctx.Err()
	}
	t := e.now()
	return false, 1, e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		switch cerr {
		case nil:
			if _, err := tx.ExecContext(ctx, `UPDATE steps SET state='compensated', attempts=?, completed_at=? WHERE run_id=? AND ord=?`, attempt, t.Unix(), run.ID, st.Ord); err != nil {
				return err
			}
			if err := e.recordEffect(ctx, tx, run, st, "", true); err != nil {
				return err
			}
			return e.Store.audit(ctx, tx, "engine", "step.compensated", run.ID, map[string]any{"step": st.Ord, "op": st.CompOp, "connector": st.Connector})
		default:
			code, perm := errCode(cerr)
			if perm || attempt >= e.Policy.MaxAttempts {
				if _, err := tx.ExecContext(ctx, `UPDATE steps SET state='comp_failed', attempts=?, error_code=? WHERE run_id=? AND ord=?`, attempt, code, run.ID, st.Ord); err != nil {
					return err
				}
				if err := e.Store.enqueue(ctx, tx, "provision.failed.v1", failedEvent(run, st, "compensation_failed", t)); err != nil {
					return err
				}
				return e.Store.audit(ctx, tx, "engine", "step.comp_failed", run.ID, map[string]any{"step": st.Ord, "op": st.CompOp, "connector": st.Connector, "code": code})
			}
			if _, err := tx.ExecContext(ctx, `UPDATE steps SET attempts=?, error_code=?, next_at=? WHERE run_id=? AND ord=?`, attempt, code, t.Add(e.Policy.backoff(attempt)).Unix(), run.ID, st.Ord); err != nil {
				return err
			}
			return e.Store.audit(ctx, tx, "engine", "step.comp_retry", run.ID, map[string]any{"step": st.Ord, "code": code, "attempt": attempt})
		}
	})
}

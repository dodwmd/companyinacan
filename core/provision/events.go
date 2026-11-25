package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Event types this module consumes and publishes.
const (
	EventPersonCreated       = "person.created.v1"
	EventPersonStatusChanged = "person.status_changed.v1"
	EventPersonOffboarded    = "person.offboarded.v1"
	EventTaskCompleted       = "checklist.task_completed.v1"
	EventApprovalDecided     = "approval.decided.v1"
	EventStepCompleted       = "provision.step.completed.v1"
	EventFailed              = "provision.failed.v1"
)

// HandleEvent is the handler of can-core's signed webhooks. An error answers 5xx so can-core
// retries; a trigger that does not fit the person's real status is acknowledged and audited, not
// retried. Redeliveries are recognised by event id (persistently) and by the run idempotency keys.
func (e *Engine) HandleEvent(ctx context.Context, ev modclient.Event) error {
	fresh, err := e.Store.firstSeen(ctx, ev.ID)
	if err != nil {
		return err
	}
	if !fresh {
		return nil
	}
	if err := e.dispatch(ctx, ev); err != nil {
		e.Store.forgetSeen(ctx, ev.ID)
		return err
	}
	return nil
}

func (e *Engine) dispatch(ctx context.Context, ev modclient.Event) error {
	switch ev.Type {
	case EventPersonCreated:
		var d struct {
			PartyID string `json:"party_id"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || !partyRe.MatchString(d.PartyID) {
			return e.ignore(ctx, ev, "malformed")
		}
		if d.Status == "onboarding" || d.Status == "active" {
			return e.trigger(ctx, ev, StartRequest{Kind: KindJoiner, PartyID: d.PartyID})
		}
		return nil // a candidate has no access yet
	case EventPersonStatusChanged:
		var d struct {
			PartyID string `json:"party_id"`
			From    string `json:"from"`
			To      string `json:"to"`
			Date    string `json:"effective_date"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || !partyRe.MatchString(d.PartyID) {
			return e.ignore(ctx, ev, "malformed")
		}
		switch d.To {
		case "onboarding", "active":
			if d.From == "offboarding" { // offboarding was called off: stop a leaver that has not started
				return e.cancelWaitingLeaver(ctx, d.PartyID)
			}
			return e.trigger(ctx, ev, StartRequest{Kind: KindJoiner, PartyID: d.PartyID})
		case "offboarding":
			return e.trigger(ctx, ev, StartRequest{Kind: KindLeaver, PartyID: d.PartyID})
		case "former":
			return e.trigger(ctx, ev, StartRequest{Kind: KindLeaver, PartyID: d.PartyID, EffectiveDate: validDate(d.Date)})
		}
		return nil
	case EventPersonOffboarded:
		var d struct {
			PartyID string `json:"party_id"`
			Date    string `json:"effective_date"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || !partyRe.MatchString(d.PartyID) {
			return e.ignore(ctx, ev, "malformed")
		}
		return e.trigger(ctx, ev, StartRequest{Kind: KindLeaver, PartyID: d.PartyID, EffectiveDate: validDate(d.Date)})
	case EventTaskCompleted:
		var d struct {
			PartyID string `json:"party_id"`
			Action  string `json:"action"`
		}
		if json.Unmarshal(ev.Data, &d) != nil || !partyRe.MatchString(d.PartyID) {
			return nil
		}
		switch d.Action {
		case "identity.provision":
			return e.trigger(ctx, ev, StartRequest{Kind: KindJoiner, PartyID: d.PartyID})
		case "identity.deprovision", "identity.disable":
			return e.trigger(ctx, ev, StartRequest{Kind: KindLeaver, PartyID: d.PartyID}) // verified against the directory status
		}
		return nil
	case EventApprovalDecided:
		return e.approvalDecided(ctx, ev)
	}
	return nil
}

func validDate(s string) string {
	if dateRe.MatchString(s) {
		return s
	}
	return ""
}

// trigger starts a run for an event. Conflicts (the directory says otherwise) and unknown people
// are acknowledged; a directory that cannot answer is an error so the event is redelivered.
func (e *Engine) trigger(ctx context.Context, ev modclient.Event, req StartRequest) error {
	req.Trigger, req.Key = ev.Type, ev.ID
	_, _, err := e.Start(ctx, req)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrConflict), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid):
		return e.ignore(ctx, ev, "does_not_fit")
	}
	return err
}

func (e *Engine) ignore(ctx context.Context, ev modclient.Event, why string) error {
	return e.Store.Audit(ctx, "event:"+ev.Type, "trigger.ignored", ev.ID, map[string]any{"why": why})
}

func (e *Engine) cancelWaitingLeaver(ctx context.Context, party string) error {
	live, ok, err := e.Store.liveRun(ctx, party, KindLeaver)
	if err != nil || !ok || live.State != RunScheduled {
		return err
	}
	return e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// only a run in which nothing has happened yet may be called off
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM steps WHERE run_id=? AND state NOT IN ('pending')`, live.ID).Scan(&n); err != nil || n > 0 {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='cancelled', updated_at=? WHERE id=?`, e.now().Unix(), live.ID); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, "event:"+EventPersonStatusChanged, "run.cancelled", live.ID, map[string]any{"why": "offboarding_called_off"})
	})
}

// approvalDecided applies a signed decision to the step waiting for it. Approval needs a second
// person twice over: the engine's own check, and here a check that no approver is the person the
// run is about or whoever started the run. Anything else is rejected.
func (e *Engine) approvalDecided(ctx context.Context, ev modclient.Event) error {
	var d struct {
		ID        string   `json:"request_id"`
		Module    string   `json:"module_id"`
		Type      string   `json:"type"`
		Outcome   string   `json:"outcome"`
		Approvers []string `json:"approvers"`
	}
	if json.Unmarshal(ev.Data, &d) != nil || d.ID == "" || d.Type != "privileged-leaver" && d.Type != "privileged-access" {
		return nil
	}
	var runID string
	var ord int
	ctx2, cancel := e.Store.read(ctx)
	err := e.Store.db.QueryRowContext(ctx2, `SELECT run_id, ord FROM steps WHERE approval_id=?`, d.ID).Scan(&runID, &ord)
	cancel()
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not ours
	}
	if err != nil {
		return err
	}
	run, rows, err := e.Store.getRunRows(ctx, runID)
	if err != nil {
		return err
	}
	var st *stepRow
	for i := range rows {
		if rows[i].Ord == ord {
			st = &rows[i]
		}
	}
	if st == nil || st.State != StepAwaiting {
		return nil // already decided: a redelivery
	}
	if d.Outcome != "approved" {
		return e.markStep(ctx, run, st, StepRejected, "approval_"+d.Outcome, "run.step.rejected", true)
	}
	if e.approverIsInvolved(ctx, run, d.Approvers) {
		return e.markStep(ctx, run, st, StepRejected, "separation_of_duties", "run.step.rejected", true)
	}
	err = e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET state='pending', approved=1 WHERE run_id=? AND ord=? AND state='awaiting_approval'`, runID, ord); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='pending', updated_at=? WHERE id=? AND state='awaiting_approval'`, e.now().Unix(), runID); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, "event:"+ev.Type, "approval.granted", runID, map[string]any{"step": ord, "approval_id": d.ID, "approvers": len(d.Approvers)})
	})
	if err == nil {
		e.kick()
	}
	return err
}

// approverIsInvolved reports whether any approver is the subject of the run (by person id or by
// the account linked to them) or the initiator who started it.
func (e *Engine) approverIsInvolved(ctx context.Context, run Run, approvers []string) bool {
	involved := map[string]bool{canon("party:" + run.PartyID): true, canon("user:" + run.PartyID): true}
	if run.Initiator != "" {
		involved[canon(run.Initiator)] = true
		involved[canon("user:"+strings.TrimPrefix(run.Initiator, "user:"))] = true
	}
	if p, err := e.Dir.Get(ctx, run.PartyID); err == nil && p.SSOSubject != "" {
		involved[canon(p.SSOSubject)] = true
		involved[canon("user:"+strings.TrimPrefix(p.SSOSubject, "user:"))] = true
	} else if err != nil {
		return true // fail closed: cannot tell who the person is
	}
	if len(approvers) == 0 {
		return true
	}
	for _, a := range approvers {
		if involved[canon(a)] {
			return true
		}
	}
	return false
}

func canon(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ---- administrator actions ---------------------------------------------------------------------

// FlagUrgent marks a termination urgent: access ends now, not on the last day. Only a person who
// is offboarding or former can be flagged, and nobody can flag themselves.
func (e *Engine) FlagUrgent(ctx context.Context, actor, party string) error {
	p, err := e.verify(ctx, KindLeaver, party)
	if err != nil {
		return err
	}
	if actor == "" || canon(actor) == canon(p.ID) || (p.SSOSubject != "" && canon(strings.TrimPrefix(actor, "user:")) == canon(strings.TrimPrefix(p.SSOSubject, "user:"))) {
		return fmt.Errorf("%w: nobody can flag their own termination", ErrForbidden)
	}
	err = e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO urgent(party_id, flagged_by, flagged_at) VALUES (?,?,?)`, party, actor, e.now().Unix()); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, actor, "urgent.flag", party, nil)
	})
	if err != nil {
		return err
	}
	if live, ok, err := e.Store.liveRun(ctx, party, KindLeaver); err != nil {
		return err
	} else if ok {
		return e.releaseLeaver(ctx, live, StartRequest{Kind: KindLeaver, PartyID: party, Urgent: true, Initiator: actor, Trigger: "api"})
	}
	return nil
}

// RetryRun puts dead-lettered steps of a run back in the queue with fresh attempts.
func (e *Engine) RetryRun(ctx context.Context, actor, id string) error {
	run, rows, err := e.Store.getRunRows(ctx, id)
	if err != nil {
		return err
	}
	if run.State == RunSucceeded || run.State == RunCancelled || run.State == RunCompensating {
		return fmt.Errorf("%w: the run is %s", ErrConflict, run.State)
	}
	dead := 0
	for _, s := range rows {
		if s.State == StepDead || s.State == StepRejected {
			dead++
		}
	}
	if dead == 0 {
		return fmt.Errorf("%w: no dead-lettered step to retry", ErrConflict)
	}
	err = e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// a person may have a newer live run of the same kind: do not create a second
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET state='pending', attempts=0, next_at=0, error_code='', waits=0, gen=gen+1, approval_id='', approved=0 WHERE run_id=? AND state IN ('dead','rejected')`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='pending', updated_at=? WHERE id=?`, e.now().Unix(), id); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, actor, "run.retry", id, map[string]any{"steps": dead})
	})
	if isUnique(err) {
		return fmt.Errorf("%w: the person already has a live %s run", ErrConflict, run.Kind)
	}
	if err == nil {
		e.kick()
	}
	return err
}

// CancelRun stops a run that has not finished. Completed steps are not undone.
func (e *Engine) CancelRun(ctx context.Context, actor, id string) error {
	run, err := e.Store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if terminalRun(run.State) {
		return fmt.Errorf("%w: the run is %s", ErrConflict, run.State)
	}
	return e.Store.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='cancelled', updated_at=? WHERE id=?`, e.now().Unix(), id); err != nil {
			return err
		}
		return e.Store.audit(ctx, tx, actor, "run.cancelled", id, map[string]any{"party_id": run.PartyID})
	})
}

// ---- event delivery ----------------------------------------------------------------------------

// Publisher sends an event envelope through can-core (*modclient.Client).
type Publisher interface {
	PublishEnvelope(ctx context.Context, envelope []byte) error
}

// DeliverPending publishes the outbox through can-core, at least once. The envelope id is the
// publish source id, so a retry after a lost acknowledgement is not delivered twice.
func (e *Engine) DeliverPending(ctx context.Context, pub Publisher) (int, error) {
	items, err := e.Store.PendingEvents(ctx, 20)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		env, _ := json.Marshal(map[string]any{"id": it.ID, "type": it.Type, "data": json.RawMessage(it.Data)})
		ok := pub.PublishEnvelope(ctx, env) == nil
		if err := e.Store.MarkEvent(ctx, it.ID, ok); err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

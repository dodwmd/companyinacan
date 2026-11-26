package people

// Time off is decided by can-core's approval engine when the service runs with can-core
// (specs/070-approvals-inbox): submitting queues the request (core/modapproval) and names the
// requester's manager, the engine's decision applies it, and the module's own decision route
// approves only what the engine has approved (when applying failed, for example on the balance).
// Without can-core the service decides itself (DecideLeaveRequest). Review releases are not wired.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/dodwmd/companyinacan/core/modapproval"
	"github.com/dodwmd/companyinacan/core/modclient"
)

// apTimeOff is the approval type of modules/people/module.yaml.
const apTimeOff = "timeoff"

const approvalEvery = 15 * time.Second

// errDecidedInCore refuses a decision that belongs to can-core's approvals inbox.
var errDecidedInCore = &ConflictError{Msg: "this is decided in the approvals inbox of the admin UI"}

// UseCore wires can-core's approval engine. Call it before the service serves.
func (s *Service) UseCore(c modclient.Approvals, log *slog.Logger) {
	s.pump = &modapproval.Pump{Client: c, DB: s.db, Input: s.approvalInput, Reconcile: s.queueWaiting, Log: log, Now: func() time.Time { return s.now() }}
}

// SyncApprovals does one pass of the approval queue (the service does it on its own; tests call it).
func (s *Service) SyncApprovals(ctx context.Context) error {
	if s.pump == nil {
		return nil
	}
	return s.pump.Sync(ctx)
}

// queueLeave queues a request that has just become pending, in the caller's transaction. account is
// who asked, as the account is written.
func (s *Service) queueLeave(ctx context.Context, tx *sqlTx, id, personID, account string) error {
	var manager sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT m.sso_subject FROM people p JOIN people m ON m.id = p.manager_id WHERE p.id = ?`, personID).Scan(&manager)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !modapproval.IsAccount(manager.String) {
		manager.String = "" // no account: the engine's HR step decides
	}
	_, err = modapproval.Enqueue(ctx, tx, apTimeOff, id, 0, account, manager.String, s.now())
	return err
}

func (s *Service) withdrawLeave(ctx context.Context, tx *sqlTx, id string) error {
	if s.pump == nil {
		return nil
	}
	return modapproval.Withdraw(ctx, tx, apTimeOff, id)
}

// engineAllows says whether the module's own route may decide a pending request. Approving needs the
// engine's approval; rejecting is refused only while the request is open in the inbox.
func (s *Service) engineAllows(ctx context.Context, tx *sqlTx, id string, approve bool) error {
	if s.pump == nil {
		return nil
	}
	row, found, err := modapproval.Get(ctx, tx, apTimeOff, id)
	if err != nil {
		return err
	}
	if !approve {
		if found && row.Status == modapproval.Pending {
			return errDecidedInCore
		}
		return nil
	}
	if !found || row.Status != modapproval.Approved {
		return errDecidedInCore
	}
	return nil
}

// ApprovalState says where a pending request stands with the engine ("" without one), and why for
// refused and unapplied ones. For the web UI: the caller has already been allowed to read it.
func (s *Service) ApprovalState(ctx context.Context, id string) (engine bool, status, note string, err error) {
	if s.pump == nil {
		return false, "", "", nil
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	row, found, err := modapproval.Get(ctx, s.db, apTimeOff, id)
	if err == nil && !found {
		row.Status = modapproval.Queued
	}
	return true, row.Status, row.Note, err
}

// queueWaiting queues pending requests without a row: those from before the upgrade, or restored
// from a backup.
func (s *Service) queueWaiting(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.person_id, CASE WHEN r.submitted_account <> '' THEN r.submitted_account
		ELSE COALESCE(NULLIF(p.sso_subject, ''), NULLIF(r.submitted_by, ''), 'unknown') END
		FROM leave_requests r JOIN people p ON p.id = r.person_id
		WHERE r.status = 'pending' AND NOT EXISTS (SELECT 1 FROM approval_queue q WHERE q.status <> 'cancel' AND q.kind = 'timeoff' AND q.item_id = r.id)
		ORDER BY r.created_at, r.id LIMIT 500`)
	if err != nil {
		return err
	}
	type item struct{ id, person, account string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.person, &it.account); err != nil {
			_ = rows.Close()
			return err
		}
		items = append(items, it)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, it := range items {
		if err := s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
			return s.queueLeave(ctx, tx, it.id, it.person, it.account)
		}); err != nil {
			return err
		}
	}
	return nil
}

// approvalInput is what can-core holds about a time off request: its id and nothing else. Dates and
// the leave type can reveal absence or health, so the approver opens the request in this module.
func (s *Service) approvalInput(ctx context.Context, row modapproval.Row) (modclient.ApprovalInput, bool, error) {
	var in modclient.ApprovalInput
	ctx, cancel := readCtx(ctx)
	defer cancel()
	r, err := s.loadLeave(ctx, s.db, row.ItemID)
	if errors.Is(err, ErrNotFound) || (err == nil && r.Status != "pending") {
		return in, false, nil
	}
	if err != nil {
		return in, false, err
	}
	in.Title = "Time off request " + r.ID
	in.Payload, err = json.Marshal(map[string]string{"module_path": "/timeoff/" + r.ID})
	return in, true, err
}

// engineActor is the identity under which the engine's decisions are recorded: it holds no role and
// no permission.
func (s *Service) engineActor(ctx context.Context, q querier, subject string) Actor {
	return Actor{Subject: subject, PersonID: s.personIDForSubject(ctx, q, subject)}
}

// applyDecision applies the engine's decision to the request that asked for it. Unknown refs and
// requests no longer pending are ignored. An approval that cannot be applied (balance, calendar) is
// kept for the module's decision route; only a failure worth retrying is returned.
func (s *Service) applyDecision(ctx context.Context, d modapproval.Decision) error {
	var started bool
	var unapplied error
	core := Actor{Subject: "can-core"}
	var resource string
	err := s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
		row, found, err := modapproval.ByRef(ctx, tx, d.Ref)
		if err != nil || !found || row.Kind != d.Type || (row.ApprovalID != "" && row.ApprovalID != d.ApprovalID) || row.Status == modapproval.Approved {
			return err
		}
		resource = "leave-request:" + row.ItemID
		r, err := s.loadLeave(ctx, tx, row.ItemID)
		if errors.Is(err, ErrNotFound) || (err == nil && r.Status != "pending") {
			return modapproval.Drop(ctx, tx, row.Ref)
		}
		if err != nil {
			return err
		}
		now := s.now().Unix()
		switch d.Outcome {
		case "approved", "rejected":
		default: // cancelled or expired: back to a draft, which can be submitted again
			if _, err := tx.ExecContext(ctx, `UPDATE leave_requests SET status='draft', updated_at=? WHERE id=?`, now, r.ID); err != nil {
				return err
			}
			if err := s.auditIn(ctx, tx, core, "leave.approval_"+d.Outcome, resource, nil); err != nil {
				return err
			}
			return modapproval.Drop(ctx, tx, row.Ref)
		}
		approve := d.Outcome == "approved"
		by := core.Subject
		if len(d.Approvers) > 0 {
			by = d.Approvers[0]
		}
		approver := s.engineActor(ctx, tx, by)
		if approve {
			chain := append(s.idents(ctx, tx, r.PersonID), r.submittedBy, row.Requester)
			if len(d.Approvers) == 0 || slices.ContainsFunc(d.Approvers, func(x string) bool { return !sodOK(s.engineActor(ctx, tx, x), chain...) }) {
				approve = false // separation of duties, or nobody named: nothing is approved
				if err := s.auditIn(ctx, tx, core, "leave.approval_refused_sod", resource, nil); err != nil {
					return err
				}
			}
		}
		decided, err := s.decidedEvent(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if !approve {
			if err := s.rejectLeave(ctx, tx, r.ID, approver.auditIDOrSubject(), decided); err != nil {
				return err
			}
		} else {
			if started, err = s.approveLeave(ctx, tx, r, approver, decided); err != nil {
				if errors.Is(err, ErrConflict) {
					unapplied = err
				}
				return err
			}
		}
		if err := s.auditIn(ctx, tx, approver, "leave.decide", resource, nil); err != nil {
			return err
		}
		return modapproval.Drop(ctx, tx, row.Ref)
	})
	if unapplied != nil {
		return s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
			if err := modapproval.Settle(ctx, tx, d.Ref, modapproval.Approved, "could not be applied: "+unapplied.Error()); err != nil {
				return err
			}
			return s.auditIn(ctx, tx, core, "leave.approval_not_applied", resource, nil)
		})
	}
	if err == nil && started {
		s.reconcileLogged(ctx)
	}
	return err
}

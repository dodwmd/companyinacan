package finance

// Timesheets, expenses and purchase orders are decided by can-core's approval engine
// (specs/070-approvals-inbox). Without can-core the service decides itself, as before. With it:
// submitting queues the record (core/modapproval), the engine's decision applies it, and the
// module's own approve route works only for a record the engine has approved (when applying the
// decision failed, for example in a locked period).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modapproval"
	"github.com/dodwmd/companyinacan/core/modclient"
)

// Approval types (modules/finance/module.yaml `approvals`).
const (
	apTimesheet     = "timesheet"
	apExpense       = "expense"
	apPurchaseOrder = "purchase-order"
)

const approvalEvery = 15 * time.Second

// ErrDecidedInCore refuses a decision that belongs to can-core's approvals inbox.
var ErrDecidedInCore = fmt.Errorf("%w: this is decided in the approvals inbox of the admin UI", ErrConflict)

// sodReason is recorded when an approver of the engine had also made the record.
const sodReason = "separation of duties: an approver also created or edited this record"

// useCore wires the engine: records that wait are queued for it.
func (s *Store) useCore(c modclient.Approvals) {
	s.core = c
	s.pump = &modapproval.Pump{Client: c, DB: s.db, Input: s.approvalInput, Reconcile: s.queueWaiting, Now: func() time.Time { return s.now() }}
}

// SyncApprovals does one pass of the approval queue (the service does it on its own; tests and
// tooling call it).
func (s *Store) SyncApprovals(ctx context.Context) error {
	if s.pump == nil {
		return nil
	}
	return s.pump.Sync(ctx)
}

// requestDecision queues a record that has just started waiting, in the caller's transaction.
func (s *Store) requestDecision(ctx context.Context, tx *sql.Tx, kind string, id, version int64, requester string) error {
	if s.pump == nil {
		return nil
	}
	_, err := modapproval.Enqueue(ctx, tx, kind, strconv.FormatInt(id, 10), version, requester, "", s.now())
	return err
}

func (s *Store) withdrawDecision(ctx context.Context, tx *sql.Tx, kind string, id int64) error {
	if s.pump == nil {
		return nil
	}
	return modapproval.Withdraw(ctx, tx, kind, strconv.FormatInt(id, 10))
}

// engineAllows says whether the module's own routes may decide a waiting record. Approving needs the
// engine's approval; sending back is refused only while the request is open in the inbox, so that a
// record is not stuck when can-core cannot take the request.
func (s *Store) engineAllows(ctx context.Context, tx *sql.Tx, kind string, id, version int64, a *Principal, approve bool) error {
	if s.pump == nil {
		return nil
	}
	row, found, err := modapproval.Get(ctx, tx, kind, strconv.FormatInt(id, 10))
	if err != nil {
		return err
	}
	if !approve {
		if found && row.Status == modapproval.Pending {
			return ErrDecidedInCore
		}
		return nil
	}
	if !found || row.Status != modapproval.Approved || (kind == apPurchaseOrder && row.Version != version) {
		return ErrDecidedInCore
	}
	if sameSubject(row.Requester, a.Subject) {
		return ErrSeparation
	}
	return nil
}

// queueWaiting queues submitted records without a row: those that were already waiting when the
// module started using the engine (an upgrade) or restored from a backup. Rows of records that are
// gone (retention, erasure) are withdrawn.
func (s *Store) queueWaiting(ctx context.Context) error {
	const open = ` FROM approval_queue q WHERE q.status <> 'cancel' AND q.kind = ? AND q.item_id = CAST(t.id AS TEXT)`
	type item struct {
		kind, requester string
		id              int64
	}
	var items []item
	for _, w := range []struct{ kind, query string }{
		{apTimesheet, `SELECT t.id, CASE WHEN t.submitted_by <> '' THEN t.submitted_by ELSE t.owner END FROM timesheets t WHERE t.status = 'submitted' AND NOT EXISTS (SELECT 1` + open + `) ORDER BY t.id LIMIT 500`},
		{apExpense, `SELECT t.id, t.owner FROM expenses t WHERE t.status = 'submitted' AND NOT EXISTS (SELECT 1` + open + `) ORDER BY t.id LIMIT 500`},
	} {
		rows, err := s.db.QueryContext(ctx, w.query, w.kind)
		if err != nil {
			return err
		}
		for rows.Next() {
			it := item{kind: w.kind}
			if err := rows.Scan(&it.id, &it.requester); err != nil {
				_ = rows.Close()
				return err
			}
			items = append(items, it)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	for _, it := range items {
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			return s.requestDecision(ctx, tx, it.kind, it.id, 0, it.requester)
		})
		if err != nil {
			return err
		}
	}
	var gone []modapproval.Row
	rows, err := s.db.QueryContext(ctx, `SELECT kind, item_id FROM approval_queue q WHERE status <> 'cancel' AND (
		(kind = 'timesheet' AND NOT EXISTS (SELECT 1 FROM timesheets t WHERE CAST(t.id AS TEXT) = q.item_id AND t.status = 'submitted')) OR
		(kind = 'expense' AND NOT EXISTS (SELECT 1 FROM expenses t WHERE CAST(t.id AS TEXT) = q.item_id AND t.status = 'submitted')) OR
		(kind = 'purchase-order' AND NOT EXISTS (SELECT 1 FROM purchase_orders t WHERE CAST(t.id AS TEXT) = q.item_id AND t.status = 'draft'))) LIMIT 500`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r modapproval.Row
		if err := rows.Scan(&r.Kind, &r.ItemID); err != nil {
			_ = rows.Close()
			return err
		}
		gone = append(gone, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range gone {
		if err := s.inTx(ctx, func(tx *sql.Tx) error { return modapproval.Withdraw(ctx, tx, r.Kind, r.ItemID) }); err != nil {
			return err
		}
	}
	return nil
}

// approvalInput is what the approver sees: ids, dates and amounts, never free text or names (the
// record itself is opened in finance through module_path).
func (s *Store) approvalInput(ctx context.Context, r modapproval.Row) (modclient.ApprovalInput, bool, error) {
	var in modclient.ApprovalInput
	id, err := strconv.ParseInt(r.ItemID, 10, 64)
	if err != nil {
		return in, false, nil
	}
	var payload map[string]string
	switch r.Kind {
	case apTimesheet:
		t, err := s.loadTimesheet(ctx, s.db, &Principal{}, id)
		if errors.Is(err, ErrNotFound) || (err == nil && t.Status != TsSubmitted) {
			return in, false, nil
		}
		if err != nil {
			return in, false, err
		}
		in.Title = fmt.Sprintf("Timesheet %d, week of %s", id, t.WeekStart)
		payload = map[string]string{"week_start": t.WeekStart, "hours": fmt.Sprintf("%d:%02d", t.TotalMinutes/60, t.TotalMinutes%60), "module_path": fmt.Sprintf("/timesheets/%d", id)}
	case apExpense:
		e, err := s.loadExpense(ctx, s.db, &Principal{}, id)
		if errors.Is(err, ErrNotFound) || (err == nil && e.Status != ExSubmitted) {
			return in, false, nil
		}
		if err != nil {
			return in, false, err
		}
		in.Title, in.Amount = fmt.Sprintf("Expense %d", id), e.AmountMinor
		payload = map[string]string{"currency": e.Currency, "category": e.Category, "incurred_on": e.IncurredOn, "module_path": fmt.Sprintf("/expenses/%d", id)}
	case apPurchaseOrder:
		po, err := s.loadPO(ctx, s.db, &Principal{}, id)
		if errors.Is(err, ErrNotFound) || (err == nil && (po.Status != PoDraft || po.Version != r.Version)) {
			return in, false, nil
		}
		if err != nil {
			return in, false, err
		}
		in.Title, in.Amount = fmt.Sprintf("Purchase order draft %d", id), po.TotalMinor
		payload = map[string]string{"currency": po.Currency, "module_path": fmt.Sprintf("/purchase-orders/%d", id)}
	default:
		return in, false, nil
	}
	in.Payload, err = json.Marshal(payload)
	return in, true, err
}

// engineView is what a record's page shows about the engine's say on it.
type engineView struct {
	On          bool   // the engine decides (installed with can-core)
	State, Note string // while the record waits: queued, pending, refused, approved or rejected
}

// MayApprove: the module's own approve button works.
func (v engineView) MayApprove() bool { return !v.On || v.State == modapproval.Approved }

// MaySendBack: the module's own reject button works.
func (v engineView) MaySendBack() bool { return !v.On || v.State != modapproval.Pending }

func (s *Store) engineView(ctx context.Context, kind string, id int64, waiting bool) (engineView, error) {
	v := engineView{On: s.pump != nil}
	if !v.On {
		return v, nil
	}
	var err error
	if v.State, v.Note, err = s.ApprovalState(ctx, kind, id); err != nil {
		return v, err
	}
	if v.State == "" && waiting {
		v.State = modapproval.Queued
	}
	return v, nil
}

// RequestPurchaseOrderApproval asks the engine to approve a draft purchase order (like an invoice:
// finance.write asks, and cannot be the one who approves). Asking again for the same version while
// the request waits returns it.
func (s *Store) RequestPurchaseOrderApproval(ctx context.Context, a *Principal, id int64) (*CoreApproval, error) {
	if s.pump == nil {
		return nil, fmt.Errorf("%w: approvals through can-core are not configured on this installation", ErrConflict)
	}
	if !a.Can(PermWrite) {
		return nil, ErrForbidden
	}
	var ref string
	_, err := s.withPO(ctx, a, id, nil, func(tx *sql.Tx, po *PurchaseOrder) error {
		if po.Status != PoDraft {
			return fmt.Errorf("%w: only a draft needs approval (the purchase order is %s)", ErrConflict, po.Status)
		}
		row, found, err := modapproval.Get(ctx, tx, apPurchaseOrder, strconv.FormatInt(id, 10))
		if err != nil {
			return err
		}
		if found && row.Version == po.Version && row.Status != modapproval.Rejected {
			ref = row.Ref
			return nil
		}
		ref, err = modapproval.Enqueue(ctx, tx, apPurchaseOrder, strconv.FormatInt(id, 10), po.Version, a.Subject, "", s.now())
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "purchase_order.approval_request", fmt.Sprint(id), map[string]any{})
	})
	if err != nil {
		return nil, err
	}
	row, err := s.pump.SendRef(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v (the request is queued and will be sent)", ErrApprovalsUnavailable, err)
	}
	if row.Status == modapproval.Refused {
		return nil, invalid("approval", "can-core refused the approval request: %s", row.Note)
	}
	return &CoreApproval{ApprovalID: row.ApprovalID, Status: row.Status}, nil
}

// ApprovalState says where a record stands with the engine, for the web UI: "" when it has no open
// request, else a modapproval status and, for refused and unapplied ones, why.
func (s *Store) ApprovalState(ctx context.Context, kind string, id int64) (status, note string, err error) {
	if s.pump == nil {
		return "", "", nil
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	row, _, err := modapproval.Get(rctx, s.rdb, kind, strconv.FormatInt(id, 10))
	return row.Status, row.Note, err
}

// applyDecision applies the engine's decision to the record that asked for it. Unknown refs and
// records that no longer wait are ignored (a redelivery, a withdrawn request). A decision that cannot
// be applied (locked period, ledger) is kept, so that the module's own routes can finish it; only a
// database failure is returned, which makes can-core deliver again.
func (s *Store) applyDecision(ctx context.Context, d modapproval.Decision) error {
	var kept struct{ status, note string }
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		row, found, err := modapproval.ByRef(ctx, tx, d.Ref)
		if err != nil || !found || row.Kind != d.Type || (row.ApprovalID != "" && row.ApprovalID != d.ApprovalID) {
			return err
		}
		if row.Status == modapproval.Approved || row.Status == modapproval.Rejected {
			return nil // recorded before
		}
		id, err := strconv.ParseInt(row.ItemID, 10, 64)
		if err != nil {
			return modapproval.Drop(ctx, tx, row.Ref)
		}
		approve := d.Outcome == "approved"
		by, reason := "", "rejected in the approvals inbox (request "+d.ApprovalID+")"
		if len(d.Approvers) > 0 {
			by = d.Approvers[0]
		}
		kept.status = modapproval.Rejected
		if approve {
			kept.status, reason = modapproval.Approved, ""
		}
		if d.Outcome != "approved" && d.Outcome != "rejected" { // cancelled or expired: ask again
			return s.giveBack(ctx, tx, row, id, d)
		}
		if approve && (by == "" || slices.ContainsFunc(d.Approvers, func(x string) bool { return sameSubject(x, row.Requester) })) {
			approve, reason, kept.status = false, sodReason, modapproval.Rejected
		}
		if by == "" {
			by = "can-core"
		}
		sentBack := func(involved ...string) {
			if approve && slices.ContainsFunc(d.Approvers, func(x string) bool {
				return slices.ContainsFunc(involved, func(y string) bool { return y != "" && sameSubject(x, y) })
			}) {
				approve, reason, kept.status = false, sodReason, modapproval.Rejected
			}
		}
		// Applying may fail for a business reason (kept.note); the transaction is then rolled back.
		fail := func(err error) error {
			kept.note = err.Error()
			return err
		}
		switch row.Kind {
		case apTimesheet:
			t, err := s.loadTimesheet(ctx, tx, &Principal{}, id)
			if errors.Is(err, ErrNotFound) || (err == nil && t.Status != TsSubmitted) {
				return modapproval.Drop(ctx, tx, row.Ref)
			}
			if err != nil {
				return err
			}
			sentBack(t.Owner, t.CreatedBy, t.updatedBy, t.submittedBy)
			if err := s.applyTimesheet(ctx, tx, t, approve, by, reason); err != nil {
				return fail(err)
			}
		case apExpense:
			e, err := s.loadExpense(ctx, tx, &Principal{}, id)
			if errors.Is(err, ErrNotFound) || (err == nil && e.Status != ExSubmitted) {
				return modapproval.Drop(ctx, tx, row.Ref)
			}
			if err != nil {
				return err
			}
			sentBack(e.Owner)
			if err := s.applyExpense(ctx, tx, e, approve, by, reason); err != nil {
				return fail(err)
			}
		case apPurchaseOrder:
			po, err := s.loadPO(ctx, tx, &Principal{}, id)
			if errors.Is(err, ErrNotFound) || (err == nil && (po.Status != PoDraft || po.Version != row.Version)) {
				return modapproval.Drop(ctx, tx, row.Ref)
			}
			if err != nil {
				return err
			}
			sentBack(po.CreatedBy, po.updatedBy)
			if !approve {
				// The draft stays; the row says it was turned down until approval is asked again.
				if err := modapproval.Settle(ctx, tx, row.Ref, modapproval.Rejected, reason); err != nil {
					return err
				}
				return s.audit(ctx, tx, by, "purchase_order.rejected", fmt.Sprint(id), map[string]any{"approval_id": d.ApprovalID})
			}
			if err := s.applyPO(ctx, tx, po, by); err != nil {
				return fail(err)
			}
		default:
			return modapproval.Drop(ctx, tx, row.Ref)
		}
		if err := s.audit(ctx, tx, "can-core", row.Kind+".approval_"+d.Outcome, row.ItemID, map[string]any{"approval_id": d.ApprovalID, "approvers": d.Approvers}); err != nil {
			return err
		}
		return modapproval.Drop(ctx, tx, row.Ref)
	})
	if err != nil && kept.note != "" && businessError(err) {
		return s.inTx(ctx, func(tx *sql.Tx) error {
			if err := modapproval.Settle(ctx, tx, d.Ref, kept.status, "could not be applied: "+kept.note); err != nil {
				return err
			}
			return s.audit(ctx, tx, "can-core", "approval.not_applied", d.Ref, map[string]any{"approval_id": d.ApprovalID, "error": kept.note})
		})
	}
	return err
}

// giveBack returns a record whose request ended without a decision (withdrawn in the inbox, expired)
// to its draft, from where it can be submitted again.
func (s *Store) giveBack(ctx context.Context, tx *sql.Tx, row modapproval.Row, id int64, d modapproval.Decision) error {
	why := "the approval request was " + strings.TrimSpace(d.Outcome) + ": submit again"
	var err error
	switch row.Kind {
	case apTimesheet:
		_, err = tx.ExecContext(ctx, `UPDATE timesheets SET status = 'draft', reject_reason = ?, version = version + 1, updated_at = ? WHERE id = ? AND status = 'submitted'`, why, ts(s.now()), id)
	case apExpense:
		_, err = tx.ExecContext(ctx, `UPDATE expenses SET status = 'draft', reject_reason = ?, version = version + 1, updated_at = ? WHERE id = ? AND status = 'submitted'`, why, ts(s.now()), id)
	}
	if err != nil {
		return err
	}
	if err := s.audit(ctx, tx, "can-core", row.Kind+".approval_"+d.Outcome, row.ItemID, map[string]any{"approval_id": d.ApprovalID}); err != nil {
		return err
	}
	return modapproval.Drop(ctx, tx, row.Ref)
}

// businessError tells a rule the record breaks from a failure worth retrying.
func businessError(err error) bool {
	var inv *InvalidError
	return errors.Is(err, ErrLocked) || errors.Is(err, ErrConflict) || errors.As(err, &inv)
}

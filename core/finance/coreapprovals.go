package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/dodwmd/companyinacan/core/modapproval"
	"github.com/dodwmd/companyinacan/core/modclient"
)

// ErrApprovalsUnavailable is returned when can-core's approvals engine cannot be reached.
var ErrApprovalsUnavailable = errors.New("approvals are unavailable: can-core did not answer")

// CoreApproval is the state of an invoice's can-core approval request.
type CoreApproval struct {
	ApprovalID string `json:"approval_id"`
	Status     string `json:"status"`
}

type coreRow struct {
	approvalID, status, requestedBy string
	version                         int64
	attempt                         int
}

type txCtxKey struct{}

// withTx makes the transaction the Approver reads through: the store has a single database
// connection, so a query on s.db while the approval's own transaction is open would wait forever.
func withTx(ctx context.Context, q dbq) context.Context { return context.WithValue(ctx, txCtxKey{}, q) }

func (s *Store) coreRow(ctx context.Context, ref string) (coreRow, bool, error) {
	var r coreRow
	var q dbq = s.db
	if tx, ok := ctx.Value(txCtxKey{}).(dbq); ok {
		q = tx
	}
	err := q.QueryRowContext(ctx, `SELECT approval_id, status, requested_by, version, attempt FROM core_approvals WHERE ref = ?`, ref).
		Scan(&r.approvalID, &r.status, &r.requestedBy, &r.version, &r.attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// RequestInvoiceApproval opens a can-core approval request for a draft invoice (type `invoice` in
// modules/finance/module.yaml). Asking again for the same version while the request is pending or
// approved returns it; after a rejection, or after the invoice was edited, a new request is opened.
// The requester is the caller, who then cannot be the one who approves.
func (s *Store) RequestInvoiceApproval(ctx context.Context, a *Principal, id int64) (*CoreApproval, error) {
	if s.core == nil {
		return nil, fmt.Errorf("%w: approvals through can-core are not configured on this installation", ErrConflict)
	}
	if !a.Can(PermWrite) {
		return nil, ErrForbidden
	}
	if !canSubjectRe.MatchString(a.Subject) {
		return nil, invalid("approval", "requesting an approval through can-core needs a signed-in person: use a personal access token, not the bootstrap or a service token")
	}
	v, err := s.GetInvoice(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if v.Status != StDraft {
		return nil, fmt.Errorf("%w: only a draft needs approval (the invoice is %s)", ErrConflict, v.Status)
	}
	ref := fmt.Sprintf("invoice:%d", id)
	prev, found, err := s.coreRow(ctx, ref)
	if err != nil {
		return nil, err
	}
	if found && prev.version == v.Version && (prev.status == "pending" || prev.status == "approved") {
		return &CoreApproval{ApprovalID: prev.approvalID, Status: prev.status}, nil
	}
	attempt := prev.attempt + 1
	ap, err := s.core.CreateApproval(ctx, modclient.ApprovalInput{
		Type: "invoice", Requester: a.Subject, Ref: ref, Title: fmt.Sprintf("Invoice draft %d", id), Amount: v.TotalMinor,
		Key: fmt.Sprintf("invoice:%d:v%d:n%d", id, v.Version, attempt),
	})
	switch {
	case errors.Is(err, modclient.ErrApprovalRefused):
		return nil, invalid("approval", "can-core refused the approval request: %v", err)
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrApprovalsUnavailable, err)
	}
	if ap.Status != "pending" && ap.Status != "approved" {
		return nil, invalid("approval", "can-core answered status %q", ap.Status)
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO core_approvals (ref, approval_id, version, attempt, status, requested_by, created_at) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT (ref) DO UPDATE SET approval_id = excluded.approval_id, version = excluded.version, attempt = excluded.attempt, status = excluded.status,
			requested_by = excluded.requested_by, created_at = excluded.created_at, decided_at = NULL`,
			ref, ap.ID, v.Version, attempt, ap.Status, a.Subject, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "invoice.approval_request", ref, map[string]any{"approval_id": ap.ID})
	})
	if err != nil {
		return nil, err
	}
	return &CoreApproval{ApprovalID: ap.ID, Status: ap.Status}, nil
}

// RecordCoreDecision stores the outcome of a can-core approval request. It ignores requests it does
// not know and requests no longer pending (a redelivered event). approvers are the subjects can-core
// says approved: if the requester is among them the outcome is recorded as rejected, whatever the
// engine said (defence in depth for separation of duties).
func (s *Store) RecordCoreDecision(ctx context.Context, approvalID, outcome string, approvers []string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var by string
		err := tx.QueryRowContext(ctx, `SELECT requested_by FROM core_approvals WHERE approval_id = ? AND status = 'pending'`, approvalID).Scan(&by)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		status := "rejected" // rejected, cancelled and expired all send the requester back to ask again
		if outcome == "approved" && !slices.Contains(approvers, by) {
			status = "approved"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE core_approvals SET status = ?, decided_at = ? WHERE approval_id = ? AND status = 'pending'`, status, ts(s.now()), approvalID); err != nil {
			return err
		}
		return s.audit(ctx, tx, "can-core", "invoice.approval_"+status, approvalID, nil)
	})
}

// CoreApprover is the Approver for invoices backed by can-core: an invoice may be approved only when
// its current version has an approved can-core request, and never by whoever requested it. Expenses,
// timesheets and purchase orders are held to the engine by Store.engineAllows (queued.go); voiding
// and journals keep the permission rule.
type CoreApprover struct{ store *Store }

// CanApprove implements Approver. It fails closed on any error.
func (c *CoreApprover) CanApprove(ctx context.Context, a *Principal, req ApprovalRequest) (bool, error) {
	if !a.Can(req.perm()) {
		return false, nil
	}
	if req.Kind != "invoice" || req.Ref == "" {
		return true, nil
	}
	row, found, err := c.store.coreRow(ctx, req.Ref)
	if err != nil || !found {
		return false, err
	}
	return row.status == "approved" && row.version == req.Version && !sameSubject(row.requestedBy, a.Subject), nil
}

// CanEventHandler handles the events can-core delivers to this module (manifest `subscribes`).
func (s *Store) CanEventHandler() func(context.Context, modclient.Event) error {
	return func(ctx context.Context, ev modclient.Event) error {
		if ev.Type != "approval.decided.v1" || ev.Source != "can-core" {
			return nil
		}
		if q, ok := modapproval.Decode(ev); ok && (q.Type == apTimesheet || q.Type == apExpense || q.Type == apPurchaseOrder) {
			return s.applyDecision(ctx, q)
		}
		var d struct {
			RequestID string   `json:"request_id"`
			Type      string   `json:"type"`
			Outcome   string   `json:"outcome"`
			Approvers []string `json:"approvers"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.RequestID == "" || d.Type != "invoice" {
			return nil // malformed or not ours: nothing to retry
		}
		return s.RecordCoreDecision(ctx, d.RequestID, d.Outcome, d.Approvers)
	}
}

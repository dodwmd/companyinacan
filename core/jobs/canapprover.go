package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// CoreApprover is the Approver backed by can-core's approvals engine (type `requisition` in
// modules/jobs/module.yaml). The decision is not immediate: it arrives later as the signed
// approval.decided.v1 event (Store.ApplyApprovalDecision). Separation of duties is the engine's
// (nobody approves their own request) and is kept in Store.DecideApproval for manual decisions.
type CoreApprover struct{ Client modclient.Approvals }

// Request implements Approver. It fails closed: when can-core cannot be reached or refuses, the
// requisition stays without approval and the caller is told.
func (a CoreApprover) Request(ctx context.Context, j *Job, requestedBy string) (string, string, error) {
	if !canSubjectRe.MatchString(requestedBy) {
		return "", "", invalid("approval", "requesting an approval through can-core needs a signed-in person: use a personal access token, not the bootstrap or a service token")
	}
	ap, err := a.Client.CreateApproval(ctx, modclient.ApprovalInput{
		Type: "requisition", Requester: requestedBy, Ref: "job:" + j.ID, Title: "Requisition " + j.ID,
		Key: fmt.Sprintf("job:%s:v%d", j.ID, j.Version),
	})
	switch {
	case errors.Is(err, modclient.ErrApprovalRefused):
		return "", "", invalid("approval", "can-core refused the approval request: %v", err)
	case err != nil:
		return "", "", fmt.Errorf("approvals are unavailable: %w", err)
	}
	switch ap.Status {
	case "pending":
		return ap.ID, ApprovalPending, nil
	case "approved":
		return ap.ID, ApprovalApproved, nil
	}
	return "", "", invalid("approval", "can-core answered status %q", ap.Status)
}

// ApplyApprovalDecision records the outcome of a can-core approval request on the requisition that
// asked for it. It is idempotent and ignores requests it does not know or that are no longer
// pending (a redelivered event, a withdrawn request). approvers are the subjects can-core says
// approved: if the person who requested the approval is among them the outcome is recorded as
// rejected, whatever the engine said (defence in depth for separation of duties).
func (s *Store) ApplyApprovalDecision(ctx context.Context, approvalID, outcome string, approvers []string) error {
	state := ApprovalRejected // rejected, cancelled and expired all send the requester back to ask again
	if outcome == "approved" {
		state = ApprovalApproved
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var id, by string
		err := tx.QueryRowContext(ctx, `SELECT id, approval_requested_by FROM jobs WHERE approval_ref = ? AND approval = 'pending'`, approvalID).Scan(&id, &by)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if state == ApprovalApproved && slices.Contains(approvers, by) {
			state = ApprovalRejected
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET approval=?, updated_at=?, version=version+1 WHERE id=? AND approval='pending'`, state, ts(s.now()), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, "can-core", "job.approval_"+state, "job:"+id, nil, "ok")
	})
}

// CanEventHandler handles the events can-core delivers to this module (manifest `subscribes`).
func (s *Store) CanEventHandler() func(context.Context, modclient.Event) error {
	return func(ctx context.Context, ev modclient.Event) error {
		if ev.Type != "approval.decided.v1" || ev.Source != "can-core" {
			return nil
		}
		var d struct {
			RequestID string   `json:"request_id"`
			Type      string   `json:"type"`
			Outcome   string   `json:"outcome"`
			Approvers []string `json:"approvers"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil || d.RequestID == "" {
			return nil // malformed: nothing to retry
		}
		if d.Type != "requisition" {
			return nil
		}
		return s.ApplyApprovalDecision(ctx, d.RequestID, d.Outcome, d.Approvers)
	}
}

package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

const requestCols = `id, module_id, type, requester, ref, title, amount, payload_json, status, outcome_reason, created_at, expires_at, decided_at`

func scanRequest(row interface{ Scan(...any) error }, withPayload bool) (Request, error) {
	var r Request
	var payload string
	var created, expires, decided int64
	if err := row.Scan(&r.ID, &r.ModuleID, &r.Type, &r.Requester, &r.Ref, &r.Title, &r.Amount, &payload, &r.Status, &r.OutcomeReason, &created, &expires, &decided); err != nil {
		return Request{}, err
	}
	r.CreatedAt = time.Unix(created, 0)
	if expires > 0 {
		t := time.Unix(expires, 0)
		r.ExpiresAt = &t
	}
	if decided > 0 {
		t := time.Unix(decided, 0)
		r.DecidedAt = &t
	}
	if withPayload {
		r.Payload = json.RawMessage(payload)
	}
	return r, nil
}

func (e *Engine) loadRequest(ctx context.Context, id string) (Request, error) {
	if !ValidID(id) {
		return Request{}, ErrNotFound
	}
	r, err := scanRequest(e.db.QueryRowContext(ctx, `SELECT `+requestCols+` FROM approval_requests WHERE id=?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

type loaded struct {
	Detail
	policyJSON string
}

func (e *Engine) load(ctx context.Context, id string) (loaded, error) {
	r, err := e.loadRequest(ctx, id)
	if err != nil {
		return loaded{}, err
	}
	l := loaded{Detail: Detail{Request: r, Steps: []Step{}, Decisions: []Decision{}}}
	if err := e.db.QueryRowContext(ctx, `SELECT policy_json FROM approval_requests WHERE id=?`, id).Scan(&l.policyJSON); err != nil {
		return loaded{}, err
	}
	rows, err := e.db.QueryContext(ctx, `SELECT idx, stage, name, quorum, status, approvers FROM approval_steps WHERE request_id=? ORDER BY idx LIMIT 50`, id)
	if err != nil {
		return loaded{}, err
	}
	for rows.Next() {
		var s Step
		var aj string
		if err := rows.Scan(&s.Idx, &s.Stage, &s.Name, &s.Quorum, &s.Status, &aj); err != nil {
			_ = rows.Close()
			return loaded{}, err
		}
		var set approverSet
		_ = json.Unmarshal([]byte(aj), &set)
		s.Roles, s.Users = set.Roles, set.Users
		l.Steps = append(l.Steps, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return loaded{}, err
	}
	drows, err := e.db.QueryContext(ctx, `SELECT id, step_idx, actor, on_behalf_of, kind, comment, at FROM approval_decisions WHERE request_id=? ORDER BY id LIMIT ?`, id, MaxDecisions+50)
	if err != nil {
		return loaded{}, err
	}
	defer func() { _ = drows.Close() }()
	for drows.Next() {
		var d Decision
		var at int64
		if err := drows.Scan(&d.ID, &d.StepIdx, &d.Actor, &d.OnBehalfOf, &d.Kind, &d.Comment, &at); err != nil {
			return loaded{}, err
		}
		d.At = time.Unix(at, 0)
		l.Decisions = append(l.Decisions, d)
		if d.Kind == "approve" {
			for i := range l.Steps {
				if l.Steps[i].Idx == d.StepIdx {
					l.Steps[i].Approvals++
				}
			}
		}
	}
	return l, drows.Err()
}

// Get returns a request with its steps and history. It does not check who is asking: callers use
// CanView.
func (e *Engine) Get(ctx context.Context, id string) (Detail, error) {
	l, err := e.load(ctx, id)
	return l.Detail, err
}

// CanView reports whether viewer may see the request: the requester, an approver of any step
// (named, by role or by delegation), anyone who already took part, and approvals admins. Errors
// deny.
func (e *Engine) CanView(ctx context.Context, d Detail, viewer string) bool {
	if viewer == "" {
		return false
	}
	if viewer == d.Request.Requester {
		return true
	}
	for _, dec := range d.Decisions {
		if dec.Actor == viewer {
			return true
		}
	}
	if e.d.Perms != nil {
		if ok, err := e.d.Perms.Check(ctx, viewer, rbac.PermApprovalAdmin, ""); err == nil && ok {
			return true
		}
	}
	delegators, _ := e.activeDelegators(ctx, viewer)
	for _, s := range d.Steps {
		if s.Status == "skipped" {
			continue
		}
		set := approverSet{Roles: s.Roles, Users: s.Users}
		for _, who := range append([]string{viewer}, delegators...) {
			if ok, err := e.holds(ctx, who, set); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// CanAct reports whether actor could vote on the request right now (pending, not the requester, an
// active step names them directly or through a delegation, and they have not voted). Errors deny.
func (e *Engine) CanAct(ctx context.Context, d Detail, actor string) bool {
	if d.Request.Status != "pending" || actor == "" || actor == d.Request.Requester {
		return false
	}
	_, onBehalf, err := e.route(ctx, d.Request, d.Steps, actor)
	if err != nil {
		return false
	}
	for _, dec := range d.Decisions {
		if (dec.Kind == "approve" || dec.Kind == "reject") && (dec.Actor == actor || dec.OnBehalfOf == actor || (onBehalf != "" && (dec.Actor == onBehalf || dec.OnBehalfOf == onBehalf))) {
			return false
		}
	}
	return true
}

func (e *Engine) listRequests(ctx context.Context, q string, args ...any) ([]Request, error) {
	rows, err := e.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Request{}
	for rows.Next() {
		r, err := scanRequest(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func clampLimit(n int) int {
	if n < 1 || n > 200 {
		return 50
	}
	return n
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// Waiting lists the pending requests actor can act on now: an active step names them (directly, by
// role, or through a delegation) and they have not voted and are not the requester. At most limit
// rows, newest first.
func (e *Engine) Waiting(ctx context.Context, actor string, limit int) ([]Request, error) {
	delegators, err := e.activeDelegators(ctx, actor)
	if err != nil {
		return nil, err
	}
	subjects := append([]string{actor}, delegators...)
	roleSet := map[string]bool{}
	for _, s := range subjects {
		as, err := e.d.Roles.SubjectAssignments(ctx, s)
		if err != nil {
			return nil, err
		}
		for _, a := range as {
			roleSet[a.Role] = true
		}
	}
	args := []any{}
	for _, s := range subjects {
		args = append(args, s)
	}
	q := `SELECT ` + prefixCols("r.") + ` FROM approval_requests r WHERE r.status='pending'
		AND r.requester NOT IN (` + placeholders(len(subjects)) + `)
		AND EXISTS (SELECT 1 FROM approval_steps s WHERE s.request_id=r.id AND s.status='active' AND (
			EXISTS (SELECT 1 FROM json_each(s.approvers,'$.users') u WHERE u.value IN (` + placeholders(len(subjects)) + `))`
	args = append(args, argsOf(subjects)...)
	if len(roleSet) > 0 {
		roles := make([]string, 0, len(roleSet))
		for r := range roleSet {
			roles = append(roles, r)
		}
		q += ` OR EXISTS (SELECT 1 FROM json_each(s.approvers,'$.roles') x WHERE x.value IN (` + placeholders(len(roles)) + `))`
		args = append(args, argsOf(roles)...)
	}
	q += `))
		AND NOT EXISTS (SELECT 1 FROM approval_decisions d WHERE d.request_id=r.id AND d.kind IN ('approve','reject') AND (d.actor IN (` + placeholders(len(subjects)) + `) OR d.on_behalf_of IN (` + placeholders(len(subjects)) + `)))
		ORDER BY r.created_at DESC, r.id LIMIT ?`
	args = append(args, argsOf(subjects)...)
	args = append(args, argsOf(subjects)...)
	args = append(args, clampLimit(limit))
	return e.listRequests(ctx, q, args...)
}

func argsOf(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

func prefixCols(p string) string {
	cols := strings.Split(requestCols, ", ")
	for i, c := range cols {
		cols[i] = p + c
	}
	return strings.Join(cols, ", ")
}

// Requested lists the requests subject made, newest first.
func (e *Engine) Requested(ctx context.Context, subject string, limit int) ([]Request, error) {
	return e.listRequests(ctx, `SELECT `+requestCols+` FROM approval_requests WHERE requester=? ORDER BY created_at DESC, id LIMIT ?`, subject, clampLimit(limit))
}

// List lists requests of any requester, optionally of one status (pending, approved, rejected,
// cancelled, expired); it is for approvals admins.
func (e *Engine) List(ctx context.Context, status string, limit int) ([]Request, error) {
	if status == "" {
		return e.listRequests(ctx, `SELECT `+requestCols+` FROM approval_requests ORDER BY created_at DESC, id LIMIT ?`, clampLimit(limit))
	}
	switch status {
	case "pending", "approved", "rejected", "cancelled", "expired":
	default:
		return nil, fmt.Errorf("%w: status", ErrInvalid)
	}
	return e.listRequests(ctx, `SELECT `+requestCols+` FROM approval_requests WHERE status=? ORDER BY created_at DESC, id LIMIT ?`, status, clampLimit(limit))
}

// --- delegation -----------------------------------------------------------------------------

// Delegate lets delegate act for delegator between from and to (out of office). A person can
// delegate only for themselves; the delegate keeps their own separation-of-duties limits.
func (e *Engine) Delegate(ctx context.Context, delegator, delegate string, from, to time.Time) (int64, error) {
	if !rbac.ValidSubject(delegator) || !rbac.ValidSubject(delegate) || delegator == delegate {
		return 0, fmt.Errorf("%w: delegation needs two different subjects", ErrInvalid)
	}
	if !to.After(from) || to.Sub(from) > MaxDelegation || !to.After(e.now()) {
		return 0, fmt.Errorf("%w: a delegation must end in the future and last at most %d days", ErrInvalid, int(MaxDelegation/(24*time.Hour)))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var id int64
	err := e.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM approval_delegations WHERE delegator=? AND revoked=0 AND to_ts>?`, delegator, e.now().Unix()).Scan(&n); err != nil {
			return err
		}
		if n >= 10 {
			return fmt.Errorf("%w: at most 10 active delegations", ErrInvalid)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO approval_delegations(delegator, delegate, from_ts, to_ts) VALUES (?,?,?,?)`, delegator, delegate, from.Unix(), to.Unix())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		_, err = e.audit.AppendTx(ctx, tx, audit.Entry{Actor: delegator, Action: "approval.delegate", Target: delegate, Detail: map[string]any{"from": from.Unix(), "to": to.Unix()}})
		return err
	})
	return id, err
}

// RevokeDelegation ends one of actor's delegations.
func (e *Engine) RevokeDelegation(ctx context.Context, actor string, id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE approval_delegations SET revoked=1 WHERE id=? AND delegator=? AND revoked=0`, id, actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = e.audit.AppendTx(ctx, tx, audit.Entry{Actor: actor, Action: "approval.delegate.revoke", Target: fmt.Sprint(id)})
		return err
	})
}

// Delegations lists subject's current and future delegations, as delegator or delegate.
func (e *Engine) Delegations(ctx context.Context, subject string) ([]Delegation, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id, delegator, delegate, from_ts, to_ts FROM approval_delegations
		WHERE (delegator=? OR delegate=?) AND revoked=0 AND to_ts>? ORDER BY to_ts, id LIMIT 50`, subject, subject, e.now().Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Delegation{}
	for rows.Next() {
		var d Delegation
		var f, t int64
		if err := rows.Scan(&d.ID, &d.Delegator, &d.Delegate, &f, &t); err != nil {
			return nil, err
		}
		d.From, d.To = time.Unix(f, 0), time.Unix(t, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

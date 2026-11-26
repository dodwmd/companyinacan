package equity

import (
	"context"
	"database/sql"
	"errors"
)

// Exercise statuses: requested -> approved -> payment_recorded -> issued, or rejected / cancelled.
const (
	ExRequested = "requested"
	ExApproved  = "approved"
	ExPaid      = "payment_recorded"
	ExIssued    = "issued"
	ExRejected  = "rejected"
	ExCancelled = "cancelled"
)

// Exercise is a request to exercise vested options. Nothing is issued until the payment is
// recorded and a second person issues the shares into the ledger.
type Exercise struct {
	ID            string `json:"id"`
	EntityID      string `json:"entity_id"`
	GrantID       string `json:"grant_id"`
	StakeholderID string `json:"stakeholder_id"`
	Quantity      Qty    `json:"quantity"`
	Currency      string `json:"currency"`
	DueMinor      int64  `json:"due_minor"`
	Status        string `json:"status"`
	PaidMinor     *int64 `json:"paid_minor,omitempty"`
	PaidOn        string `json:"paid_on,omitempty"`
	IssuedOn      string `json:"issued_on,omitempty"`
	LedgerID      string `json:"ledger_id,omitempty"`
	ApprovalRef   string `json:"approval_ref,omitempty"`
	// PaymentReference is the sealed bank reference; present only for callers with field access.
	PaymentReference string `json:"payment_reference,omitempty"`
}

type exerciseRow struct {
	Exercise
	requestedBy, approvedBy, paymentBy string
	sealedRef                          sql.NullString
}

const exCols = `id, entity_id, grant_id, stakeholder_id, quantity, currency, due_minor, status, requested_by, approved_by, paid_minor, paid_on, payment_by, sens_payment_ref, issued_on, ledger_id, approval_ref`

func scanExercise(sc interface{ Scan(...any) error }) (*exerciseRow, error) {
	var e exerciseRow
	var q int64
	var appr, paidOn, payBy, issued, led sql.NullString
	var paid sql.NullInt64
	if err := sc.Scan(&e.ID, &e.EntityID, &e.GrantID, &e.StakeholderID, &q, &e.Currency, &e.DueMinor, &e.Status, &e.requestedBy, &appr, &paid, &paidOn, &payBy, &e.sealedRef, &issued, &led, &e.ApprovalRef); err != nil {
		return nil, err
	}
	e.Quantity = Qty(q)
	e.approvedBy, e.paymentBy, e.PaidOn, e.IssuedOn, e.LedgerID = appr.String, payBy.String, paidOn.String, issued.String, led.String
	if paid.Valid {
		v := paid.Int64
		e.PaidMinor = &v
	}
	return &e, nil
}

func (s *Service) loadExercise(ctx context.Context, q querier, id string) (*exerciseRow, error) {
	if err := validID("exr", id); err != nil {
		return nil, err
	}
	e, err := scanExercise(q.QueryRowContext(ctx, `SELECT `+exCols+` FROM exercises WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (s *Service) pendingExerciseQty(ctx context.Context, q querier, grantID string) (Qty, error) {
	var n sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT SUM(quantity) FROM exercises WHERE grant_id = ? AND status IN ('requested', 'approved', 'payment_recorded')`, grantID).Scan(&n)
	return Qty(n.Int64), err
}

func (s *Service) cancelPendingExercises(ctx context.Context, q querier, grantID, _ string) error {
	_, err := q.ExecContext(ctx, `UPDATE exercises SET status = 'cancelled', updated_at = ? WHERE grant_id = ? AND status IN ('requested', 'approved', 'payment_recorded')`, s.now().Unix(), grantID)
	return err
}

// RequestExercise asks to exercise quantity of a grant. The holder may request for themself;
// hr and finance may request on behalf. The quantity must fit what has vested, is not yet
// exercised or reserved by another request, within the exercise window. The amount due is
// computed once here: quantity x strike, rounded half away from zero to the minor unit.
func (s *Service) RequestExercise(ctx context.Context, a Actor, grantID string, qty Qty, idemKey string, body []byte) (string, bool, error) {
	g, err := s.loadGrant(ctx, s.db, grantID)
	if err != nil {
		return "", false, err
	}
	f, err := s.selfFact(ctx, s.db, a, g.StakeholderID)
	if err != nil {
		return "", false, err
	}
	if !Decide(a, ActExerciseRequest, f) || (!a.any() && (g.Status == GrantDraft || g.Status == GrantPending)) {
		s.auditDeny(ctx, a, string(ActExerciseRequest), "grant:"+grantID, nil)
		return "", false, ErrNotFound
	}
	return s.idempotent(ctx, a, "exercise.create:"+grantID, idemKey, body, func(ctx context.Context) (string, error) {
		if qty <= 0 || qty > MaxQty {
			return "", invalid("quantity must be a positive quantity")
		}
		id := newID("exr")
		err := s.txAudit(ctx, a, "exercise.request", "exercise:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
			g, err := s.loadGrant(ctx, tx, grantID)
			if err != nil {
				return err
			}
			if g.Status != GrantActive {
				return conflict("only an active grant can be exercised (status is %s)", g.Status)
			}
			if g.Type == "SAR" {
				return conflict("SAR settlement is not modelled: record the cash settlement outside this service")
			}
			st, err := s.stakeholder(ctx, tx, g.StakeholderID)
			if err != nil {
				return err
			}
			if st.Erased {
				return conflict("the stakeholder is erased")
			}
			v, err := s.vestingView(ctx, tx, &g.Grant, s.today())
			if err != nil {
				return err
			}
			if qty > v.Exercisable {
				return conflict("only %s can be exercised now (vested, not yet exercised or requested, inside the exercise window ending %s)", v.Exercisable, v.WindowEnd)
			}
			ref := ""
			if s.approvals != nil {
				if ref, err = s.approvals.Request(ctx, ApprovalRequest{Type: "exercise", Ref: id, Requester: canonSubject(a.Subject)}); err != nil {
					return err
				}
			}
			due, err := exerciseCost(qty, g.StrikeMinor)
			if err != nil {
				return err
			}
			setIdem(ctx, id)
			now := s.now().Unix()
			_, err = tx.ExecContext(ctx, `INSERT INTO exercises (id, entity_id, grant_id, stakeholder_id, quantity, currency, due_minor, status, requested_by, approval_ref, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, id, g.EntityID, g.ID, g.StakeholderID, int64(qty), g.Currency, due, ExRequested, canonSubject(a.Subject), ref, now, now)
			return err
		})
		if err != nil {
			return "", err
		}
		return id, nil
	})
}

// GetExercise returns one exercise request; the holder sees their own, hr, finance, board and
// admin see all. The payment reference is a sealed field for finance and admin only.
func (s *Service) GetExercise(ctx context.Context, a Actor, id string) (*Exercise, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	e, err := s.loadExercise(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	f, err := s.selfFact(ctx, s.db, a, e.StakeholderID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActExerciseRead, f) {
		s.auditDeny(ctx, a, "exercise.read", "exercise:"+id, nil)
		return nil, ErrNotFound
	}
	fields := []string{FieldGrantTerms}
	if e.sealedRef.Valid && FieldAccess(a, false, FieldPaymentRef, f) {
		fields = append(fields, FieldPaymentRef)
	}
	if err := s.auditRead(ctx, a, "exercise:"+id, fields); err != nil {
		return nil, err
	}
	if len(fields) > 1 {
		ref, err := s.keys.Open(e.sealedRef.String, id+":payment_ref")
		if err != nil {
			return nil, err
		}
		e.PaymentReference = ref
	}
	return &e.Exercise, nil
}

// ListExercises lists an entity's exercises (hr, finance, board, admin), optionally by status.
func (s *Service) ListExercises(ctx context.Context, a Actor, entityID, status string) ([]Exercise, error) {
	if err := s.authorize(ctx, a, ActExerciseRead, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if !a.any() {
		return nil, ErrForbidden
	}
	return s.listExercises(ctx, a, `entity_id = ?`, []any{entityID}, status)
}

// MyExercises lists the caller's own exercise requests.
func (s *Service) MyExercises(ctx context.Context, a Actor) ([]Exercise, error) {
	if err := s.authorize(ctx, a, ActSelfList, Fact{}, "self"); err != nil {
		return nil, err
	}
	return s.listExercises(ctx, a, `stakeholder_id IN (SELECT id FROM stakeholders WHERE person_id = ?)`, []any{a.PersonID}, "")
}

func (s *Service) listExercises(ctx context.Context, a Actor, where string, args []any, status string) ([]Exercise, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	q := `SELECT ` + exCols + ` FROM exercises WHERE ` + where
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	var es []*exerciseRow
	for rows.Next() {
		e, err := scanExercise(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		es = append(es, e)
	}
	_ = rows.Close()
	res := make([]string, len(es))
	for i, e := range es {
		res[i] = "exercise:" + e.ID
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldGrantTerms}); err != nil {
		return nil, err
	}
	out := make([]Exercise, len(es))
	for i, e := range es {
		out[i] = e.Exercise
	}
	return out, nil
}

// step runs one transition of the exercise chain in a transaction.
func (s *Service) exerciseStep(ctx context.Context, a Actor, id string, act Action, audit string, fields []string, fn func(ctx context.Context, tx *sqlTx, e *exerciseRow, g *grantRow) error) (*Exercise, error) {
	if err := s.authorize(ctx, a, act, Fact{}, "exercise:"+id); err != nil {
		return nil, err
	}
	err := s.txAudit(ctx, a, audit, "exercise:"+id, fields, func(ctx context.Context, tx *sqlTx) error {
		e, err := s.loadExercise(ctx, tx, id)
		if err != nil {
			return err
		}
		g, err := s.loadGrant(ctx, tx, e.GrantID)
		if err != nil {
			return err
		}
		return fn(ctx, tx, e, g)
	})
	if err != nil {
		if errors.Is(err, ErrSeparation) {
			s.auditDeny(ctx, a, string(act), "exercise:"+id, nil)
		}
		return nil, err
	}
	e, err := s.GetExercise(ctx, a, id)
	if err != nil && errors.Is(err, ErrNotFound) {
		// the actor may act on an exercise they may not read back (hr, board): return the bare record
		e2, lerr := s.loadExercise(ctx, s.db, id)
		if lerr != nil {
			return nil, lerr
		}
		return &e2.Exercise, nil
	}
	return e, err
}

// ApproveExercise approves a requested exercise: finance, board or admin, never the requester
// (nor the holder), through the same sodOK as everything else. With an approval engine
// configured, the engine must have approved.
func (s *Service) ApproveExercise(ctx context.Context, a Actor, id string) (*Exercise, error) {
	ex, err := s.exerciseStep(ctx, a, id, ActExerciseApprove, "exercise.approve", nil, func(ctx context.Context, tx *sqlTx, e *exerciseRow, _ *grantRow) error {
		if e.Status != ExRequested {
			return conflict("only a requested exercise can be approved (status is %s)", e.Status)
		}
		approver := canonSubject(a.Subject)
		decider := Actor{Subject: approver}
		if s.approvals != nil {
			st, err := s.approvals.Status(ctx, e.ApprovalRef)
			if err != nil {
				return err
			}
			if st.Outcome != "approved" {
				return conflict("the approval engine has not approved this exercise (outcome %q)", st.Outcome)
			}
			if decider, err = s.approver(ctx, st.DecidedBy); err != nil {
				return err
			}
			approver = decider.Subject
		}
		var person string
		if err := tx.QueryRowContext(ctx, `SELECT IFNULL(person_id, '') FROM stakeholders WHERE id = ?`, e.StakeholderID).Scan(&person); err != nil {
			return err
		}
		if !sodOK(decider, e.requestedBy, person) || !sodOK(a, e.requestedBy, person) {
			return ErrSeparation
		}
		if err := s.requireEstablished(ctx, tx, a, e.StakeholderID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE exercises SET status=?, approved_by=?, updated_at=? WHERE id=?`, ExApproved, approver, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ex, nil
}

// RejectExercise rejects a requested exercise.
func (s *Service) RejectExercise(ctx context.Context, a Actor, id string) (*Exercise, error) {
	ex, err := s.exerciseStep(ctx, a, id, ActExerciseApprove, "exercise.reject", nil, func(ctx context.Context, tx *sqlTx, e *exerciseRow, _ *grantRow) error {
		if e.Status != ExRequested {
			return conflict("only a requested exercise can be rejected (status is %s)", e.Status)
		}
		_, err := tx.ExecContext(ctx, `UPDATE exercises SET status=?, updated_at=? WHERE id=?`, ExRejected, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ex, nil
}

// CancelExercise withdraws an exercise before payment. The requester, hr, finance and admin may.
func (s *Service) CancelExercise(ctx context.Context, a Actor, id string) (*Exercise, error) {
	e0, err := s.loadExercise(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	f, err := s.selfFact(ctx, s.db, a, e0.StakeholderID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActExerciseRequest, f) {
		s.auditDeny(ctx, a, "exercise.cancel", "exercise:"+id, nil)
		return nil, ErrNotFound
	}
	err = s.txAudit(ctx, a, "exercise.cancel", "exercise:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		e, err := s.loadExercise(ctx, tx, id)
		if err != nil {
			return err
		}
		if e.Status != ExRequested && e.Status != ExApproved {
			return conflict("an exercise can be cancelled before its payment is recorded (status is %s)", e.Status)
		}
		_, err = tx.ExecContext(ctx, `UPDATE exercises SET status=?, updated_at=? WHERE id=?`, ExCancelled, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetExercise(ctx, a, id)
}

// PaymentInput records the payment received for an exercise.
type PaymentInput struct {
	AmountMinor int64  `json:"amount_minor"`
	PaidOn      string `json:"paid_on"`
	Reference   string `json:"reference"`
}

// RecordPayment records the exercise price received. The amount must equal the amount due
// (partial and net/cashless exercises are not modelled). finance or admin, not the requester.
func (s *Service) RecordPayment(ctx context.Context, a Actor, id string, in PaymentInput) (*Exercise, error) {
	d, err := parseDate(in.PaidOn)
	if err != nil {
		return nil, err
	}
	if d.After(s.now().UTC()) {
		return nil, invalid("paid_on cannot be in the future")
	}
	if err := validLine("reference", in.Reference, 200, false); err != nil {
		return nil, err
	}
	ex, err := s.exerciseStep(ctx, a, id, ActExercisePay, "exercise.payment", []string{FieldPaymentRef}, func(ctx context.Context, tx *sqlTx, e *exerciseRow, _ *grantRow) error {
		if e.Status != ExApproved {
			return conflict("payment is recorded on an approved exercise (status is %s)", e.Status)
		}
		if in.AmountMinor != e.DueMinor {
			return conflict("the amount due is %s %s; partial and net exercises are not modelled", FormatMinor(e.DueMinor, e.Currency), e.Currency)
		}
		var person string
		if err := tx.QueryRowContext(ctx, `SELECT IFNULL(person_id, '') FROM stakeholders WHERE id = ?`, e.StakeholderID).Scan(&person); err != nil {
			return err
		}
		if !sodOK(a, e.requestedBy, person) {
			return ErrSeparation
		}
		if err := s.requireEstablished(ctx, tx, a, e.StakeholderID); err != nil {
			return err
		}
		var ref any
		if in.Reference != "" {
			if ref, err = s.keys.Seal(in.Reference, id+":payment_ref"); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE exercises SET status=?, paid_minor=?, paid_on=?, payment_by=?, sens_payment_ref=?, updated_at=? WHERE id=?`,
			ExPaid, in.AmountMinor, in.PaidOn, canonSubject(a.Subject), ref, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ex, nil
}

// IssueShares completes an exercise: the shares go into the ledger and the grant's exercised
// count rises, in one transaction. The exercisable quantity and the class's authorised shares
// are checked again here. finance, admin or hr, never the requester and never the person who
// recorded the payment.
func (s *Service) IssueShares(ctx context.Context, a Actor, id string) (*Exercise, error) {
	ex, err := s.exerciseStep(ctx, a, id, ActExerciseIssue, "exercise.issue", nil, func(ctx context.Context, tx *sqlTx, e *exerciseRow, g *grantRow) error {
		if e.Status != ExPaid {
			return conflict("shares are issued after the payment is recorded (status is %s)", e.Status)
		}
		var person string
		if err := tx.QueryRowContext(ctx, `SELECT IFNULL(person_id, '') FROM stakeholders WHERE id = ?`, e.StakeholderID).Scan(&person); err != nil {
			return err
		}
		if !sodOK(a, e.requestedBy, person, e.paymentBy) {
			return ErrSeparation
		}
		if err := s.requireEstablished(ctx, tx, a, e.StakeholderID); err != nil {
			return err
		}
		if g.Status != GrantActive {
			return conflict("the grant is %s: it can no longer be exercised", g.Status)
		}
		st, err := s.stakeholder(ctx, tx, g.StakeholderID)
		if err != nil {
			return err
		}
		if st.Erased {
			return conflict("the stakeholder is erased")
		}
		today := s.today()
		if today > windowEnd(&g.Grant) {
			return conflict("the exercise window ended on %s", windowEnd(&g.Grant))
		}
		in, err := s.vestInputFor(ctx, tx, &g.Grant, today)
		if err != nil {
			return err
		}
		vested, err := Vested(in)
		if err != nil {
			return err
		}
		if vested-g.Exercised < e.Quantity {
			return conflict("only %s is vested and unexercised", vested-g.Exercised)
		}
		p, err := s.getPlan(ctx, tx, g.PlanID)
		if err != nil {
			return err
		}
		ledID, err := s.appendLedger(ctx, tx, ledgerRow{entityID: g.EntityID, classID: p.ClassID, stakeholderID: g.StakeholderID, kind: "exercise", date: today,
			qty: e.Quantity, priceMinor: &g.StrikeMinor, currency: g.Currency, grantID: g.ID, exerciseID: id, by: canonSubject(a.Subject)})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE grants SET exercised = exercised + ?, updated_at = ? WHERE id = ?`, int64(e.Quantity), s.now().Unix(), g.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE exercises SET status=?, issued_by=?, issued_on=?, ledger_id=?, updated_at=? WHERE id=?`, ExIssued, canonSubject(a.Subject), today, ledID, s.now().Unix(), id); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, EventExerciseRecorded, map[string]any{
			"exercise_id": id, "grant_id": g.ID, "stakeholder_id": g.StakeholderID, "entity_id": g.EntityID, "recorded_on": today})
	})
	if err != nil {
		return nil, err
	}
	return ex, nil
}

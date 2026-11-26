package equity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Grant statuses. draft -> pending_approval -> approved -> active -> cancelled | forfeited |
// expired. Terms are frozen when the grant leaves draft (and by a database trigger).
const (
	GrantDraft     = "draft"
	GrantPending   = "pending_approval"
	GrantApproved  = "approved"
	GrantActive    = "active"
	GrantCancelled = "cancelled"
	GrantForfeited = "forfeited"
	GrantExpired   = "expired"
)

// Grant is an option, RSU or SAR grant. Quantities are exact decimals (strings in JSON).
type Grant struct {
	ID              string   `json:"id"`
	EntityID        string   `json:"entity_id"`
	PlanID          string   `json:"plan_id"`
	StakeholderID   string   `json:"stakeholder_id"`
	Type            string   `json:"type"`
	Status          string   `json:"status"`
	Version         int      `json:"version"`
	SupersedesID    string   `json:"supersedes_id,omitempty"`
	Quantity        Qty      `json:"quantity"`
	Currency        string   `json:"currency"`
	StrikeMinor     int64    `json:"strike_minor"`
	GrantDate       string   `json:"grant_date"`
	VestStart       string   `json:"vesting_start"`
	Schedule        Schedule `json:"schedule"`
	ExpiryDate      string   `json:"expiry_date"`
	PTEPDays        int      `json:"post_termination_exercise_days"`
	ApprovedOn      string   `json:"approved_on,omitempty"`
	ActivatedOn     string   `json:"activated_on,omitempty"`
	TerminationDate string   `json:"termination_date,omitempty"`
	ForCause        bool     `json:"for_cause,omitempty"`
	TerminalOn      string   `json:"terminal_on,omitempty"`
	TerminalReason  string   `json:"terminal_reason,omitempty"`
	Exercised       Qty      `json:"exercised"`
	ApprovalRef     string   `json:"approval_ref,omitempty"`
	Notes           *string  `json:"notes,omitempty"`
}

// GrantInput is a draft grant. Notes are free-form (sealed at rest).
type GrantInput struct {
	StakeholderID string   `json:"stakeholder_id"`
	Type          string   `json:"type"`
	Quantity      Qty      `json:"quantity"`
	StrikeMinor   int64    `json:"strike_minor"`
	Currency      string   `json:"currency"`
	GrantDate     string   `json:"grant_date"`
	VestStart     string   `json:"vesting_start"`
	Schedule      Schedule `json:"schedule"`
	ExpiryDate    string   `json:"expiry_date"`
	PTEPDays      int      `json:"post_termination_exercise_days"`
	Notes         string   `json:"notes"`
}

// GrantPatch changes a draft. Absent fields are kept.
type GrantPatch struct {
	Type        *string   `json:"type"`
	Quantity    *Qty      `json:"quantity"`
	StrikeMinor *int64    `json:"strike_minor"`
	GrantDate   *string   `json:"grant_date"`
	VestStart   *string   `json:"vesting_start"`
	Schedule    *Schedule `json:"schedule"`
	ExpiryDate  *string   `json:"expiry_date"`
	PTEPDays    *int      `json:"post_termination_exercise_days"`
	Notes       *string   `json:"notes"`
}

// grantRow is a grant with the internal fields the service needs.
type grantRow struct {
	Grant
	createdBy, submittedBy, approvedBy string
	editedBy                           []string
	sealedNotes                        sql.NullString
	seq                                int64
}

const grantCols = `g.id, g.entity_id, g.plan_id, g.stakeholder_id, g.type, g.status, g.version, g.supersedes_id, g.quantity, g.currency, g.strike_minor,
	g.grant_date, g.vest_start, g.schedule, g.expiry_date, g.ptep_days, g.approved_on, g.activated_on, g.termination_date, g.for_cause, g.terminal_on,
	g.terminal_reason, g.exercised, g.approval_ref, g.created_by, g.edited_by, g.submitted_by, g.approved_by, g.sens_notes, g.rowid`

func scanGrant(sc interface{ Scan(...any) error }) (*grantRow, error) {
	var g grantRow
	var sup, appr, act, term, tOn, sub, apprBy sql.NullString
	var sched, edited string
	var qty, exer int64
	var cause int
	err := sc.Scan(&g.ID, &g.EntityID, &g.PlanID, &g.StakeholderID, &g.Type, &g.Status, &g.Version, &sup, &qty, &g.Currency, &g.StrikeMinor,
		&g.GrantDate, &g.VestStart, &sched, &g.ExpiryDate, &g.PTEPDays, &appr, &act, &term, &cause, &tOn,
		&g.TerminalReason, &exer, &g.ApprovalRef, &g.createdBy, &edited, &sub, &apprBy, &g.sealedNotes, &g.seq)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(sched), &g.Schedule); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(edited), &g.editedBy)
	g.Quantity, g.Exercised = Qty(qty), Qty(exer)
	g.SupersedesID, g.ApprovedOn, g.ActivatedOn, g.TerminationDate, g.TerminalOn = sup.String, appr.String, act.String, term.String, tOn.String
	g.ForCause = cause == 1
	g.submittedBy, g.approvedBy = sub.String, apprBy.String
	return &g, nil
}

func (s *Service) loadGrant(ctx context.Context, q querier, id string) (*grantRow, error) {
	if err := validID("grt", id); err != nil {
		return nil, err
	}
	g, err := scanGrant(q.QueryRowContext(ctx, `SELECT `+grantCols+` FROM grants g WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return g, err
}

// view returns the grant as the actor may see it: notes only with field access.
func (s *Service) view(a Actor, g *grantRow, f Fact) (*Grant, error) {
	out := g.Grant
	if !a.any() {
		out.TerminalReason = "" // free text written by hr or the board is not for the holder; the status says what happened
	}
	if g.sealedNotes.Valid && FieldAccess(a, false, FieldGrantNotes, f) {
		n, err := s.keys.Open(g.sealedNotes.String, g.ID+":notes")
		if err != nil {
			return nil, err
		}
		out.Notes = &n
	}
	return &out, nil
}

func validateGrantTerms(g *Grant, entityCurrency string) error {
	switch g.Type {
	case "ISO", "NSO", "RSU", "SAR":
	default:
		return invalid("type must be ISO, NSO, RSU or SAR")
	}
	if g.Quantity <= 0 || g.Quantity > MaxQty {
		return invalid("quantity must be a positive quantity")
	}
	if err := validMinor("strike_minor", g.StrikeMinor); err != nil {
		return err
	}
	if g.Type == "RSU" && g.StrikeMinor != 0 {
		return invalid("an RSU has no strike price")
	}
	if g.Type != "RSU" && g.StrikeMinor == 0 {
		return invalid("%s needs a strike price greater than zero", g.Type)
	}
	if err := validCurrency(g.Currency); err != nil {
		return err
	}
	if g.Currency != entityCurrency {
		return invalid("currency must be the entity's currency (%s)", entityCurrency)
	}
	gd, err := parseDate(g.GrantDate)
	if err != nil {
		return err
	}
	if _, err := parseDate(g.VestStart); err != nil {
		return err
	}
	ed, err := parseDate(g.ExpiryDate)
	if err != nil {
		return err
	}
	if !ed.After(gd) {
		return invalid("expiry_date must be after grant_date")
	}
	if _, err := exerciseCost(g.Quantity, g.StrikeMinor); err != nil {
		return err
	}
	if g.PTEPDays < 0 || g.PTEPDays > 3650 {
		return invalid("post_termination_exercise_days must be between 0 and 3650")
	}
	return g.Schedule.Validate()
}

func notesSeal(s *Service, id, notes string) (any, error) {
	if err := validText("notes", notes, 2000, false); err != nil {
		return nil, err
	}
	if notes == "" {
		return nil, nil
	}
	return s.keys.Seal(notes, id+":notes")
}

// CreateGrant records a draft grant. hr or admin. The grantor is recorded: they can never
// approve it.
func (s *Service) CreateGrant(ctx context.Context, a Actor, planID string, in GrantInput, idemKey string, body []byte) (string, bool, error) {
	if err := s.authorize(ctx, a, ActGrantDraft, Fact{}, "plan:"+planID); err != nil {
		return "", false, err
	}
	return s.idempotent(ctx, a, "grant.create:"+planID, idemKey, body, func(ctx context.Context) (string, error) {
		p, err := s.getPlan(ctx, s.db, planID)
		if err != nil {
			return "", err
		}
		ent, err := s.entity(ctx, s.db, p.EntityID)
		if err != nil {
			return "", err
		}
		st, err := s.stakeholder(ctx, s.db, in.StakeholderID)
		if err != nil || st.EntityID != p.EntityID {
			return "", invalid("stakeholder_id is not a stakeholder of this entity")
		}
		if st.Erased {
			return "", conflict("the stakeholder is erased")
		}
		if !sodOK(a, st.PersonID) {
			return "", ErrSeparation
		}
		if p.Status != "active" {
			return "", conflict("the plan is closed")
		}
		g := Grant{ID: newID("grt"), EntityID: p.EntityID, PlanID: p.ID, StakeholderID: st.ID, Type: in.Type, Status: GrantDraft, Version: 1,
			Quantity: in.Quantity, Currency: in.Currency, StrikeMinor: in.StrikeMinor, GrantDate: in.GrantDate, VestStart: in.VestStart,
			Schedule: in.Schedule, ExpiryDate: in.ExpiryDate, PTEPDays: in.PTEPDays}
		if err := validateGrantTerms(&g, ent.Currency); err != nil {
			return "", err
		}
		notes, err := notesSeal(s, g.ID, in.Notes)
		if err != nil {
			return "", err
		}
		sched, _ := json.Marshal(g.Schedule)
		err = s.txAudit(ctx, a, "grant.create", "grant:"+g.ID, []string{FieldGrantTerms}, func(ctx context.Context, tx *sqlTx) error {
			setIdem(ctx, g.ID)
			now := s.now().Unix()
			_, err := tx.ExecContext(ctx, `INSERT INTO grants (id, entity_id, plan_id, stakeholder_id, type, status, version, quantity, currency, strike_minor, grant_date, vest_start, schedule, expiry_date, ptep_days, sens_notes, created_by, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, g.ID, g.EntityID, g.PlanID, g.StakeholderID, g.Type, g.Status, g.Version, int64(g.Quantity), g.Currency, g.StrikeMinor,
				g.GrantDate, g.VestStart, string(sched), g.ExpiryDate, g.PTEPDays, notes, canonSubject(a.Subject), now, now)
			return err
		})
		if err != nil {
			return "", err
		}
		return g.ID, nil
	})
}

// UpdateGrant edits a draft. The editor is recorded and can never approve it.
func (s *Service) UpdateGrant(ctx context.Context, a Actor, id string, p GrantPatch) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantDraft, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	err := s.txAudit(ctx, a, "grant.update", "grant:"+id, []string{FieldGrantTerms}, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if g.Status != GrantDraft {
			return conflict("grant terms are frozen once a grant leaves draft; amend it (new version) or cancel and re-grant")
		}
		ent, err := s.entity(ctx, tx, g.EntityID)
		if err != nil {
			return err
		}
		n := g.Grant
		if p.Type != nil {
			n.Type = *p.Type
		}
		if p.Quantity != nil {
			n.Quantity = *p.Quantity
		}
		if p.StrikeMinor != nil {
			n.StrikeMinor = *p.StrikeMinor
		}
		if p.GrantDate != nil {
			n.GrantDate = *p.GrantDate
		}
		if p.VestStart != nil {
			n.VestStart = *p.VestStart
		}
		if p.Schedule != nil {
			n.Schedule = *p.Schedule
		}
		if p.ExpiryDate != nil {
			n.ExpiryDate = *p.ExpiryDate
		}
		if p.PTEPDays != nil {
			n.PTEPDays = *p.PTEPDays
		}
		if err := validateGrantTerms(&n, ent.Currency); err != nil {
			return err
		}
		notes := any(g.sealedNotes.String)
		if !g.sealedNotes.Valid {
			notes = nil
		}
		if p.Notes != nil {
			if notes, err = notesSeal(s, id, *p.Notes); err != nil {
				return err
			}
		}
		edited := g.editedBy
		if c := canonSubject(a.Subject); !slices.Contains(edited, c) {
			edited = append(edited, c)
		}
		eb, _ := json.Marshal(edited)
		sched, _ := json.Marshal(n.Schedule)
		_, err = tx.ExecContext(ctx, `UPDATE grants SET type=?, quantity=?, strike_minor=?, grant_date=?, vest_start=?, schedule=?, expiry_date=?, ptep_days=?, sens_notes=?, edited_by=?, updated_at=? WHERE id=?`,
			n.Type, int64(n.Quantity), n.StrikeMinor, n.GrantDate, n.VestStart, string(sched), n.ExpiryDate, n.PTEPDays, notes, string(eb), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetGrant(ctx, a, id)
}

// DeleteGrant removes a draft that never went for approval.
func (s *Service) DeleteGrant(ctx context.Context, a Actor, id string) error {
	if err := s.authorize(ctx, a, ActGrantDraft, Fact{}, "grant:"+id); err != nil {
		return err
	}
	err := s.txAudit(ctx, a, "grant.delete", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if g.Status != GrantDraft || g.SupersedesID != "" {
			return conflict("only a draft that does not amend another grant can be deleted; cancel it instead")
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM grants WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

// visibleTo applies record-level visibility: the holder sees their own grant only once it has
// been approved (a draft or pending grant is not yet an offer); everyone else needs a role.
func (s *Service) visibleTo(ctx context.Context, a Actor, g *grantRow) (Fact, bool, error) {
	f, err := s.selfFact(ctx, s.db, a, g.StakeholderID)
	if err != nil {
		return f, false, err
	}
	if !Decide(a, ActGrantRead, f) {
		return f, false, nil
	}
	if !a.any() && (g.Status == GrantDraft || g.Status == GrantPending) {
		return f, false, nil
	}
	return f, true, nil
}

// GetGrant returns one grant. The read is audited before the data is returned. A caller who
// may not see it gets not found (a manager reading a report's grant is such a caller).
func (s *Service) GetGrant(ctx context.Context, a Actor, id string) (*Grant, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	g, err := s.loadGrant(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	f, ok, err := s.visibleTo(ctx, a, g)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.auditDeny(ctx, a, "grant.read", "grant:"+id, []string{FieldGrantTerms})
		return nil, ErrNotFound
	}
	fields := []string{FieldGrantTerms}
	if FieldAccess(a, false, FieldGrantNotes, f) && g.sealedNotes.Valid {
		fields = append(fields, FieldGrantNotes)
	}
	if err := s.auditRead(ctx, a, "grant:"+id, fields); err != nil {
		return nil, err
	}
	return s.view(a, g, f)
}

// GrantFilter narrows a list.
type GrantFilter struct {
	Status        string
	StakeholderID string
	After         int64
	Limit         int
}

// GrantPage is a page of grants with the cursor of the next one.
type GrantPage struct {
	Items []Grant `json:"items"`
	Next  string  `json:"next_cursor,omitempty"`
}

// ListGrants lists an entity's grants (hr, finance, board, admin). Every returned grant is
// audited, atomically, before the list is returned.
func (s *Service) ListGrants(ctx context.Context, a Actor, entityID string, f GrantFilter) (*GrantPage, error) {
	if err := s.authorize(ctx, a, ActGrantList, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	return s.listGrants(ctx, a, `g.entity_id = ?`, []any{entityID}, f, false)
}

func (s *Service) listGrants(ctx context.Context, a Actor, where string, args []any, f GrantFilter, selfOnly bool) (*GrantPage, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 50
	}
	q := `SELECT ` + grantCols + ` FROM grants g WHERE ` + where + ` AND g.rowid > ?`
	args = append(args, f.After)
	if f.Status != "" {
		q += ` AND g.status = ?`
		args = append(args, f.Status)
	}
	if f.StakeholderID != "" {
		q += ` AND g.stakeholder_id = ?`
		args = append(args, f.StakeholderID)
	}
	if selfOnly {
		q += ` AND g.status NOT IN ('draft', 'pending_approval')`
	}
	q += ` ORDER BY g.rowid LIMIT ?`
	args = append(args, f.Limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var gs []*grantRow
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		gs = append(gs, g)
	}
	_ = rows.Close()
	page := &GrantPage{Items: []Grant{}}
	if len(gs) > f.Limit {
		gs = gs[:f.Limit]
		page.Next = itoa(gs[len(gs)-1].seq)
	}
	res := make([]string, len(gs))
	fields := []string{FieldGrantTerms}
	for i, g := range gs {
		res[i] = "grant:" + g.ID
		if g.sealedNotes.Valid && FieldAccess(a, false, FieldGrantNotes, Fact{Self: selfOnly}) && !slices.Contains(fields, FieldGrantNotes) {
			fields = append(fields, FieldGrantNotes)
		}
	}
	if err := s.auditReadMany(ctx, a, "read", res, fields); err != nil {
		return nil, err
	}
	for _, g := range gs {
		fact := Fact{Self: selfOnly}
		v, err := s.view(a, g, fact)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *v)
	}
	return page, nil
}

// MyGrants lists the caller's own grants (approved or later) across entities.
func (s *Service) MyGrants(ctx context.Context, a Actor, f GrantFilter) (*GrantPage, error) {
	if err := s.authorize(ctx, a, ActSelfList, Fact{}, "self"); err != nil {
		return nil, err
	}
	return s.listGrants(ctx, a, `g.stakeholder_id IN (SELECT id FROM stakeholders WHERE person_id = ? AND erased_at IS NULL)`, []any{a.PersonID}, f, true)
}

// ---- lifecycle -------------------------------------------------------------------------------

// ApprovalRequest is what the approval engine is asked to decide. It carries identifiers and
// machine values only (spec 015: no titles, names or amounts).
type ApprovalRequest struct {
	Type      string // grant | exercise (declared in module.yaml)
	Ref       string // grant or exercise id
	Requester string // the subject who submitted it
}

// ApprovalStatus is the engine's answer.
type ApprovalStatus struct {
	Outcome   string // pending | approved | rejected | cancelled | expired
	DecidedBy string // subject of the final approver
}

// ApprovalEngine is the seam to can-core's approval engine (specs/015-approvals-notifications):
// the board step runs there when it is configured. The engine's own separation of duties
// applies in addition to the module's. Without an engine, the board role approves directly in
// this service.
type ApprovalEngine interface {
	Request(ctx context.Context, req ApprovalRequest) (ref string, err error)
	Status(ctx context.Context, ref string) (ApprovalStatus, error)
}

func (s *Service) poolAvailable(ctx context.Context, q querier, planID, asof, excludeGrant string) (avail Qty, err error) {
	ps, err := s.poolStatus(ctx, q, planID, asof, excludeGrant)
	if err != nil {
		return 0, err
	}
	return ps.Available, nil
}

// SubmitGrant moves a draft to pending approval: the terms freeze here. hr or admin.
func (s *Service) SubmitGrant(ctx context.Context, a Actor, id string) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantDraft, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	var ref string
	err := s.txAudit(ctx, a, "grant.submit", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if g.Status != GrantDraft {
			return conflict("only a draft can be submitted (status is %s)", g.Status)
		}
		st, err := s.stakeholder(ctx, tx, g.StakeholderID)
		if err != nil {
			return err
		}
		if st.Erased {
			return conflict("the stakeholder is erased")
		}
		if _, err := s.holderLink(ctx, tx, a, g.StakeholderID); err != nil {
			return err
		}
		history := g.editedBy
		if c := canonSubject(a.Subject); !slices.Contains(history, c) {
			history = append(history, c) // every submitter stays in the chain, even after a rejection
		}
		hb, _ := json.Marshal(history)
		avail, err := s.poolAvailable(ctx, tx, g.PlanID, s.today(), g.SupersedesID)
		if err != nil {
			return err
		}
		if avail < g.Quantity {
			return conflict("the plan has %s shares available, the grant needs %s", avail, g.Quantity)
		}
		if s.approvals != nil {
			if ref, err = s.approvals.Request(ctx, ApprovalRequest{Type: "grant", Ref: id, Requester: canonSubject(a.Subject)}); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE grants SET status=?, submitted_by=?, approval_ref=?, terminal_reason='', edited_by=?, updated_at=? WHERE id=?`, GrantPending, canonSubject(a.Subject), ref, string(hb), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetGrant(ctx, a, id)
}

// RejectGrant sends a pending grant back to draft. The board step.
func (s *Service) RejectGrant(ctx context.Context, a Actor, id, reason string) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantApprove, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	if err := validText("reason", reason, 500, false); err != nil {
		return nil, err
	}
	err := s.txAudit(ctx, a, "grant.reject", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if g.Status != GrantPending {
			return conflict("only a pending grant can be rejected (status is %s)", g.Status)
		}
		_, err = tx.ExecContext(ctx, `UPDATE grants SET status=?, submitted_by=NULL, approval_ref='', terminal_reason=?, updated_at=? WHERE id=?`, GrantDraft, truncate(reason, 500), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetGrant(ctx, a, id)
}

// ApproveGrant is the board approval step. The approver must hold the board role, and must not
// have created, edited or submitted the grant, nor be the grantee (one function: sodOK). With an
// approval engine configured, the engine must have approved and its decider is the approver of
// record. The pool is re-checked in the same transaction, so two pending grants can never
// together exceed it.
func (s *Service) ApproveGrant(ctx context.Context, a Actor, id string) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantApprove, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	err := s.txAudit(ctx, a, "grant.approve", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if g.Status != GrantPending {
			return conflict("only a pending grant can be approved (status is %s)", g.Status)
		}
		approver := canonSubject(a.Subject)
		decider := Actor{Subject: approver}
		if s.approvals != nil {
			st, err := s.approvals.Status(ctx, g.ApprovalRef)
			if err != nil {
				return err
			}
			if st.Outcome != "approved" {
				return conflict("the approval engine has not approved this grant (outcome %q)", st.Outcome)
			}
			if decider, err = s.approver(ctx, st.DecidedBy); err != nil {
				return err
			}
			approver = decider.Subject
		}
		link, err := s.holderLink(ctx, tx, a, g.StakeholderID)
		if err != nil {
			return err
		}
		// The holder and whoever wrote the grant cannot approve it. Who stands behind the holder
		// record is not excluded: establishing it already took two actors, and excluding them
		// as well left a company of two with nobody able to approve. It is audited instead.
		chain := append([]string{g.createdBy, g.submittedBy, link.person}, g.editedBy...)
		if !sodOK(decider, chain...) || !sodOK(a, chain...) {
			return ErrSeparation
		}
		behind := []string{link.createdBy, link.linker, link.attester, link.verifier}
		if !sodOK(decider, behind...) || !sodOK(a, behind...) {
			if err := s.auditIn(ctx, tx, a, "grant.approve.by_record_establisher", "grant:"+id, nil); err != nil {
				return err
			}
		}
		if g.SupersedesID != "" {
			if err := s.cancelSuperseded(ctx, tx, g); err != nil {
				return err
			}
		}
		avail, err := s.poolAvailable(ctx, tx, g.PlanID, s.today(), "")
		if err != nil {
			return err
		}
		if avail < g.Quantity {
			return conflict("the plan has %s shares available, the grant needs %s", avail, g.Quantity)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE grants SET status=?, approved_by=?, approved_on=?, terminal_reason='', updated_at=? WHERE id=?`, GrantApproved, approver, s.today(), s.now().Unix(), id); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, EventGrantApproved, map[string]any{
			"grant_id": id, "stakeholder_id": g.StakeholderID, "entity_id": g.EntityID, "plan_id": g.PlanID, "approved_on": s.today()})
	})
	if err != nil {
		if errors.Is(err, ErrSeparation) {
			s.auditDeny(ctx, a, "grant.approve", "grant:"+id, nil)
		}
		return nil, err
	}
	return s.GetGrant(ctx, a, id)
}

func (s *Service) cancelSuperseded(ctx context.Context, tx *sqlTx, g *grantRow) error {
	old, err := s.loadGrant(ctx, tx, g.SupersedesID)
	if err != nil {
		return err
	}
	if old.Status != GrantApproved && old.Status != GrantActive {
		return conflict("the grant this one amends is %s", old.Status)
	}
	if old.Exercised > 0 {
		return conflict("the grant this one amends has exercises; cancel the unexercised part and grant again")
	}
	if err := s.cancelPendingExercises(ctx, tx, old.ID, "superseded"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE grants SET status=?, terminal_on=?, terminal_reason=?, updated_at=? WHERE id=?`, GrantCancelled, s.today(), "superseded by "+g.ID, s.now().Unix(), old.ID)
	return err
}

// AmendGrant starts a new version of an approved or active grant as a draft; the old grant
// stays in force until the new version is approved (then it is cancelled, atomically). hr or admin.
func (s *Service) AmendGrant(ctx context.Context, a Actor, id string) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantDraft, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	newID := newID("grt")
	err := s.txAudit(ctx, a, "grant.amend", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if g.Status != GrantApproved && g.Status != GrantActive {
			return conflict("only an approved or active grant can be amended (status is %s)", g.Status)
		}
		if g.Exercised > 0 {
			return conflict("a grant with exercises cannot be amended; cancel the unexercised part and grant again")
		}
		sched, _ := json.Marshal(g.Schedule)
		now := s.now().Unix()
		_, err = tx.ExecContext(ctx, `INSERT INTO grants (id, entity_id, plan_id, stakeholder_id, type, status, version, supersedes_id, quantity, currency, strike_minor, grant_date, vest_start, schedule, expiry_date, ptep_days, created_by, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, newID, g.EntityID, g.PlanID, g.StakeholderID, g.Type, GrantDraft, g.Version+1, g.ID, int64(g.Quantity), g.Currency, g.StrikeMinor,
			g.GrantDate, g.VestStart, string(sched), g.ExpiryDate, g.PTEPDays, canonSubject(a.Subject), now, now)
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("this grant already has an amendment in progress")
		}
		return nil, err
	}
	return s.GetGrant(ctx, a, newID)
}

// ActivateGrant marks an approved grant active (the agreement is accepted or signed).
// Exercises are possible only on active grants. hr or admin.
func (s *Service) ActivateGrant(ctx context.Context, a Actor, id, on string) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantAdminister, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	if on == "" {
		on = s.today()
	}
	d, err := parseDate(on)
	if err != nil {
		return nil, err
	}
	if d.After(s.now().UTC()) {
		return nil, invalid("activated_on cannot be in the future")
	}
	err = s.txAudit(ctx, a, "grant.activate", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if g.Status != GrantApproved {
			return conflict("only an approved grant can be activated (status is %s)", g.Status)
		}
		if on < g.ApprovedOn {
			return invalid("activated_on cannot be before the approval date %s", g.ApprovedOn)
		}
		_, err = tx.ExecContext(ctx, `UPDATE grants SET status=?, activated_on=?, updated_at=? WHERE id=?`, GrantActive, on, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetGrant(ctx, a, id)
}

// CancelGrant cancels a grant that is not yet terminal. Shares already issued by exercise stay
// issued; only what is unexercised is released. hr or admin.
func (s *Service) CancelGrant(ctx context.Context, a Actor, id, reason string) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantAdminister, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	if err := validText("reason", reason, 500, false); err != nil {
		return nil, err
	}
	err := s.txAudit(ctx, a, "grant.cancel", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		switch g.Status {
		case GrantDraft, GrantPending, GrantApproved, GrantActive:
		default:
			return conflict("the grant is already %s", g.Status)
		}
		if err := s.cancelPendingExercises(ctx, tx, id, "grant cancelled"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE grants SET status=?, terminal_on=?, terminal_reason=?, updated_at=? WHERE id=?`, GrantCancelled, s.today(), truncate(reason, 500), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetGrant(ctx, a, id)
}

// TerminateGrant records the holder leaving. Vesting stops on the termination date. For cause
// forfeits everything unexercised at once (a simplification: real plans differ, so this is
// data the operator sets); otherwise the vested part stays exercisable for the grant's
// post-termination exercise window, then the grant expires. hr or admin.
func (s *Service) TerminateGrant(ctx context.Context, a Actor, id, on string, forCause bool) (*Grant, error) {
	if err := s.authorize(ctx, a, ActGrantAdminister, Fact{}, "grant:"+id); err != nil {
		return nil, err
	}
	d, err := parseDate(on)
	if err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "grant.terminate", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if err := s.secondActor(a, g); err != nil {
			return err
		}
		if g.Status != GrantActive {
			return conflict("only an active grant can be terminated (status is %s)", g.Status)
		}
		if g.TerminationDate != "" {
			return conflict("termination is already recorded (%s)", g.TerminationDate)
		}
		if on < g.VestStart {
			return invalid("termination_date cannot be before the vesting start")
		}
		if d.Before(s.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -maxBackdateDays)) {
			return invalid("termination_date cannot be more than %d days in the past: a termination recorded later than that changes what vested and needs a corrected grant (cancel and re-grant)", maxBackdateDays)
		}
		if forCause {
			if d.After(s.now().UTC()) {
				return invalid("a termination for cause cannot be dated in the future")
			}
			if err := s.cancelPendingExercises(ctx, tx, id, "terminated for cause"); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE grants SET status=?, termination_date=?, for_cause=1, terminal_on=?, terminal_reason='terminated for cause', updated_at=? WHERE id=?`, GrantForfeited, on, on, s.now().Unix(), id)
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE grants SET termination_date=?, updated_at=? WHERE id=?`, on, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetGrant(ctx, a, id)
}

// windowEnd is the last day the vested part can be exercised: the expiry date, or the
// termination date plus the post-termination window when that is earlier.
func windowEnd(g *Grant) string {
	end := g.ExpiryDate
	if g.TerminationDate != "" {
		if t, err := time.Parse(dateLayout, g.TerminationDate); err == nil {
			if w := t.AddDate(0, 0, g.PTEPDays).Format(dateLayout); w < end {
				end = w
			}
		}
	}
	return end
}

// RecordMilestone records that a milestone tranche was achieved. Append-only. hr or admin.
func (s *Service) RecordMilestone(ctx context.Context, a Actor, id string, idx int, on string) error {
	if err := s.authorize(ctx, a, ActGrantAdminister, Fact{}, "grant:"+id); err != nil {
		return err
	}
	d, err := parseDate(on)
	if err != nil {
		return err
	}
	if d.After(s.now().UTC()) {
		return invalid("achieved_on cannot be in the future")
	}
	err = s.txAudit(ctx, a, "grant.milestone", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if err := s.secondActor(a, g); err != nil {
			return err
		}
		if g.Status != GrantApproved && g.Status != GrantActive {
			return conflict("milestones are recorded on approved or active grants (status is %s)", g.Status)
		}
		if err := factDate(on, g.ApprovedOn, s.now()); err != nil {
			return err
		}
		if g.Schedule.Kind != "tranches" || idx < 0 || idx >= len(g.Schedule.Tranches) || !g.Schedule.Tranches[idx].Milestone {
			return invalid("tranche %d is not a milestone tranche of this grant", idx)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO grant_milestones (grant_id, tranche_idx, achieved_on, recorded_by, created_at) VALUES (?,?,?,?,?)`, id, idx, on, canonSubject(a.Subject), s.now().Unix())
		if err != nil && isUnique(err) {
			return conflict("that milestone is already recorded")
		}
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

// RecordAcceleration records that an acceleration trigger happened (a change of control, a
// termination without cause...). The service does not decide whether an event qualifies; the
// person recording it does, and it is audited. Append-only. hr or admin.
func (s *Service) RecordAcceleration(ctx context.Context, a Actor, id, trigger, on string) error {
	if err := s.authorize(ctx, a, ActGrantAdminister, Fact{}, "grant:"+id); err != nil {
		return err
	}
	d, err := parseDate(on)
	if err != nil {
		return err
	}
	if d.After(s.now().UTC()) {
		return invalid("occurred_on cannot be in the future")
	}
	err = s.txAudit(ctx, a, "grant.acceleration", "grant:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		g, err := s.loadGrant(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.guardGrantee(ctx, tx, a, g); err != nil {
			return err
		}
		if err := s.secondActor(a, g); err != nil {
			return err
		}
		if g.Status != GrantApproved && g.Status != GrantActive && g.Status != GrantForfeited && g.Status != GrantExpired {
			return conflict("acceleration is recorded on approved or active grants (status is %s)", g.Status)
		}
		if err := factDate(on, g.ApprovedOn, s.now()); err != nil {
			return err
		}
		known := false
		for _, c := range g.Schedule.Acceleration {
			known = known || slices.Contains(c.Triggers, trigger)
		}
		if !known {
			return invalid("no acceleration clause of this grant uses the trigger %q", trigger)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO grant_accel_events (grant_id, trigger, occurred_on, recorded_by, created_at) VALUES (?,?,?,?,?)`, id, trigger, on, canonSubject(a.Subject), s.now().Unix())
		if err != nil && isUnique(err) {
			return conflict("that trigger is already recorded")
		}
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

// vestInputFor builds the calculator input for a grant from the recorded facts.
func (s *Service) vestInputFor(ctx context.Context, q querier, g *Grant, asof string) (VestInput, error) {
	in := VestInput{Total: g.Quantity, Start: g.VestStart, Sched: g.Schedule, AsOf: asof, Termination: g.TerminationDate}
	rows, err := q.QueryContext(ctx, `SELECT tranche_idx, achieved_on FROM grant_milestones WHERE grant_id = ?`, g.ID)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var i int
		var d string
		if err := rows.Scan(&i, &d); err != nil {
			_ = rows.Close()
			return in, err
		}
		if in.Achieved == nil {
			in.Achieved = map[int]string{}
		}
		in.Achieved[i] = d
	}
	_ = rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT trigger, occurred_on FROM grant_accel_events WHERE grant_id = ?`, g.ID)
	if err != nil {
		return in, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e AccelEvent
		if err := rows.Scan(&e.Trigger, &e.Date); err != nil {
			return in, err
		}
		in.Events = append(in.Events, e)
	}
	return in, rows.Err()
}

// VestingView is the vesting position of a grant as of a date.
type VestingView struct {
	GrantID      string      `json:"grant_id"`
	AsOf         string      `json:"as_of"`
	Status       string      `json:"status"`
	Type         string      `json:"type"`
	Quantity     Qty         `json:"quantity"`
	Vested       Qty         `json:"vested"`
	Unvested     Qty         `json:"unvested"`
	Exercised    Qty         `json:"exercised"`
	Exercisable  Qty         `json:"exercisable"`
	StrikeMinor  int64       `json:"strike_minor"`
	Currency     string      `json:"currency"`
	WindowEnd    string      `json:"exercise_window_end"`
	Timeline     []VestPoint `json:"timeline"`
	Illustrative bool        `json:"illustrative"`
	Notice       string      `json:"notice"`
}

// Notice is the standing disclaimer on computed views.
const Notice = "Computed from the recorded data by a documented simple formula. This is a tool, not legal, tax or securities advice; jurisdiction-specific rules (409A, ISO/NSO limits, tax treatment, 83(b), plan-specific terms) are not implemented."

// Vesting returns the vesting position of one grant as of a date (today by default). The read is
// audited before it is returned; the holder may read their own, managers cannot read reports'.
func (s *Service) Vesting(ctx context.Context, a Actor, id, asof string) (*VestingView, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if asof == "" {
		asof = s.today()
	}
	if _, err := parseDate(asof); err != nil {
		return nil, err
	}
	g, err := s.loadGrant(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	_, ok, err := s.visibleTo(ctx, a, g)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.auditDeny(ctx, a, "vesting.read", "grant:"+id, []string{FieldGrantTerms})
		return nil, ErrNotFound
	}
	if err := s.auditRead(ctx, a, "grant:"+id, []string{FieldGrantTerms}); err != nil {
		return nil, err
	}
	return s.vestingView(ctx, s.db, &g.Grant, asof)
}

func (s *Service) vestingView(ctx context.Context, q querier, g *Grant, asof string) (*VestingView, error) {
	in, err := s.vestInputFor(ctx, q, g, asof)
	if err != nil {
		return nil, err
	}
	v := &VestingView{GrantID: g.ID, AsOf: asof, Status: g.Status, Type: g.Type, Quantity: g.Quantity, Exercised: g.Exercised,
		StrikeMinor: g.StrikeMinor, Currency: g.Currency, WindowEnd: windowEnd(g), Illustrative: true, Notice: Notice}
	if v.Vested, err = Vested(in); err != nil {
		return nil, err
	}
	v.Unvested = g.Quantity - v.Vested
	switch g.Status {
	case GrantCancelled, GrantForfeited, GrantExpired:
		v.Unvested = 0
		if g.Status != GrantExpired {
			v.Vested = min(v.Vested, g.Exercised)
		}
	}
	reserved, err := s.pendingExerciseQty(ctx, q, g.ID)
	if err != nil {
		return nil, err
	}
	v.Exercisable = exercisable(g, v.Vested, reserved, asof)
	if v.Timeline, err = Timeline(in); err != nil {
		return nil, err
	}
	if v.Timeline == nil {
		v.Timeline = []VestPoint{}
	}
	return v, nil
}

// exercisable is the vested, unexercised, unreserved quantity that can be exercised on asof.
func exercisable(g *Grant, vested, reserved Qty, asof string) Qty {
	if g.Status != GrantActive || g.Type == "SAR" || asof > windowEnd(g) {
		return 0
	}
	n := vested - g.Exercised - reserved
	if n < 0 {
		return 0
	}
	return n
}

// maxBackdateDays is how far back a milestone or acceleration trigger may be dated: facts that
// change what has vested are recorded promptly, by someone other than the grantor.
const maxBackdateDays = 90

// factDate checks the date of a recorded vesting fact: not before the grant was approved, not in
// the future, not further back than maxBackdateDays.
func factDate(on, approvedOn string, now time.Time) error {
	d, err := parseDate(on)
	if err != nil {
		return err
	}
	today := now.UTC().Truncate(24 * time.Hour)
	if d.After(today) {
		return invalid("the date cannot be in the future")
	}
	if approvedOn != "" && on < approvedOn {
		return invalid("the date cannot be before the grant was approved (%s)", approvedOn)
	}
	if d.Before(today.AddDate(0, 0, -maxBackdateDays)) {
		return invalid("the date cannot be more than %d days in the past", maxBackdateDays)
	}
	return nil
}

// guardGrantee refuses any change to a grant by its own grantee: whatever roles they hold,
// nobody administers their own equity. The check is the same sodOK as everywhere else, against
// the stakeholder's directory person id.
func (s *Service) guardGrantee(ctx context.Context, q querier, a Actor, g *grantRow) error {
	var person string
	if err := q.QueryRowContext(ctx, `SELECT IFNULL(person_id, '') FROM stakeholders WHERE id = ?`, g.StakeholderID).Scan(&person); err != nil {
		return err
	}
	if !sodOK(a, person) {
		return ErrSeparation
	}
	return nil
}

// secondActor requires someone other than the grantor chain (creator, editors, submitter,
// approver) to record a fact that changes what has vested.
func (s *Service) secondActor(a Actor, g *grantRow) error {
	if !sodOK(a, append([]string{g.createdBy, g.submittedBy, g.approvedBy}, g.editedBy...)...) {
		return ErrSeparation
	}
	return nil
}

type holderLinkInfo struct{ person, attester, verifier, linker, createdBy string }

// holderLink requires the stakeholder to be established (requireEstablished) and returns the
// holder and who stands behind the record.
func (s *Service) holderLink(ctx context.Context, q querier, a Actor, stakeholderID string) (holderLinkInfo, error) {
	var l holderLinkInfo
	if err := q.QueryRowContext(ctx, `SELECT IFNULL(person_id, ''), IFNULL(external_attested_by, ''), IFNULL(person_verified_by, ''), IFNULL(person_linked_by, ''), created_by FROM stakeholders WHERE id = ?`, stakeholderID).
		Scan(&l.person, &l.attester, &l.verifier, &l.linker, &l.createdBy); err != nil {
		return l, err
	}
	return l, s.requireEstablished(ctx, q, a, stakeholderID)
}

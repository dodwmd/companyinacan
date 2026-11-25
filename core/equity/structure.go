package equity

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"strings"
)

// ---- legal entities -------------------------------------------------------------------------

// Entity is a legal entity whose equity is tracked.
type Entity struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Jurisdiction string `json:"jurisdiction,omitempty"`
	Currency     string `json:"currency"`
}

// EntityInput creates an entity.
type EntityInput struct {
	Name         string `json:"name"`
	Jurisdiction string `json:"jurisdiction"`
	Currency     string `json:"currency"`
}

// CreateEntity registers a legal entity. board or admin.
func (s *Service) CreateEntity(ctx context.Context, a Actor, in EntityInput) (*Entity, error) {
	if err := s.authorize(ctx, a, ActStructureManage, Fact{}, "entity"); err != nil {
		return nil, err
	}
	if err := validLine("name", in.Name, 200, true); err != nil {
		return nil, err
	}
	if err := validLine("jurisdiction", in.Jurisdiction, 100, false); err != nil {
		return nil, err
	}
	if err := validCurrency(in.Currency); err != nil {
		return nil, err
	}
	e := &Entity{ID: newID("ent"), Name: in.Name, Jurisdiction: in.Jurisdiction, Currency: in.Currency}
	err := s.txAudit(ctx, a, "entity.create", "entity:"+e.ID, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO entities (id, name, jurisdiction, currency, created_at) VALUES (?,?,?,?,?)`, e.ID, e.Name, e.Jurisdiction, e.Currency, s.now().Unix())
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ListEntities lists entities.
func (s *Service) ListEntities(ctx context.Context, a Actor) ([]Entity, error) {
	if err := s.authorize(ctx, a, ActStructureRead, Fact{}, "entity"); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, jurisdiction, currency FROM entities ORDER BY created_at, id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Entity{}
	for rows.Next() {
		var e Entity
		if err := rows.Scan(&e.ID, &e.Name, &e.Jurisdiction, &e.Currency); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEntity returns one entity.
func (s *Service) GetEntity(ctx context.Context, a Actor, id string) (*Entity, error) {
	if err := s.authorize(ctx, a, ActStructureRead, Fact{}, "entity:"+id); err != nil {
		return nil, err
	}
	return s.entity(ctx, s.db, id)
}

func (s *Service) entity(ctx context.Context, q querier, id string) (*Entity, error) {
	if err := validID("ent", id); err != nil {
		return nil, err
	}
	var e Entity
	err := q.QueryRowContext(ctx, `SELECT id, name, jurisdiction, currency FROM entities WHERE id = ?`, id).Scan(&e.ID, &e.Name, &e.Jurisdiction, &e.Currency)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

// ---- share classes --------------------------------------------------------------------------

// ShareClass is a class of shares: common or preferred, with a par value and an authorised count.
type ShareClass struct {
	ID         string `json:"id"`
	EntityID   string `json:"entity_id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	ParMinor   int64  `json:"par_minor"`
	Currency   string `json:"currency"`
	Authorised Qty    `json:"authorised"`
	Issued     Qty    `json:"issued"`
}

// ShareClassInput creates a class.
type ShareClassInput struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	ParMinor   int64  `json:"par_minor"`
	Currency   string `json:"currency"`
	Authorised Qty    `json:"authorised"`
}

// CreateShareClass adds a class to an entity. board or admin.
func (s *Service) CreateShareClass(ctx context.Context, a Actor, entityID string, in ShareClassInput) (*ShareClass, error) {
	if err := s.authorize(ctx, a, ActStructureManage, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	if in.Kind != "common" && in.Kind != "preferred" {
		return nil, invalid("kind must be common or preferred")
	}
	if err := validMinor("par_minor", in.ParMinor); err != nil {
		return nil, err
	}
	if err := validCurrency(in.Currency); err != nil {
		return nil, err
	}
	if in.Authorised <= 0 || in.Authorised > MaxQty {
		return nil, invalid("authorised must be a positive quantity")
	}
	c := &ShareClass{ID: newID("cls"), EntityID: entityID, Name: in.Name, Kind: in.Kind, ParMinor: in.ParMinor, Currency: in.Currency, Authorised: in.Authorised}
	err := s.txAudit(ctx, a, "class.create", "class:"+c.ID, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO share_classes (id, entity_id, name, kind, par_minor, currency, authorised, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			c.ID, entityID, c.Name, c.Kind, c.ParMinor, c.Currency, int64(c.Authorised), s.now().Unix())
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("a share class named %q already exists", in.Name)
		}
		return nil, err
	}
	return c, nil
}

// RaiseAuthorised increases a class's authorised shares (never lowers them). board or admin.
func (s *Service) RaiseAuthorised(ctx context.Context, a Actor, classID string, to Qty) (*ShareClass, error) {
	if err := s.authorize(ctx, a, ActStructureManage, Fact{}, "class:"+classID); err != nil {
		return nil, err
	}
	if err := validID("cls", classID); err != nil {
		return nil, err
	}
	if to <= 0 || to > MaxQty {
		return nil, invalid("authorised must be a positive quantity")
	}
	err := s.txAudit(ctx, a, "class.raise", "class:"+classID, nil, func(ctx context.Context, tx *sqlTx) error {
		var cur int64
		if err := tx.QueryRowContext(ctx, `SELECT authorised FROM share_classes WHERE id = ?`, classID).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if int64(to) < cur {
			return conflict("authorised shares can be raised, not lowered (current %s)", Qty(cur))
		}
		_, err := tx.ExecContext(ctx, `UPDATE share_classes SET authorised = ? WHERE id = ?`, int64(to), classID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.getClass(ctx, s.db, classID)
}

func (s *Service) getClass(ctx context.Context, q querier, id string) (*ShareClass, error) {
	if err := validID("cls", id); err != nil {
		return nil, err
	}
	var c ShareClass
	var auth int64
	err := q.QueryRowContext(ctx, `SELECT id, entity_id, name, kind, par_minor, currency, authorised FROM share_classes WHERE id = ?`, id).
		Scan(&c.ID, &c.EntityID, &c.Name, &c.Kind, &c.ParMinor, &c.Currency, &auth)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Authorised = Qty(auth)
	var iss sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT SUM(quantity) FROM ledger WHERE class_id = ?`, id).Scan(&iss); err != nil {
		return nil, err
	}
	c.Issued = Qty(iss.Int64)
	return &c, nil
}

// ListShareClasses lists an entity's classes with their issued counts.
func (s *Service) ListShareClasses(ctx context.Context, a Actor, entityID string) ([]ShareClass, error) {
	if err := s.authorize(ctx, a, ActStructureRead, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM share_classes WHERE entity_id = ? ORDER BY created_at, id LIMIT 200`, entityID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	out := []ShareClass{}
	for _, id := range ids {
		c, err := s.getClass(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, nil
}

// ---- plans ----------------------------------------------------------------------------------

// Plan is an equity plan or pool: shares reserved for grants.
type Plan struct {
	ID         string `json:"id"`
	EntityID   string `json:"entity_id"`
	ClassID    string `json:"class_id"`
	Name       string `json:"name"`
	Authorised Qty    `json:"authorised"`
	AdoptedOn  string `json:"adopted_on"`
	Status     string `json:"status"`
}

// PlanInput creates a plan.
type PlanInput struct {
	Name       string `json:"name"`
	ClassID    string `json:"class_id"`
	Authorised Qty    `json:"authorised"`
	AdoptedOn  string `json:"adopted_on"`
}

// CreatePlan adds a plan whose shares come from a class. board or admin. The pool may not
// exceed what the class has authorised across all its plans.
func (s *Service) CreatePlan(ctx context.Context, a Actor, entityID string, in PlanInput) (*Plan, error) {
	if err := s.authorize(ctx, a, ActStructureManage, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	if in.Authorised <= 0 || in.Authorised > MaxQty {
		return nil, invalid("authorised must be a positive quantity")
	}
	if _, err := parseDate(in.AdoptedOn); err != nil {
		return nil, err
	}
	p := &Plan{ID: newID("pln"), EntityID: entityID, ClassID: in.ClassID, Name: in.Name, Authorised: in.Authorised, AdoptedOn: in.AdoptedOn, Status: "active"}
	err := s.txAudit(ctx, a, "plan.create", "plan:"+p.ID, nil, func(ctx context.Context, tx *sqlTx) error {
		c, err := s.getClass(ctx, tx, in.ClassID)
		if err != nil || c.EntityID != entityID {
			return invalid("class_id is not a share class of this entity")
		}
		var reserved sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT SUM(authorised) FROM plans WHERE class_id = ? AND status = 'active'`, in.ClassID).Scan(&reserved); err != nil {
			return err
		}
		if reserved.Int64+int64(in.Authorised) > int64(c.Authorised) {
			return conflict("the plans of this class would reserve more than the %s shares the class authorises", c.Authorised)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO plans (id, entity_id, class_id, name, authorised, adopted_on, status, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			p.ID, entityID, p.ClassID, p.Name, int64(p.Authorised), p.AdoptedOn, p.Status, s.now().Unix())
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("a plan named %q already exists", in.Name)
		}
		return nil, err
	}
	return p, nil
}

func (s *Service) getPlan(ctx context.Context, q querier, id string) (*Plan, error) {
	if err := validID("pln", id); err != nil {
		return nil, err
	}
	var p Plan
	var auth int64
	err := q.QueryRowContext(ctx, `SELECT id, entity_id, class_id, name, authorised, adopted_on, status FROM plans WHERE id = ?`, id).
		Scan(&p.ID, &p.EntityID, &p.ClassID, &p.Name, &auth, &p.AdoptedOn, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	p.Authorised = Qty(auth)
	return &p, err
}

// ListPlans lists an entity's plans.
func (s *Service) ListPlans(ctx context.Context, a Actor, entityID string) ([]Plan, error) {
	if err := s.authorize(ctx, a, ActStructureRead, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM plans WHERE entity_id = ? ORDER BY created_at, id LIMIT 200`, entityID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	out := []Plan{}
	for _, id := range ids {
		p, err := s.getPlan(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

// ---- stakeholders ---------------------------------------------------------------------------

// Stakeholder is a holder of (or candidate for) equity. The record carries no personal data:
// the identity is a separate, sealed, per-field-audited read.
type Stakeholder struct {
	ID        string `json:"id"`
	EntityID  string `json:"entity_id"`
	Kind      string `json:"kind"`
	PersonID  string `json:"person_id,omitempty"`
	LegalHold bool   `json:"legal_hold"`
	Erased    bool   `json:"erased"`
	// ExternalAttested is true when a second person attested the holder as external (not a
	// director or approver); it stands in for a directory person link.
	ExternalAttested bool `json:"external_holder_attested"`
}

// Identity is the sealed part of a stakeholder.
type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	TaxID string `json:"tax_id,omitempty"`
}

// StakeholderInput creates a stakeholder.
type StakeholderInput struct {
	Kind     string `json:"kind"`
	PersonID string `json:"person_id"`
	Identity
}

func validIdentity(in Identity) error {
	if err := validLine("name", in.Name, 200, true); err != nil {
		return err
	}
	if in.Email != "" {
		ad, err := mail.ParseAddress(in.Email)
		if err != nil || ad.Address != in.Email || len(in.Email) > 254 {
			return invalid("email is not a valid address")
		}
	}
	return validLine("tax_id", in.TaxID, 64, false)
}

func (s *Service) sealIdentity(id string, in Identity) (name, email, tax any, err error) {
	seal := func(field, v string) (any, error) {
		if v == "" {
			return nil, nil
		}
		return s.keys.Seal(v, id+":"+field)
	}
	if name, err = seal("name", in.Name); err != nil {
		return
	}
	if email, err = seal("email", in.Email); err != nil {
		return
	}
	tax, err = seal("tax_id", in.TaxID)
	return
}

// CreateStakeholder registers a holder. hr, finance, board or admin.
func (s *Service) CreateStakeholder(ctx context.Context, a Actor, entityID string, in StakeholderInput) (*Stakeholder, error) {
	if err := s.authorize(ctx, a, ActStakeholderEdit, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if !FieldAccess(a, true, FieldIdentity, Fact{}) {
		s.auditDeny(ctx, a, "write", "entity:"+entityID, []string{FieldIdentity})
		return nil, ErrForbidden
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	switch in.Kind {
	case "founder", "employee", "investor", "advisor":
	default:
		return nil, invalid("kind must be founder, employee, investor or advisor")
	}
	if in.Kind == "employee" && in.PersonID == "" {
		return nil, invalid("an employee stakeholder needs the directory person_id")
	}
	if in.PersonID != "" && !personIDRe.MatchString(in.PersonID) {
		return nil, invalid("person_id must be a directory person id")
	}
	if err := validIdentity(in.Identity); err != nil {
		return nil, err
	}
	st := &Stakeholder{ID: newID("stk"), EntityID: entityID, Kind: in.Kind, PersonID: in.PersonID}
	n, e, t, err := s.sealIdentity(st.ID, in.Identity)
	if err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "stakeholder.create", "stakeholder:"+st.ID, []string{FieldIdentity}, func(ctx context.Context, tx *sqlTx) error {
		now := s.now().Unix()
		_, err := tx.ExecContext(ctx, `INSERT INTO stakeholders (id, entity_id, kind, person_id, sens_name, sens_email, sens_tax_id, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			st.ID, entityID, st.Kind, nullStr(st.PersonID), n, e, t, canonSubject(a.Subject), now, now)
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("that person is already a stakeholder of this entity")
		}
		return nil, err
	}
	return st, nil
}

// SetIdentity replaces a stakeholder's identity (all three fields). Audited per field.
func (s *Service) SetIdentity(ctx context.Context, a Actor, id string, in Identity) error {
	if err := validID("stk", id); err != nil {
		return err
	}
	if err := s.authorize(ctx, a, ActStakeholderEdit, Fact{}, "stakeholder:"+id); err != nil {
		return err
	}
	if !FieldAccess(a, true, FieldIdentity, Fact{}) {
		return ErrForbidden
	}
	if err := validIdentity(in); err != nil {
		return err
	}
	n, e, t, err := s.sealIdentity(id, in)
	if err != nil {
		return err
	}
	err = s.txAudit(ctx, a, "stakeholder.identity.write", "stakeholder:"+id, []string{FieldIdentity}, func(ctx context.Context, tx *sqlTx) error {
		var erased sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT erased_at FROM stakeholders WHERE id = ?`, id).Scan(&erased); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if erased.Valid {
			return conflict("the stakeholder is erased")
		}
		_, err := tx.ExecContext(ctx, `UPDATE stakeholders SET sens_name = ?, sens_email = ?, sens_tax_id = ?, updated_at = ? WHERE id = ?`, n, e, t, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) stakeholder(ctx context.Context, q querier, id string) (*Stakeholder, error) {
	if err := validID("stk", id); err != nil {
		return nil, err
	}
	var st Stakeholder
	var pid sql.NullString
	var hold int
	var erased sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT id, entity_id, kind, person_id, legal_hold, erased_at, external_attested_by IS NOT NULL FROM stakeholders WHERE id = ?`, id).
		Scan(&st.ID, &st.EntityID, &st.Kind, &pid, &hold, &erased, &st.ExternalAttested)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	st.PersonID, st.LegalHold, st.Erased = pid.String, hold == 1, erased.Valid
	return &st, nil
}

// GetStakeholder returns the non-identifying record. A caller who may not see it gets not
// found, so the response is no existence oracle for managers and colleagues.
func (s *Service) GetStakeholder(ctx context.Context, a Actor, id string) (*Stakeholder, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	st, err := s.stakeholder(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	f, err := s.selfFact(ctx, s.db, a, id)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActStakeholderRead, f) {
		s.auditDeny(ctx, a, "stakeholder.read", "stakeholder:"+id, nil)
		return nil, ErrNotFound
	}
	return st, nil
}

// ListStakeholders lists an entity's stakeholders (no identities).
func (s *Service) ListStakeholders(ctx context.Context, a Actor, entityID string) ([]Stakeholder, error) {
	if err := s.authorize(ctx, a, ActStakeholderList, Fact{}, "entity:"+entityID); err != nil {
		return nil, err
	}
	if _, err := s.entity(ctx, s.db, entityID); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, entity_id, kind, person_id, legal_hold, erased_at, external_attested_by IS NOT NULL FROM stakeholders WHERE entity_id = ? ORDER BY created_at, id LIMIT 5000`, entityID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Stakeholder{}
	for rows.Next() {
		var st Stakeholder
		var pid sql.NullString
		var hold int
		var erased sql.NullInt64
		if err := rows.Scan(&st.ID, &st.EntityID, &st.Kind, &pid, &hold, &erased, &st.ExternalAttested); err != nil {
			return nil, err
		}
		st.PersonID, st.LegalHold, st.Erased = pid.String, hold == 1, erased.Valid
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetIdentity returns the sealed identity. The read is audited before anything is returned:
// when the audit cannot be written, nothing is.
func (s *Service) GetIdentity(ctx context.Context, a Actor, id string) (*Identity, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if _, err := s.stakeholder(ctx, s.db, id); err != nil {
		return nil, err
	}
	f, err := s.selfFact(ctx, s.db, a, id)
	if err != nil {
		return nil, err
	}
	if !FieldAccess(a, false, FieldIdentity, f) {
		s.auditDeny(ctx, a, "read", "stakeholder:"+id, []string{FieldIdentity})
		return nil, ErrNotFound
	}
	if err := s.auditRead(ctx, a, "stakeholder:"+id, []string{FieldIdentity}); err != nil {
		return nil, err
	}
	return s.openIdentity(ctx, s.db, id)
}

func (s *Service) openIdentity(ctx context.Context, q querier, id string) (*Identity, error) {
	var n, e, t sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT sens_name, sens_email, sens_tax_id FROM stakeholders WHERE id = ?`, id).Scan(&n, &e, &t); err != nil {
		return nil, err
	}
	open := func(field string, v sql.NullString) (string, error) {
		if !v.Valid {
			return "", nil
		}
		return s.keys.Open(v.String, id+":"+field)
	}
	var out Identity
	var err error
	if out.Name, err = open("name", n); err != nil {
		return nil, err
	}
	if out.Email, err = open("email", e); err != nil {
		return nil, err
	}
	if out.TaxID, err = open("tax_id", t); err != nil {
		return nil, err
	}
	if out.Name == "" {
		out.Name = "[erased]"
	}
	return &out, nil
}

// AttestExternal records that a stakeholder with no directory link is an external holder: not a
// director, not someone who approves grants. It exists so that the board's separation of duties
// cannot be sidestepped by a holder the service cannot tie to a person. The attester must be
// someone other than whoever created the stakeholder, and is audited. hr, finance, board or admin.
func (s *Service) AttestExternal(ctx context.Context, a Actor, id string) (*Stakeholder, error) {
	if err := validID("stk", id); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActStakeholderEdit, Fact{}, "stakeholder:"+id); err != nil {
		return nil, err
	}
	err := s.txAudit(ctx, a, "stakeholder.attest_external", "stakeholder:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		var created string
		var pid, att sql.NullString
		var erased sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT created_by, person_id, external_attested_by, erased_at FROM stakeholders WHERE id = ?`, id).Scan(&created, &pid, &att, &erased); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if erased.Valid {
			return conflict("the stakeholder is erased")
		}
		if pid.Valid {
			return conflict("the stakeholder is linked to a directory person; no attestation is needed")
		}
		if att.Valid {
			return conflict("the stakeholder is already attested")
		}
		if !sodOK(a, created) {
			return ErrSeparation
		}
		_, err := tx.ExecContext(ctx, `UPDATE stakeholders SET external_attested_by = ?, external_attested_on = ?, updated_at = ? WHERE id = ?`, canonSubject(a.Subject), s.today(), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.stakeholder(ctx, s.db, id)
}

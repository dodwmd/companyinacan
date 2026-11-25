package board

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- legal entities ---------------------------------------------------------------------------

// LegalEntityInput creates a company record. The registration and tax numbers are sealed. No
// jurisdiction rules are implemented: the entity type and jurisdiction are labels.
type LegalEntityInput struct {
	Name            string `json:"name"`
	Jurisdiction    string `json:"jurisdiction"`
	EntityType      string `json:"entity_type"`
	IncorporatedOn  string `json:"incorporated_on"`
	FiscalYearEnd   string `json:"fiscal_year_end"` // MM-DD
	RegNumber       string `json:"registration_number"`
	TaxID           string `json:"tax_id"`
	EquityEntityRef string `json:"equity_entity_ref"`
}

// LegalEntity is a company record (identifiers are a separate, audited read).
type LegalEntity struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Jurisdiction    string `json:"jurisdiction,omitempty"`
	EntityType      string `json:"entity_type,omitempty"`
	Status          string `json:"status"`
	IncorporatedOn  string `json:"incorporated_on,omitempty"`
	FiscalYearEnd   string `json:"fiscal_year_end,omitempty"`
	EquityEntityRef string `json:"equity_entity_ref,omitempty"`
}

// Identifiers are the sealed registration details of an entity.
type Identifiers struct {
	EntityID  string `json:"entity_id"`
	RegNumber string `json:"registration_number,omitempty"`
	TaxID     string `json:"tax_id,omitempty"`
}

func validMMDD(s string) bool {
	if s == "" {
		return true
	}
	_, err := time.Parse("01-02", s)
	return err == nil && len(s) == 5
}

func (s *Service) seeEntity(ctx context.Context, a Actor, id string) (*LegalEntity, Fact, error) {
	e, err := s.loadEntity(ctx, s.db, id)
	if err != nil {
		return nil, Fact{}, err
	}
	f, err := s.entityMemberFact(ctx, s.db, a, id)
	if err != nil {
		return nil, f, err
	}
	if err := s.hide(ctx, a, ActEntityRead, f, "legal-entity:"+id); err != nil {
		return nil, f, err
	}
	return e, f, nil
}

func (s *Service) loadEntity(ctx context.Context, q querier, id string) (*LegalEntity, error) {
	if err := validID("lge", id); err != nil {
		return nil, err
	}
	var e LegalEntity
	err := q.QueryRowContext(ctx, `SELECT id, name, jurisdiction, entity_type, status, incorporated_on, fiscal_year_end, equity_entity_ref FROM legal_entities WHERE id = ?`, id).
		Scan(&e.ID, &e.Name, &e.Jurisdiction, &e.EntityType, &e.Status, &e.IncorporatedOn, &e.FiscalYearEnd, &e.EquityEntityRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

// CreateLegalEntity records a company. secretary or admin; the identifiers only the secretary.
func (s *Service) CreateLegalEntity(ctx context.Context, a Actor, in LegalEntityInput) (*LegalEntity, error) {
	if err := s.authorize(ctx, a, ActEntityManage, Fact{}, "legal-entities"); err != nil {
		return nil, err
	}
	if err := validLine("name", in.Name, 200, true); err != nil {
		return nil, err
	}
	for _, f := range []struct{ n, v string }{{"jurisdiction", in.Jurisdiction}, {"entity_type", in.EntityType}} {
		if err := validLine(f.n, f.v, 100, false); err != nil {
			return nil, err
		}
	}
	if in.IncorporatedOn != "" {
		if _, err := parseDate(in.IncorporatedOn); err != nil {
			return nil, err
		}
	}
	if !validMMDD(in.FiscalYearEnd) {
		return nil, invalid("fiscal_year_end must be MM-DD")
	}
	if err := validRef("equity_entity_ref", in.EquityEntityRef, false); err != nil {
		return nil, err
	}
	for _, f := range []struct{ n, v string }{{"registration_number", in.RegNumber}, {"tax_id", in.TaxID}} {
		if err := validLine(f.n, f.v, 64, false); err != nil {
			return nil, err
		}
	}
	if (in.RegNumber != "" || in.TaxID != "") && !FieldAccess(a, true, FieldRegNumber, Fact{}) {
		s.auditDeny(ctx, a, "write", "legal-entities", []string{FieldRegNumber})
		return nil, ErrForbidden
	}
	id := newID("lge")
	reg, err := s.sealNull(id, "reg_number", in.RegNumber)
	if err != nil {
		return nil, err
	}
	tax, err := s.sealNull(id, "tax_id", in.TaxID)
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	err = s.txAudit(ctx, a, "write", "legal-entity:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO legal_entities (id, name, jurisdiction, entity_type, status, incorporated_on, fiscal_year_end, sens_reg_number, sens_tax_id, equity_entity_ref, created_by, created_at, updated_at)
			VALUES (?,?,?,?,'active',?,?,?,?,?,?,?,?)`, id, in.Name, in.Jurisdiction, in.EntityType, in.IncorporatedOn, in.FiscalYearEnd, reg, tax, in.EquityEntityRef, canonSubject(a.Subject), now, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.loadEntity(ctx, s.db, id)
}

// GetLegalEntity returns an entity the caller can see.
func (s *Service) GetLegalEntity(ctx context.Context, a Actor, id string) (*LegalEntity, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	e, _, err := s.seeEntity(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "legal-entity:"+id, nil); err != nil {
		return nil, err
	}
	return e, nil
}

// ListLegalEntities lists the entities the caller can see.
func (s *Service) ListLegalEntities(ctx context.Context, a Actor) ([]*LegalEntity, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if !a.known() {
		s.auditDeny(ctx, a, "read", "legal-entities", nil)
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, jurisdiction, entity_type, status, incorporated_on, fiscal_year_end, equity_entity_ref FROM legal_entities ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var all []*LegalEntity
	for rows.Next() {
		var e LegalEntity
		if err := rows.Scan(&e.ID, &e.Name, &e.Jurisdiction, &e.EntityType, &e.Status, &e.IncorporatedOn, &e.FiscalYearEnd, &e.EquityEntityRef); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, &e)
	}
	_ = rows.Close()
	out := []*LegalEntity{}
	for _, e := range all { // filtered per entity: a director sees the entities their bodies are tied to
		f, err := s.entityMemberFact(ctx, s.db, a, e.ID)
		if err != nil {
			return nil, err
		}
		if Decide(a, ActEntityRead, f) {
			out = append(out, e)
			if len(out) >= 1000 {
				break
			}
		}
	}
	if err := s.auditRead(ctx, a, "legal-entities", nil); err != nil {
		return nil, err
	}
	return out, nil
}

// SetEntityStatus marks an entity active, dormant or dissolved. secretary or admin.
func (s *Service) SetEntityStatus(ctx context.Context, a Actor, id, status string) (*LegalEntity, error) {
	e, f, err := s.seeEntity(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActEntityManage, f, "legal-entity:"+id); err != nil {
		return nil, err
	}
	if status != "active" && status != "dormant" && status != "dissolved" {
		return nil, invalid("status must be active, dormant or dissolved")
	}
	if e.Status == "dissolved" && status != "dissolved" {
		return nil, conflict("a dissolved entity stays dissolved: record a new entity")
	}
	err = s.txAudit(ctx, a, "write", "legal-entity:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE legal_entities SET status = ?, updated_at = ? WHERE id = ?`, status, s.now().Unix(), id)
		return err
	})
	e.Status = status
	return e, err
}

// GetIdentifiers returns the sealed registration and tax numbers. The read is audited before it is returned.
func (s *Service) GetIdentifiers(ctx context.Context, a Actor, id string) (*Identifiers, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	_, f, err := s.seeEntity(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !FieldAccess(a, false, FieldRegNumber, f) {
		s.auditDeny(ctx, a, "read", "legal-entity:"+id, []string{FieldRegNumber})
		return nil, ErrForbidden
	}
	var reg, tax sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT sens_reg_number, sens_tax_id FROM legal_entities WHERE id = ?`, id).Scan(&reg, &tax); err != nil {
		return nil, err
	}
	out := &Identifiers{EntityID: id}
	if out.RegNumber, err = s.open(id, "reg_number", reg); err != nil {
		return nil, err
	}
	if out.TaxID, err = s.open(id, "tax_id", tax); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "legal-entity:"+id, []string{FieldRegNumber}); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- registered agent and addresses (effective-dated) -----------------------------------------

// ContactInput records the registered agent or an address from a date. A later entry of the same kind
// ends the earlier one the day before it starts; the history stays.
type ContactInput struct {
	Kind          string `json:"kind"` // registered_agent | registered_office | principal_office | postal
	Name          string `json:"name"`
	Address       string `json:"address"`
	PersonRef     string `json:"person_ref"`
	EffectiveFrom string `json:"effective_from"`
}

// Contact is one effective-dated contact record.
type Contact struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Name          string `json:"name,omitempty"`
	Address       string `json:"address,omitempty"`
	PersonRef     string `json:"person_ref,omitempty"`
	EffectiveFrom string `json:"effective_from"`
	EffectiveTo   string `json:"effective_to,omitempty"`
	Withheld      bool   `json:"withheld,omitempty"`
}

// AddContact records a contact record. secretary.
func (s *Service) AddContact(ctx context.Context, a Actor, entityID string, in ContactInput) (*Contact, error) {
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActRegisterWrite, f, "legal-entity:"+entityID); err != nil {
		return nil, err
	}
	switch in.Kind {
	case "registered_agent", "registered_office", "principal_office", "postal":
	default:
		return nil, invalid("kind must be registered_agent, registered_office, principal_office or postal")
	}
	if err := validLine("name", in.Name, 200, false); err != nil {
		return nil, err
	}
	if err := validText("address", in.Address, 1000, true); err != nil {
		return nil, err
	}
	if in.PersonRef != "" {
		if err := validPerson("person_ref", in.PersonRef); err != nil {
			return nil, err
		}
	}
	if _, err := parseDate(in.EffectiveFrom); err != nil {
		return nil, err
	}
	id := newID("ctc")
	name, err := s.sealNull(id, "name", in.Name)
	if err != nil {
		return nil, err
	}
	addr, err := s.seal(id, "address", in.Address)
	if err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "write", "legal-entity:"+entityID+"/contacts", []string{FieldRegisterDetail}, func(ctx context.Context, tx *sqlTx) error {
		var prevID, prevFrom string
		switch err := tx.QueryRowContext(ctx, `SELECT id, effective_from FROM entity_contacts WHERE entity_id = ? AND kind = ? AND effective_to IS NULL`, entityID, in.Kind).Scan(&prevID, &prevFrom); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if in.EffectiveFrom <= prevFrom {
				return conflict("effective_from must be after %s, when the current record starts", prevFrom)
			}
			t, _ := time.Parse(dateLayout, in.EffectiveFrom)
			if _, err := tx.ExecContext(ctx, `UPDATE entity_contacts SET effective_to = ? WHERE id = ? AND effective_to IS NULL`, t.AddDate(0, 0, -1).Format(dateLayout), prevID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO entity_contacts (id, entity_id, kind, sens_name, sens_address, person_ref, effective_from, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			id, entityID, in.Kind, name, addr, in.PersonRef, in.EffectiveFrom, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Contact{ID: id, Kind: in.Kind, Name: in.Name, Address: in.Address, PersonRef: in.PersonRef, EffectiveFrom: in.EffectiveFrom}, nil
}

// ListContacts returns the contact records in force on a date (default today), or all ("all"). The
// sealed name and address are returned only to those the field policy allows.
func (s *Service) ListContacts(ctx context.Context, a Actor, entityID, asOf string) ([]Contact, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	q, args := `SELECT id, kind, sens_name, sens_address, person_ref, effective_from, IFNULL(effective_to,'') FROM entity_contacts WHERE entity_id = ?`, []any{entityID}
	if asOf != "all" {
		if asOf == "" {
			asOf = s.today()
		}
		if _, err := parseDate(asOf); err != nil {
			return nil, err
		}
		q += ` AND effective_from <= ? AND (effective_to IS NULL OR effective_to >= ?)`
		args = append(args, asOf, asOf)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY kind, effective_from`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Contact{}
	var res []string
	for rows.Next() {
		var c Contact
		var nm, ad sql.NullString
		if err := rows.Scan(&c.ID, &c.Kind, &nm, &ad, &c.PersonRef, &c.EffectiveFrom, &c.EffectiveTo); err != nil {
			return nil, err
		}
		cf := f
		field := FieldRegisterDetail
		if c.PersonRef != "" {
			field = FieldPersonPrivate
			cf.Self = a.PersonID != "" && c.PersonRef == a.PersonID
		}
		if FieldAccess(a, false, field, cf) {
			if c.Name, err = s.open(c.ID, "name", nm); err != nil {
				return nil, err
			}
			if c.Address, err = s.open(c.ID, "address", ad); err != nil {
				return nil, err
			}
			res = append(res, "contact:"+c.ID)
		} else {
			c.Withheld = true
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldRegisterDetail}); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- statutory registers ----------------------------------------------------------------------------

// Registers.
const (
	RegMembers   = "members"
	RegDirectors = "directors"
	RegCharges   = "charges"
)

var registerKinds = map[string][]string{
	RegMembers:   {"allotted", "transferred", "ceased"},
	RegDirectors: {"appointed", "ceased"},
	RegCharges:   {"created", "satisfied"},
}

// EntryInput appends an entry to a statutory register. Detail holds personal and descriptive data (name,
// address, date_of_birth, email, description, terms): sealed.
type EntryInput struct {
	Kind          string            `json:"kind"`
	PartyRef      string            `json:"party_ref"`
	Office        string            `json:"office"`
	Title         string            `json:"title"`
	ClassRef      string            `json:"class_ref"`
	Quantity      int64             `json:"quantity"`
	EffectiveOn   string            `json:"effective_on"`
	Detail        map[string]string `json:"detail"`
	ResolutionRef string            `json:"resolution_ref"`
	CorrectsID    string            `json:"corrects_id"`
}

// Entry is one ledger line.
type Entry struct {
	ID             string            `json:"id"`
	Register       string            `json:"register"`
	Kind           string            `json:"kind"`
	PartyRef       string            `json:"party_ref"`
	Office         string            `json:"office,omitempty"`
	Title          string            `json:"title,omitempty"`
	ClassRef       string            `json:"class_ref,omitempty"`
	Quantity       int64             `json:"quantity,omitempty"`
	EffectiveOn    string            `json:"effective_on"`
	ResolutionRef  string            `json:"resolution_ref,omitempty"`
	CorrectsID     string            `json:"corrects_id,omitempty"`
	RecordedOn     string            `json:"recorded_on"`
	Attested       bool              `json:"attested"`
	AttestedOn     string            `json:"attested_on,omitempty"`
	Detail         map[string]string `json:"detail,omitempty"`
	DetailWithheld bool              `json:"detail_withheld,omitempty"`
}

var personalKeys = map[string]bool{"address": true, "date_of_birth": true, "email": true}

type regRow struct {
	Entry
	seq      int64
	recAt    int64
	by       string
	sealed   sql.NullString
	attestBy string
}

const regCols = `seq, id, register, kind, party_ref, office, title, class_ref, quantity, effective_on, resolution_ref, IFNULL(corrects_id,''), recorded_by, recorded_at, attested_by, IFNULL(attested_on,''), sens_detail`

func scanReg(r interface{ Scan(...any) error }) (*regRow, error) {
	var x regRow
	var attOn string
	err := r.Scan(&x.seq, &x.ID, &x.Register, &x.Kind, &x.PartyRef, &x.Office, &x.Title, &x.ClassRef, &x.Quantity, &x.EffectiveOn, &x.ResolutionRef, &x.CorrectsID, &x.by, &x.recAt, &x.attestBy, &attOn, &x.sealed)
	x.AttestedOn, x.Attested = attOn, attOn != ""
	x.RecordedOn = time.Unix(x.recAt, 0).UTC().Format(dateLayout)
	return &x, err
}

// effectiveEntries drops the entries that a later entry in the same set corrects, and orders by
// effective date then sequence.
func effectiveEntries(in []*regRow) []*regRow {
	corrected := map[string]bool{}
	for _, e := range in {
		if e.CorrectsID != "" {
			corrected[e.CorrectsID] = true
		}
	}
	var out []*regRow
	for _, e := range in {
		if !corrected[e.ID] {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].EffectiveOn != out[j].EffectiveOn {
			return out[i].EffectiveOn < out[j].EffectiveOn
		}
		return out[i].seq < out[j].seq
	})
	return out
}

// validateRegister replays a register and reports the first rule an entry breaks: nobody holds the
// same office twice, a holding never goes negative on any date, a member ceases only at zero, a charge
// is satisfied only when open. Exact integers.
func validateRegister(register string, entries []*regRow) error {
	open := map[string]bool{}
	bal := map[string]int64{}
	for _, e := range effectiveEntries(entries) {
		switch register {
		case RegDirectors:
			key := e.PartyRef + "|" + e.Office
			switch e.Kind {
			case "appointed":
				if open[key] {
					return conflict("the party already holds this office on the entry date")
				}
				open[key] = true
			case "ceased":
				if !open[key] {
					return conflict("the party does not hold this office on the entry date")
				}
				open[key] = false
			}
		case RegMembers:
			key := e.PartyRef + "|" + e.ClassRef
			switch e.Kind {
			case "allotted", "transferred":
				bal[key] += e.Quantity
				if bal[key] < 0 {
					return conflict("this would leave the holder with a negative holding on the entry date or a later date")
				}
			case "ceased":
				for k, v := range bal {
					if strings.HasPrefix(k, e.PartyRef+"|") && v != 0 {
						return conflict("the party still holds shares on the entry date: transfer them before the membership ceases")
					}
				}
			}
		case RegCharges:
			switch e.Kind {
			case "created":
				if open[e.PartyRef] {
					return conflict("the charge is already registered and not satisfied on the entry date")
				}
				open[e.PartyRef] = true
			case "satisfied":
				if !open[e.PartyRef] {
					return conflict("the charge is not registered on the entry date")
				}
				open[e.PartyRef] = false
			}
		}
	}
	return nil
}

func (s *Service) registerRows(ctx context.Context, q querier, entityID, register string, asOf, knownAt string) ([]*regRow, error) {
	query := `SELECT ` + regCols + ` FROM register_entries WHERE entity_id = ? AND register = ?`
	args := []any{entityID, register}
	if asOf != "" {
		query += ` AND effective_on <= ?`
		args = append(args, asOf)
	}
	if knownAt != "" {
		t, err := time.Parse(dateLayout, knownAt)
		if err != nil {
			return nil, invalid("known_at must be YYYY-MM-DD")
		}
		query += ` AND recorded_at < ?`
		args = append(args, t.AddDate(0, 0, 1).Unix())
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY seq LIMIT 100000`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*regRow
	for rows.Next() {
		r, err := scanReg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordEntry appends an entry to a statutory register. secretary. The register is replayed with the new
// entry and the entry is refused if it would break a rule; nothing is ever edited or deleted: a mistake
// is corrected by a later entry that names it. A director or officer attests the entry afterwards.
func (s *Service) RecordEntry(ctx context.Context, a Actor, entityID, register string, in EntryInput) (*Entry, error) {
	e, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActRegisterWrite, f, "legal-entity:"+entityID); err != nil {
		return nil, err
	}
	kinds, ok := registerKinds[register]
	if !ok {
		return nil, ErrNotFound
	}
	if !contains(kinds, in.Kind) {
		return nil, invalid("kind for this register must be one of %s", strings.Join(kinds, ", "))
	}
	if e.Status == "dissolved" {
		return nil, conflict("the entity is dissolved")
	}
	if err := validRef("party_ref", in.PartyRef, true); err != nil {
		return nil, err
	}
	if _, err := parseDate(in.EffectiveOn); err != nil {
		return nil, err
	}
	if in.EffectiveOn > s.today() {
		return nil, invalid("effective_on cannot be in the future: a register records what has happened")
	}
	for _, r := range []struct{ n, v string }{{"resolution_ref", in.ResolutionRef}, {"class_ref", in.ClassRef}} {
		if err := validRef(r.n, r.v, false); err != nil {
			return nil, err
		}
	}
	if err := validLine("title", in.Title, 100, false); err != nil {
		return nil, err
	}
	switch register {
	case RegDirectors:
		switch in.Office {
		case "director", "officer", "secretary", "auditor":
		default:
			return nil, invalid("office must be director, officer, secretary or auditor")
		}
		in.ClassRef, in.Quantity = "", 0
	case RegMembers:
		if in.Quantity < -1_000_000_000_000 || in.Quantity > 1_000_000_000_000 {
			return nil, invalid("quantity is out of range")
		}
		if (in.Kind == "allotted" && in.Quantity <= 0) || (in.Kind == "transferred" && in.Quantity == 0) || (in.Kind == "ceased" && in.Quantity != 0) {
			return nil, invalid("allotted needs a positive quantity, transferred a non-zero signed quantity, ceased none")
		}
		if in.Kind != "ceased" && in.ClassRef == "" {
			return nil, invalid("class_ref (the equity share class id) is required")
		}
		in.Office = ""
	case RegCharges:
		in.Office, in.ClassRef, in.Quantity = "", "", 0
	}
	if len(in.Detail) > 12 {
		return nil, invalid("at most 12 detail keys")
	}
	for k, v := range in.Detail {
		if !detailKeyOK(k) {
			return nil, invalid("detail keys are lower-case words (name, address, date_of_birth, email, description, terms)")
		}
		if err := validText("detail "+k, v, 1000, false); err != nil {
			return nil, err
		}
	}
	if in.Detail == nil {
		in.Detail = map[string]string{}
	}
	if in.CorrectsID != "" {
		if err := validID("reg", in.CorrectsID); err != nil {
			return nil, invalid("corrects_id is not an entry of this register")
		}
	}
	id := newID("reg")
	dj, _ := json.Marshal(in.Detail)
	sealed, err := s.seal(id, "detail", string(dj))
	if err != nil {
		return nil, err
	}
	var out *Entry
	err = s.txAudit(ctx, a, "write", "legal-entity:"+entityID+"/registers/"+register, []string{FieldRegisterDetail}, func(ctx context.Context, tx *sqlTx) error {
		existing, err := s.registerRows(ctx, tx, entityID, register, "", "")
		if err != nil {
			return err
		}
		if in.CorrectsID != "" {
			found := false
			for _, x := range existing {
				if x.ID == in.CorrectsID {
					found = true
				}
				if x.CorrectsID == in.CorrectsID {
					return conflict("that entry was already corrected")
				}
			}
			if !found {
				return invalid("corrects_id is not an entry of this register")
			}
		}
		cand := &regRow{Entry: Entry{ID: id, Register: register, Kind: in.Kind, PartyRef: in.PartyRef, Office: in.Office, ClassRef: in.ClassRef, Quantity: in.Quantity, EffectiveOn: in.EffectiveOn, CorrectsID: in.CorrectsID}, seq: 1 << 60}
		if err := validateRegister(register, append(existing, cand)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO register_entries (id, entity_id, register, kind, party_ref, office, title, class_ref, quantity, effective_on, sens_detail, resolution_ref, corrects_id, recorded_by, recorded_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, entityID, register, in.Kind, in.PartyRef, in.Office, in.Title, in.ClassRef, in.Quantity, in.EffectiveOn, sealed, in.ResolutionRef, nullStr(in.CorrectsID), canonSubject(a.Subject), s.now().Unix())
		out = &Entry{ID: id, Register: register, Kind: in.Kind, PartyRef: in.PartyRef, Office: in.Office, Title: in.Title, ClassRef: in.ClassRef, Quantity: in.Quantity, EffectiveOn: in.EffectiveOn,
			ResolutionRef: in.ResolutionRef, CorrectsID: in.CorrectsID, RecordedOn: s.today(), Detail: in.Detail}
		return err
	})
	return out, err
}

// registerName maps a validated register to its constant name, so a request value is never echoed.
func registerName(r string) string {
	switch r {
	case RegMembers:
		return RegMembers
	case RegDirectors:
		return RegDirectors
	case RegCharges:
		return RegCharges
	}
	return ""
}

func detailKeyOK(k string) bool {
	if k == "" || len(k) > 32 {
		return false
	}
	for _, r := range k {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// AttestEntry records that a second person has checked the entry. A director or officer; never the
// person it names, never the person who recorded it (one separation-of-duties function).
func (s *Service) AttestEntry(ctx context.Context, a Actor, entryID string) (*Entry, error) {
	if err := validID("reg", entryID); err != nil {
		return nil, err
	}
	row, err := scanReg(s.db.QueryRowContext(ctx, `SELECT `+regCols+` FROM register_entries WHERE id = ?`, entryID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var entityID string
	if err := s.db.QueryRowContext(ctx, `SELECT entity_id FROM register_entries WHERE id = ?`, entryID).Scan(&entityID); err != nil {
		return nil, err
	}
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActRegisterRead, f, "register-entry:"+entryID); err != nil {
		return nil, err
	}
	f.Subject = a.PersonID != "" && row.PartyRef == a.PersonID
	f.Drafter = !sodOK(a, row.by)
	if f.Subject || f.Drafter {
		if Decide(a, ActRegisterAttest, Fact{Member: f.Member}) { // would be allowed but for the relationship
			s.auditDeny(ctx, a, "register.attest", "register-entry:"+entryID, nil)
			return nil, ErrSeparation
		}
	}
	if err := s.authorize(ctx, a, ActRegisterAttest, f, "register-entry:"+entryID); err != nil {
		return nil, err
	}
	if row.Attested {
		return nil, conflict("the entry is already attested")
	}
	err = s.txAudit(ctx, a, "attest", "register-entry:"+entryID, nil, func(ctx context.Context, tx *sqlTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE register_entries SET attested_by = ?, attested_on = ? WHERE id = ? AND attested_on IS NULL`, canonSubject(a.Subject), s.today(), entryID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return conflict("the entry is already attested")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	row.Attested, row.AttestedOn = true, s.today()
	return &row.Entry, nil
}

// StateLine is the position of one party in a register on a date.
type StateLine struct {
	PartyRef string `json:"party_ref"`
	Office   string `json:"office,omitempty"`
	Title    string `json:"title,omitempty"`
	ClassRef string `json:"class_ref,omitempty"`
	Quantity int64  `json:"quantity,omitempty"`
	Since    string `json:"since"`
	EntryID  string `json:"entry_id"`
	Attested bool   `json:"attested"`
	Name     string `json:"name,omitempty"`
}

// RegisterView is a register as of a date: the state and, on request, the ledger behind it.
type RegisterView struct {
	EntityID string      `json:"entity_id"`
	Register string      `json:"register"`
	AsOf     string      `json:"as_of"`
	KnownAt  string      `json:"known_at,omitempty"`
	State    []StateLine `json:"state"`
	Entries  []Entry     `json:"entries,omitempty"`
}

// ViewRegister computes a register as of a date from the append-only ledger. as_of is the effective date
// (default today); known_at restricts to what had been recorded by then ("what did the register show on
// that day"). With history the ledger lines are returned too. Names and personal details come back only
// to those the field policy allows, and every entry opened is audited before the view is returned.
func (s *Service) ViewRegister(ctx context.Context, a Actor, entityID, register, asOf, knownAt string, history bool) (*RegisterView, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if _, ok := registerKinds[register]; !ok {
		return nil, ErrNotFound
	}
	if !Decide(a, ActRegisterRead, f) {
		s.auditDeny(ctx, a, "read", "legal-entity:"+entityID+"/registers/"+register, []string{FieldRegisterDetail})
		return nil, ErrNotFound // registers are confidential: invisible to those who cannot read them
	}
	if asOf == "" {
		asOf = s.today()
	}
	at, err := parseDate(asOf)
	if err != nil {
		return nil, err
	}
	asOf = at.Format(dateLayout) // re-formatted from the parsed time: only a date can come back
	rows, err := s.registerRows(ctx, s.db, entityID, register, asOf, knownAt)
	if err != nil {
		return nil, err
	}
	eff := effectiveEntries(rows)
	view := &RegisterView{EntityID: entityID, Register: registerName(register), AsOf: asOf, State: []StateLine{}}
	if knownAt != "" {
		kt, _ := time.Parse(dateLayout, knownAt) // registerRows already refused a bad known_at
		view.KnownAt = kt.Format(dateLayout)
	}
	byID := map[string]*regRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	type key = string
	state := map[key]*StateLine{}
	var order []key
	for _, e := range eff {
		switch register {
		case RegDirectors:
			k := e.PartyRef + "|" + e.Office
			if e.Kind == "appointed" {
				if state[k] == nil {
					order = append(order, k)
				}
				state[k] = &StateLine{PartyRef: e.PartyRef, Office: e.Office, Title: e.Title, Since: e.EffectiveOn, EntryID: e.ID, Attested: e.Attested}
			} else {
				delete(state, k)
			}
		case RegMembers:
			k := e.PartyRef + "|" + e.ClassRef
			if e.Kind == "ceased" {
				for kk := range state {
					if strings.HasPrefix(kk, e.PartyRef+"|") {
						delete(state, kk)
					}
				}
				continue
			}
			if state[k] == nil {
				order = append(order, k)
				state[k] = &StateLine{PartyRef: e.PartyRef, ClassRef: e.ClassRef, Since: e.EffectiveOn}
			}
			state[k].Quantity += e.Quantity
			state[k].EntryID, state[k].Attested = e.ID, e.Attested
		case RegCharges:
			if e.Kind == "created" {
				if state[e.PartyRef] == nil {
					order = append(order, e.PartyRef)
				}
				state[e.PartyRef] = &StateLine{PartyRef: e.PartyRef, Title: e.Title, Since: e.EffectiveOn, EntryID: e.ID, Attested: e.Attested}
			} else {
				delete(state, e.PartyRef)
			}
		}
	}
	for _, k := range order {
		if l := state[k]; l != nil && (register != RegMembers || l.Quantity > 0) {
			view.State = append(view.State, *l)
		}
	}
	var opened []string
	openEntry := func(r *regRow) (map[string]string, bool, error) {
		cf := f
		cf.Self = a.PersonID != "" && r.PartyRef == a.PersonID
		if !FieldAccess(a, false, FieldRegisterDetail, cf) {
			return nil, false, nil
		}
		plain, err := s.open(r.ID, "detail", r.sealed)
		if err != nil {
			return nil, false, err
		}
		m := map[string]string{}
		if plain != "" {
			if err := json.Unmarshal([]byte(plain), &m); err != nil {
				return nil, false, err
			}
		}
		if !FieldAccess(a, false, FieldPersonPrivate, cf) {
			for k := range personalKeys {
				delete(m, k)
			}
		}
		opened = append(opened, "register-entry:"+r.ID)
		return m, true, nil
	}
	for i := range view.State {
		if r := byID[view.State[i].EntryID]; r != nil {
			m, ok, err := openEntry(r)
			if err != nil {
				return nil, err
			}
			if ok {
				view.State[i].Name = m["name"]
			}
		}
	}
	if history {
		for _, r := range rows {
			e := r.Entry
			m, ok, err := openEntry(r)
			if err != nil {
				return nil, err
			}
			e.Detail, e.DetailWithheld = m, !ok
			view.Entries = append(view.Entries, e)
		}
	}
	if err := s.auditReadMany(ctx, a, "read", opened, []string{FieldRegisterDetail}); err != nil {
		return nil, err
	}
	return view, nil
}

// RegisterCSV builds the state of a register as of a date as CSV. Everything is read and audited before
// the writer is returned, so nothing is written for a read that was not recorded. Text cells are
// protected against spreadsheet formula injection.
func (s *Service) RegisterCSV(ctx context.Context, a Actor, entityID, register, asOf string) (func(io.Writer) error, error) {
	v, err := s.ViewRegister(ctx, a, entityID, register, asOf, "", false)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	c := csv.NewWriter(&buf)
	_ = c.Write([]string{"register", "as_of", "party_ref", "name", "office", "title", "class_ref", "quantity", "since", "attested"})
	for _, l := range v.State {
		_ = c.Write([]string{v.Register, v.AsOf, csvSafe(l.PartyRef), csvSafe(l.Name), csvSafe(l.Office), csvSafe(l.Title), csvSafe(l.ClassRef), strconv.FormatInt(l.Quantity, 10), l.Since, strconv.FormatBool(l.Attested)})
	}
	c.Flush()
	if err := c.Error(); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	return func(w io.Writer) error { _, err := w.Write(b); return err }, nil
}

// csvSafe defuses spreadsheet formula injection (OWASP "CSV injection"): a text cell that begins with
// = + - @ or a tab, carriage return or newline (after optional spaces) would be run as a formula by
// Excel, LibreOffice or Sheets, so it is prefixed with an apostrophe.
func csvSafe(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	t := strings.TrimLeft(s, " ")
	if t != "" && strings.ContainsRune("=+-@\t\r\n", rune(t[0])) {
		return "'" + s
	}
	return s
}

// ResolutionsCSV exports the resolutions register of a body: id, status, title, closed date and tally.
// The title is included only where the caller may read the text; the read is audited first.
func (s *Service) ResolutionsCSV(ctx context.Context, a Actor, bodyID string) (func(io.Writer) error, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	list, err := s.ListResolutions(ctx, a, bodyID)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	c := csv.NewWriter(&buf)
	_ = c.Write([]string{"id", "kind", "status", "title", "closed_on", "for", "against", "abstain", "reason"})
	var res []string
	for _, r := range list {
		r2, err := s.loadResolutionRow(ctx, s.db, r.ID)
		if err != nil {
			return nil, err
		}
		f, err := s.resFact(ctx, s.db, a, r2)
		if err != nil {
			return nil, err
		}
		title := ""
		if FieldAccess(a, false, FieldResolutionText, f) {
			if title, _, err = s.versionText(ctx, s.db, r.ID, r2.Version); err != nil {
				return nil, err
			}
			res = append(res, "resolution:"+r.ID)
		}
		row := []string{r.ID, r.Kind, r.Status, csvSafe(title), r.ClosedOn, "", "", "", ""}
		if r.Result != nil {
			row[5], row[6], row[7], row[8] = strconv.Itoa(r.Result.For), strconv.Itoa(r.Result.Against), strconv.Itoa(r.Result.Abstain), r.Result.Reason
		}
		_ = c.Write(row)
	}
	c.Flush()
	if err := c.Error(); err != nil {
		return nil, err
	}
	if err := s.auditReadMany(ctx, a, "export", res, []string{FieldResolutionText}); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	return func(w io.Writer) error { _, err := w.Write(b); return err }, nil
}

// ---- documents and share class links -----------------------------------------------------------

// DocumentInput adds a document reference (no bytes) to an entity.
type DocumentInput struct {
	Kind   string `json:"kind"`
	DocRef string `json:"doc_ref"`
	Title  string `json:"title"`
}

// Document is a document reference.
type Document struct {
	ID     string `json:"id"`
	Kind   string `json:"kind,omitempty"`
	DocRef string `json:"doc_ref"`
	Title  string `json:"title,omitempty"`
}

// AddDocument records a document reference. secretary.
func (s *Service) AddDocument(ctx context.Context, a Actor, entityID string, in DocumentInput) (*Document, error) {
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActRegisterWrite, f, "legal-entity:"+entityID); err != nil {
		return nil, err
	}
	if err := validRef("doc_ref", in.DocRef, true); err != nil {
		return nil, err
	}
	if err := validLine("title", in.Title, 200, true); err != nil {
		return nil, err
	}
	if err := validLine("kind", in.Kind, 64, false); err != nil {
		return nil, err
	}
	id := newID("doc")
	title, err := s.seal(id, "title", in.Title)
	if err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "write", "legal-entity:"+entityID+"/documents", nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO entity_documents (id, entity_id, kind, doc_ref, sens_title, added_by, added_at) VALUES (?,?,?,?,?,?,?)`,
			id, entityID, in.Kind, in.DocRef, title, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("this document is already recorded")
		}
		return nil, err
	}
	return &Document{ID: id, Kind: in.Kind, DocRef: in.DocRef, Title: in.Title}, nil
}

// ListDocuments lists document references; titles only where the register detail may be read.
func (s *Service) ListDocuments(ctx context.Context, a Actor, entityID string) ([]Document, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, doc_ref, sens_title FROM entity_documents WHERE entity_id = ? ORDER BY added_at, id`, entityID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Document{}
	var res []string
	for rows.Next() {
		var d Document
		var t sql.NullString
		if err := rows.Scan(&d.ID, &d.Kind, &d.DocRef, &t); err != nil {
			return nil, err
		}
		if FieldAccess(a, false, FieldRegisterDetail, f) {
			if d.Title, err = s.open(d.ID, "title", t); err != nil {
				return nil, err
			}
			res = append(res, "document:"+d.ID)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldRegisterDetail}); err != nil {
		return nil, err
	}
	return out, nil
}

// ShareClassLink ties an entity to a share class in `equity` by opaque id.
type ShareClassLink struct {
	ID       string `json:"id"`
	ClassRef string `json:"class_ref"`
}

// LinkShareClass records the link. secretary.
func (s *Service) LinkShareClass(ctx context.Context, a Actor, entityID, classRef string) (*ShareClassLink, error) {
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActRegisterWrite, f, "legal-entity:"+entityID); err != nil {
		return nil, err
	}
	if err := validRef("class_ref", classRef, true); err != nil {
		return nil, err
	}
	id := newID("scl")
	err = s.txAudit(ctx, a, "write", "legal-entity:"+entityID+"/share-classes", nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO share_class_links (id, entity_id, class_ref, created_by, created_at) VALUES (?,?,?,?,?)`, id, entityID, classRef, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("the share class is already linked")
		}
		return nil, err
	}
	return &ShareClassLink{ID: id, ClassRef: classRef}, nil
}

// ListShareClassLinks lists the linked share classes.
func (s *Service) ListShareClassLinks(ctx context.Context, a Actor, entityID string) ([]ShareClassLink, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if _, _, err := s.seeEntity(ctx, a, entityID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, class_ref FROM share_class_links WHERE entity_id = ? ORDER BY created_at, id`, entityID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ShareClassLink{}
	for rows.Next() {
		var l ShareClassLink
		if err := rows.Scan(&l.ID, &l.ClassRef); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "legal-entity:"+entityID+"/share-classes", nil); err != nil {
		return nil, err
	}
	return out, nil
}

package people

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Person is the directory.basic view of a record (data class directory-basic).
type Person struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Status        Status `json:"status"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	PreferredName string `json:"preferred_name,omitempty"`
	WorkEmail     string `json:"work_email,omitempty"`
	WorkPhone     string `json:"work_phone,omitempty"`
	Location      string `json:"location,omitempty"`
	DepartmentID  string `json:"department_id,omitempty"`
	TeamID        string `json:"team_id,omitempty"`
	PositionID    string `json:"position_id,omitempty"`
	ManagerID     string `json:"manager_id,omitempty"`
	StartDate     string `json:"start_date,omitempty"`
	EndDate       string `json:"end_date,omitempty"`
	SSOSubject    string `json:"sso_subject,omitempty"` // hr/admin only
	LegalHold     bool   `json:"legal_hold,omitempty"`  // hr/admin only
	Erased        bool   `json:"erased,omitempty"`      // hr/admin only
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	seq           int64
	legalHoldNote string
	endedAtUnix   int64
	erasedAtUnix  int64
}

// absentPersonID stands in for malformed ids so they take the same path as unknown ones.
const absentPersonID = "pers_000000000000000000000000"

const personCols = `seq, id, kind, status, COALESCE(sso_subject,''), given_name, family_name, preferred_name, work_email, work_phone, location,
 COALESCE(department_id,''), COALESCE(team_id,''), COALESCE(position_id,''), COALESCE(manager_id,''), start_date, end_date,
 legal_hold, legal_hold_reason, COALESCE(ended_at,0), COALESCE(erased_at,0), created_at, updated_at`

func scanPerson(sc interface{ Scan(...any) error }) (*Person, error) {
	var p Person
	var hold int
	var created, updated int64
	err := sc.Scan(&p.seq, &p.ID, &p.Kind, &p.Status, &p.SSOSubject, &p.GivenName, &p.FamilyName, &p.PreferredName, &p.WorkEmail, &p.WorkPhone, &p.Location,
		&p.DepartmentID, &p.TeamID, &p.PositionID, &p.ManagerID, &p.StartDate, &p.EndDate,
		&hold, &p.legalHoldNote, &p.endedAtUnix, &p.erasedAtUnix, &created, &updated)
	if err != nil {
		return nil, err
	}
	p.LegalHold = hold == 1
	p.Erased = p.erasedAtUnix != 0
	p.CreatedAt = time.Unix(created, 0).UTC().Format(time.RFC3339)
	p.UpdatedAt = time.Unix(updated, 0).UTC().Format(time.RFC3339)
	return &p, nil
}

// view strips the fields only hr and admin may see.
func (p *Person) view(a Actor) Person {
	v := *p
	if !a.hrAdmin() {
		v.SSOSubject, v.LegalHold, v.Erased = "", false, false
	}
	return v
}

// PersonInput creates a person.
type PersonInput struct {
	Kind          string `json:"kind"`
	Status        Status `json:"status"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	PreferredName string `json:"preferred_name"`
	WorkEmail     string `json:"work_email"`
	WorkPhone     string `json:"work_phone"`
	Location      string `json:"location"`
	DepartmentID  string `json:"department_id"`
	TeamID        string `json:"team_id"`
	PositionID    string `json:"position_id"`
	ManagerID     string `json:"manager_id"`
	StartDate     string `json:"start_date"`
	SSOSubject    string `json:"sso_subject"`
}

// PersonPatch updates basic fields; nil means unchanged.
type PersonPatch struct {
	GivenName     *string `json:"given_name"`
	FamilyName    *string `json:"family_name"`
	PreferredName *string `json:"preferred_name"`
	WorkEmail     *string `json:"work_email"`
	WorkPhone     *string `json:"work_phone"`
	Location      *string `json:"location"`
	DepartmentID  *string `json:"department_id"`
	TeamID        *string `json:"team_id"`
	PositionID    *string `json:"position_id"`
	SSOSubject    *string `json:"sso_subject"`
	StartDate     *string `json:"start_date"`
}

func (in *PersonInput) validate() error {
	if in.Kind != "employee" && in.Kind != "contractor" {
		return invalid("kind must be employee or contractor")
	}
	switch in.Status {
	case "":
		in.Status = StCandidate
	case StCandidate, StOnboarding, StActive:
	default:
		return invalid("a person can be created as candidate, onboarding or active; use the status endpoint for other changes")
	}
	for _, c := range []struct {
		n, v string
		max  int
		req  bool
	}{{"given_name", in.GivenName, 100, true}, {"family_name", in.FamilyName, 100, true}, {"preferred_name", in.PreferredName, 100, false},
		{"work_phone", in.WorkPhone, 40, false}, {"location", in.Location, 100, false}} {
		if err := validLine(c.n, c.v, c.max, c.req); err != nil {
			return err
		}
	}
	if err := validEmail(in.WorkEmail); err != nil {
		return err
	}
	if in.StartDate != "" {
		if err := validDate(in.StartDate); err != nil {
			return err
		}
	}
	if in.SSOSubject != "" && !subjectRe.MatchString(in.SSOSubject) {
		return invalid("sso_subject has an invalid format")
	}
	return nil
}

func (s *Service) orgRef(ctx context.Context, q querier, table, id string) error {
	if id == "" {
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&n); err != nil { //nolint:gosec // table is a constant at every call site
		return err
	}
	if n == 0 {
		return invalid("%s %q does not exist", strings.TrimSuffix(table, "s"), id)
	}
	return nil
}

// checkManager enforces: the manager exists and is staff in good standing, a person is not
// their own manager, and the new link creates no cycle in the reporting chain.
func (s *Service) checkManager(ctx context.Context, q querier, personID, managerID string) error {
	if managerID == "" {
		return nil
	}
	if !personIDRe.MatchString(managerID) {
		return invalid("manager_id has an invalid format")
	}
	if managerID == personID {
		return invalid("a person cannot be their own manager")
	}
	var st string
	var erased sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT status, erased_at FROM people WHERE id = ?`, managerID).Scan(&st, &erased)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("manager %q does not exist", managerID)
	}
	if err != nil {
		return err
	}
	if erased.Valid || (Status(st) != StActive && Status(st) != StLeave) {
		return invalid("a manager must be active or on leave (is %s)", st)
	}
	if personID == "" {
		return nil // a new person cannot be anyone's ancestor yet
	}
	chain, err := managerChain(ctx, q, managerID)
	if err != nil {
		return err
	}
	if slices.Contains(chain, personID) {
		return invalid("that manager assignment would create a reporting cycle")
	}
	if len(chain) >= maxChainDepth-1 {
		return invalid("reporting chain is too deep (max %d levels)", maxChainDepth)
	}
	return nil
}

// CreatePerson creates a directory record. hr or admin only.
func (s *Service) CreatePerson(ctx context.Context, a Actor, in PersonInput) (*Person, error) {
	return s.createPerson(ctx, a, in, "")
}

// createPerson is CreatePerson plus the hire reference of the jobs hand-off ("" for none).
func (s *Service) createPerson(ctx context.Context, a Actor, in PersonInput, hireRef string) (*Person, error) {
	if !Decide(a, ActDirCreate, Fact{}) {
		s.auditDeny(ctx, a, "create", "person", nil)
		return nil, ErrForbidden
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	id := newID("pers")
	now := s.now().Unix()
	err := s.tx(ctx, func(tx *sqlTx) error {
		for table, ref := range map[string]string{"departments": in.DepartmentID, "teams": in.TeamID, "positions": in.PositionID} {
			if err := s.orgRef(ctx, tx, table, ref); err != nil {
				return err
			}
		}
		if err := s.checkManager(ctx, tx, "", in.ManagerID); err != nil {
			return err
		}
		start := in.StartDate
		if _, err := tx.ExecContext(ctx, `INSERT INTO people (id, kind, status, sso_subject, given_name, family_name, preferred_name, work_email, work_phone, location,
			department_id, team_id, position_id, manager_id, start_date, created_at, updated_at, hire_ref) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, in.Kind, string(in.Status), nullStr(in.SSOSubject), in.GivenName, in.FamilyName, in.PreferredName, in.WorkEmail, in.WorkPhone, in.Location,
			nullStr(in.DepartmentID), nullStr(in.TeamID), nullStr(in.PositionID), nullStr(in.ManagerID), start, now, now, nullStr(hireRef)); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return conflict("that sso_subject is already linked to a person")
			}
			return err
		}
		eff := s.today() // the record's own history starts when it is created, not at the start date
		if _, err := tx.ExecContext(ctx, `INSERT INTO status_history (person_id, from_status, to_status, effective_date, applied, recorded_at, recorded_by) VALUES (?,?,?,?,1,?,?)`,
			id, "", string(in.Status), eff, now, a.auditID()); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, EventPersonCreated, map[string]any{"party_id": id, "kind": in.Kind, "status": string(in.Status)}); err != nil {
			return err
		}
		return s.autoChecklists(ctx, tx, id, in.Status, a)
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "create", "person:"+id, nil); err != nil {
		return nil, err
	}
	return s.GetPerson(ctx, a, id)
}

// GetPerson returns the basic view. Callers who may not see the person get ErrNotFound, so ids
// cannot be probed.
func (s *Service) GetPerson(ctx context.Context, a Actor, id string) (*Person, error) {
	if !personIDRe.MatchString(id) {
		id = absentPersonID // same code path as an id that does not exist
	}
	f, si, err := s.factFor(ctx, s.db, a, id)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActDirRead, f) || (si.erased && !a.hrAdmin()) {
		return nil, ErrNotFound
	}
	p, err := scanPerson(s.db.QueryRowContext(ctx, `SELECT `+personCols+` FROM people WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	v := p.view(a)
	v.maskLeave(a, f)
	return &v, nil
}

func maskedLeave(st Status) Status {
	if st == StLeave {
		return StActive
	}
	return st
}

// maskLeave presents "leave" as "active" to colleagues: that someone is on leave can reveal
// health or family circumstances. The person, their manager chain and hr/admin see the truth.
//
// updated_at is masked the same way (colleagues see created_at): a record that changed around the
// time leave began would otherwise give it away.
func (p *Person) maskLeave(a Actor, f Fact) {
	if !f.Self && !f.Chain && !a.hrAdmin() {
		p.UpdatedAt = p.CreatedAt
		if p.Status == StLeave {
			p.Status = StActive
		}
	}
}

// PersonFilter narrows ListPeople.
type PersonFilter struct {
	Status       Status
	DepartmentID string
	TeamID       string
	ManagerID    string
	AfterSeq     int64
	Limit        int
}

// ListPeople returns the people the actor may see, with a cursor (the last seq) for more.
func (s *Service) ListPeople(ctx context.Context, a Actor, flt PersonFilter) ([]Person, int64, error) {
	if flt.Limit <= 0 || flt.Limit > 100 {
		flt.Limit = 50
	}
	if a.blocked() {
		return nil, 0, nil
	}
	q := `SELECT ` + personCols + ` FROM people WHERE seq > ?`
	args := []any{flt.AfterSeq}
	// The status filter is applied after masking (below), never in SQL: filtering on "leave"
	// would otherwise reveal who is on leave to people who may not know.
	for col, v := range map[string]string{"department_id": flt.DepartmentID, "team_id": flt.TeamID, "manager_id": flt.ManagerID} {
		if v != "" {
			q += " AND " + col + " = ?"
			args = append(args, v)
		}
	}
	q += " ORDER BY seq"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	var all []*Person
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		all = append(all, p)
	}
	_ = rows.Close()
	var out []Person
	var next int64
	for _, p := range all {
		if p.Erased && !a.hrAdmin() {
			continue
		}
		f, _, err := s.factFor(ctx, s.db, a, p.ID)
		if err != nil {
			return nil, 0, err
		}
		if !Decide(a, ActDirRead, f) {
			continue
		}
		v := p.view(a)
		v.maskLeave(a, f)
		if flt.Status != "" && v.Status != flt.Status {
			continue
		}
		if len(out) == flt.Limit {
			next = out2seq(out, all)
			break
		}
		out = append(out, v)
	}
	return out, next, nil
}

// out2seq finds the seq of the last returned person (the cursor).
func out2seq(out []Person, all []*Person) int64 {
	last := out[len(out)-1].ID
	for _, p := range all {
		if p.ID == last {
			return p.seq
		}
	}
	return 0
}

// UpdatePerson changes basic fields. hr/admin may change any; a person may change only
// preferred_name and work_phone on their own record.
func (s *Service) UpdatePerson(ctx context.Context, a Actor, id string, patch PersonPatch) (*Person, error) {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	changed := map[string]*string{"given_name": patch.GivenName, "family_name": patch.FamilyName, "preferred_name": patch.PreferredName,
		"work_email": patch.WorkEmail, "work_phone": patch.WorkPhone, "location": patch.Location, "department_id": patch.DepartmentID,
		"team_id": patch.TeamID, "position_id": patch.PositionID, "sso_subject": patch.SSOSubject, "start_date": patch.StartDate}
	selfOnly := true
	var names []string
	for k, v := range changed {
		if v != nil {
			names = append(names, k)
			if !selfEditableBasic[k] {
				selfOnly = false
			}
		}
	}
	if len(names) == 0 {
		return nil, invalid("no fields to change")
	}
	allowed := Decide(a, ActDirUpdate, f) || (selfOnly && Decide(a, ActDirUpdateSelf, f))
	if !allowed {
		s.auditDeny(ctx, a, "write", "person:"+id, names)
		return nil, ErrForbidden
	}
	if si.erased || si.status == StFormer && !a.hrAdmin() {
		return nil, ErrNotFound
	}
	for _, c := range []struct {
		n string
		v *string
		m int
	}{{"given_name", patch.GivenName, 100}, {"family_name", patch.FamilyName, 100}, {"preferred_name", patch.PreferredName, 100}, {"work_phone", patch.WorkPhone, 40}, {"location", patch.Location, 100}} {
		if c.v != nil {
			if err := validLine(c.n, *c.v, c.m, c.n == "given_name" || c.n == "family_name"); err != nil {
				return nil, err
			}
		}
	}
	if patch.WorkEmail != nil {
		if err := validEmail(*patch.WorkEmail); err != nil {
			return nil, err
		}
	}
	if patch.StartDate != nil && *patch.StartDate != "" {
		if err := validDate(*patch.StartDate); err != nil {
			return nil, err
		}
	}
	if patch.SSOSubject != nil && *patch.SSOSubject != "" && !subjectRe.MatchString(*patch.SSOSubject) {
		return nil, invalid("sso_subject has an invalid format")
	}
	err = s.tx(ctx, func(tx *sqlTx) error {
		for table, ref := range map[string]*string{"departments": patch.DepartmentID, "teams": patch.TeamID, "positions": patch.PositionID} {
			if ref != nil {
				if err := s.orgRef(ctx, tx, table, *ref); err != nil {
					return err
				}
			}
		}
		sets, args := []string{"updated_at = ?"}, []any{s.now().Unix()}
		for k, v := range changed {
			if v == nil {
				continue
			}
			switch k {
			case "department_id", "team_id", "position_id", "sso_subject":
				sets = append(sets, k+" = ?")
				args = append(args, nullStr(*v))
			default:
				sets = append(sets, k+" = ?")
				args = append(args, *v)
			}
		}
		args = append(args, id)
		if _, err := tx.ExecContext(ctx, `UPDATE people SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return conflict("that sso_subject is already linked to a person")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "write", "person:"+id, nil); err != nil {
		return nil, err
	}
	return s.GetPerson(ctx, a, id)
}

// personFact resolves the fact for a person, returning ErrNotFound when the actor cannot see
// the person at all (the 404-not-403 rule that stops id probing).
func (s *Service) personFact(ctx context.Context, a Actor, id string) (Fact, *subjectInfo, error) {
	if !personIDRe.MatchString(id) {
		id = absentPersonID
	}
	f, si, err := s.factFor(ctx, s.db, a, id)
	if err != nil {
		return f, si, err
	}
	if !Decide(a, ActDirRead, f) {
		return f, si, ErrNotFound
	}
	return f, si, nil
}

// SetManager assigns or clears (managerID "") the manager. hr or admin only; cycles are rejected.
func (s *Service) SetManager(ctx context.Context, a Actor, id, managerID string) (*Person, error) {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActDirOrg, f) {
		s.auditDeny(ctx, a, "write", "person:"+id, []string{"manager_id"})
		return nil, ErrForbidden
	}
	if si.erased || si.status == StFormer {
		return nil, invalid("cannot change the manager of a %s record", si.status)
	}
	err = s.tx(ctx, func(tx *sqlTx) error {
		if err := s.checkManager(ctx, tx, id, managerID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE people SET manager_id = ?, updated_at = ? WHERE id = ?`, nullStr(managerID), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "write", "person:"+id, []string{"manager_id"}); err != nil {
		return nil, err
	}
	return s.GetPerson(ctx, a, id)
}

// Reports lists the direct reports the actor may see.
func (s *Service) Reports(ctx context.Context, a Actor, id string) ([]Person, error) {
	if _, _, err := s.personFact(ctx, a, id); err != nil {
		return nil, err
	}
	out, _, err := s.ListPeople(ctx, a, PersonFilter{ManagerID: id, Limit: 100})
	return out, err
}

// ---- organisation structure ----------------------------------------------------------------

// OrgUnit is a department, team or position.
type OrgUnit struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DepartmentID string `json:"department_id,omitempty"`
	LeadID       string `json:"lead_id,omitempty"`
}

// OrgInput creates a department, team or position (Name is the title for positions).
type OrgInput struct {
	Name         string `json:"name"`
	DepartmentID string `json:"department_id"`
	LeadID       string `json:"lead_id"`
}

// CreateOrgUnit creates a department, team or position. kind: departments | teams | positions.
func (s *Service) CreateOrgUnit(ctx context.Context, a Actor, kind string, in OrgInput) (*OrgUnit, error) {
	if !Decide(a, ActDirOrg, Fact{}) {
		s.auditDeny(ctx, a, "create", "org:"+kind, nil)
		return nil, ErrForbidden
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	id := newID(map[string]string{"departments": "dep", "teams": "team", "positions": "pos"}[kind])
	now := s.now().Unix()
	err := s.tx(ctx, func(tx *sqlTx) error {
		if err := s.orgRef(ctx, tx, "departments", in.DepartmentID); err != nil {
			return err
		}
		var res error
		switch kind {
		case "departments":
			_, res = tx.ExecContext(ctx, `INSERT INTO departments (id, name, created_at) VALUES (?,?,?)`, id, in.Name, now)
		case "teams":
			if in.LeadID != "" {
				if err := s.orgRef(ctx, tx, "people", in.LeadID); err != nil {
					return err
				}
			}
			_, res = tx.ExecContext(ctx, `INSERT INTO teams (id, name, department_id, lead_id, created_at) VALUES (?,?,?,?,?)`, id, in.Name, nullStr(in.DepartmentID), nullStr(in.LeadID), now)
		case "positions":
			_, res = tx.ExecContext(ctx, `INSERT INTO positions (id, title, department_id, created_at) VALUES (?,?,?,?)`, id, in.Name, nullStr(in.DepartmentID), now)
		default:
			return invalid("unknown organisation unit")
		}
		if res != nil && strings.Contains(res.Error(), "UNIQUE") {
			return conflict("%s already exists", in.Name)
		}
		return res
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "create", "org:"+id, nil); err != nil {
		return nil, err
	}
	return &OrgUnit{ID: id, Name: in.Name, DepartmentID: in.DepartmentID, LeadID: in.LeadID}, nil
}

// ListOrgUnits lists departments, teams or positions for any staff member or hr/admin.
func (s *Service) ListOrgUnits(ctx context.Context, a Actor, kind string) ([]OrgUnit, error) {
	if !a.staff() && !a.hrAdmin() {
		return nil, ErrForbidden
	}
	var q string
	switch kind {
	case "departments":
		q = `SELECT id, name, '', '' FROM departments ORDER BY name`
	case "teams":
		q = `SELECT id, name, COALESCE(department_id,''), COALESCE(lead_id,'') FROM teams ORDER BY name`
	case "positions":
		q = `SELECT id, title, COALESCE(department_id,''), '' FROM positions ORDER BY title`
	default:
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []OrgUnit{}
	for rows.Next() {
		var u OrgUnit
		if err := rows.Scan(&u.ID, &u.Name, &u.DepartmentID, &u.LeadID); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- sensitive fields ----------------------------------------------------------------------

// Salary is the compensation value, in minor currency units (cents).
type Salary struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Period   string `json:"period"` // year | month | hour
}

var sensCols = map[string]string{FieldDOB: "sens_dob", FieldNationalID: "sens_national_id", FieldAddress: "sens_address", FieldPhone: "sens_phone", FieldSalary: "sens_salary"}

func sensAAD(personID, field string) string { return personID + ":" + field }

// GetSensitive returns people-sensitive fields the actor may read. With no fields requested it
// returns every field the actor is allowed; requesting a field the actor may not read refuses
// the whole request. The read is audited per field before any data is decrypted or returned;
// when the audit hook fails the read fails.
func (s *Service) GetSensitive(ctx context.Context, a Actor, id string, fields []string) (map[string]any, error) {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if si.erased {
		return nil, ErrNotFound
	}
	var allowed []string
	if len(fields) == 0 {
		for _, fld := range SensitiveFields {
			if FieldAccess(a, false, fld, f) {
				allowed = append(allowed, fld)
			}
		}
	} else {
		for _, fld := range fields {
			if !IsSensitiveField(fld) {
				return nil, invalid("unknown field %q", fld)
			}
			if !FieldAccess(a, false, fld, f) {
				s.auditDeny(ctx, a, "read", "person:"+id, fields)
				return nil, ErrForbidden
			}
			if !slices.Contains(allowed, fld) {
				allowed = append(allowed, fld)
			}
		}
	}
	if len(allowed) == 0 {
		s.auditDeny(ctx, a, "read", "person:"+id, fields)
		return nil, ErrForbidden
	}
	if err := s.auditRead(ctx, a, "person:"+id, allowed); err != nil {
		return nil, fmt.Errorf("audit unavailable, read refused: %w", err)
	}
	out := map[string]any{}
	for _, fld := range allowed {
		var sealed sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT `+sensCols[fld]+` FROM people WHERE id = ?`, id).Scan(&sealed); err != nil { //nolint:gosec // column from a fixed map
			return nil, err
		}
		if !sealed.Valid {
			out[fld] = nil
			continue
		}
		plain, err := s.keys.Open(sealed.String, sensAAD(id, fld))
		if err != nil {
			return nil, errors.New("stored value cannot be decrypted")
		}
		if fld == FieldSalary {
			var sal Salary
			if err := json.Unmarshal([]byte(plain), &sal); err != nil {
				return nil, errors.New("stored value is corrupt")
			}
			out[fld] = sal
		} else {
			out[fld] = plain
		}
	}
	return out, nil
}

func sensitiveValue(field string, raw json.RawMessage) (string, error) {
	var asString string
	switch field {
	case FieldSalary:
		var sal Salary
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sal); err != nil {
			return "", invalid("salary must be {amount, currency, period}")
		}
		if sal.Amount < 0 || sal.Amount > math.MaxInt64/2 || len(sal.Currency) != 3 || strings.ToUpper(sal.Currency) != sal.Currency || !slices.Contains([]string{"year", "month", "hour"}, sal.Period) {
			return "", invalid("salary needs a non-negative amount in minor units, a 3-letter upper-case currency and a period of year, month or hour")
		}
		b, _ := json.Marshal(sal)
		return string(b), nil
	}
	if err := json.Unmarshal(raw, &asString); err != nil {
		return "", invalid("%s must be a string", field)
	}
	switch field {
	case FieldDOB:
		if err := validDate(asString); err != nil {
			return "", invalid("date_of_birth must be YYYY-MM-DD")
		}
	case FieldNationalID:
		if err := validLine(field, asString, 64, true); err != nil {
			return "", err
		}
	case FieldAddress:
		if err := validText(field, asString, 500, true); err != nil {
			return "", err
		}
	case FieldPhone:
		if err := validLine(field, asString, 40, true); err != nil {
			return "", err
		}
	}
	return asString, nil
}

// PutSensitive sets or clears (null) sensitive fields. All fields are authorised first, then
// all are written or none. Values are never logged or audited, only the field names.
func (s *Service) PutSensitive(ctx context.Context, a Actor, id string, values map[string]json.RawMessage) error {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return err
	}
	if si.erased {
		return ErrNotFound
	}
	if len(values) == 0 {
		return invalid("no fields to change")
	}
	var names []string
	for fld := range values {
		if !IsSensitiveField(fld) {
			return invalid("unknown field %q", fld)
		}
		names = append(names, fld)
	}
	slices.Sort(names)
	for _, fld := range names {
		if !FieldAccess(a, true, fld, f) {
			s.auditDeny(ctx, a, "write", "person:"+id, names)
			return ErrForbidden
		}
	}
	sealed := map[string]any{}
	for _, fld := range names {
		raw := values[fld]
		if string(raw) == "null" {
			sealed[fld] = nil
			continue
		}
		v, err := sensitiveValue(fld, raw)
		if err != nil {
			return err
		}
		c, err := s.keys.Seal(v, sensAAD(id, fld))
		if err != nil {
			return err
		}
		sealed[fld] = c
	}
	err = s.tx(ctx, func(tx *sqlTx) error {
		for _, fld := range names {
			if _, err := tx.ExecContext(ctx, `UPDATE people SET `+sensCols[fld]+` = ?, updated_at = ? WHERE id = ?`, sealed[fld], s.now().Unix(), id); err != nil { //nolint:gosec // column from a fixed map
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.auditWrite(ctx, a, "write", "person:"+id, names)
}

// ---- employment lifecycle --------------------------------------------------------------------

var transitions = map[Status][]Status{
	StCandidate:   {StOnboarding, StFormer},
	StOnboarding:  {StActive, StOffboarding, StFormer},
	StActive:      {StLeave, StOffboarding},
	StLeave:       {StActive, StOffboarding},
	StOffboarding: {StFormer, StActive},
}

// HistoryEntry is one effective-dated status change.
type HistoryEntry struct {
	From          string `json:"from"`
	To            string `json:"to"`
	EffectiveDate string `json:"effective_date"`
	Applied       bool   `json:"applied"`
	RecordedAt    string `json:"recorded_at"`
	RecordedBy    string `json:"recorded_by"`
	Note          string `json:"note,omitempty"`
}

// ChangeStatus moves a person along the lifecycle. A change dated today or earlier applies at
// once; a future-dated change is stored as pending and applied by ApplyDue on its date. Only one
// pending change per person; the effective date cannot precede the last applied change.
func (s *Service) ChangeStatus(ctx context.Context, a Actor, id string, to Status, effective, note string) (*Person, error) {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActDirStatus, f) {
		s.auditDeny(ctx, a, "write", "person:"+id, []string{"status"})
		return nil, ErrForbidden
	}
	if si.erased {
		return nil, ErrNotFound
	}
	if effective == "" {
		effective = s.today()
	}
	if err := validDate(effective); err != nil {
		return nil, err
	}
	if err := validText("note", note, 500, false); err != nil {
		return nil, err
	}
	// Notes are free text about a person's employment (reasons for leave or exit): sealed.
	if note != "" {
		if note, err = s.keys.Seal(note, id+":status_note"); err != nil {
			return nil, err
		}
	}
	err = s.tx(ctx, func(tx *sqlTx) error {
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM status_history WHERE person_id = ? AND applied = 0`, id).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return conflict("a future-dated status change is already pending; cancel it first")
		}
		if !slices.Contains(transitions[si.status], to) {
			return conflict("cannot change status from %s to %s", si.status, to)
		}
		var last string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(effective_date),'') FROM status_history WHERE person_id = ? AND applied = 1`, id).Scan(&last); err != nil {
			return err
		}
		if effective < last {
			return invalid("effective date %s is before the last change (%s)", effective, last)
		}
		if effective > s.today() {
			_, err := tx.ExecContext(ctx, `INSERT INTO status_history (person_id, from_status, to_status, effective_date, applied, recorded_at, recorded_by, note) VALUES (?,?,?,?,0,?,?,?)`,
				id, string(si.status), string(to), effective, s.now().Unix(), a.auditID(), note)
			return err
		}
		return s.applyStatus(ctx, tx, id, si.status, to, effective, a.auditID(), note, 0)
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "write", "person:"+id, []string{"status"}); err != nil {
		return nil, err
	}
	return s.GetPerson(ctx, a, id)
}

// applyStatus performs a transition inside a transaction: row update, history, events, and the
// side effects of offboarding. histID is the pending row to mark applied (0 inserts a new one).
func (s *Service) applyStatus(ctx context.Context, tx *sqlTx, id string, from, to Status, date, by, note string, histID int64) error {
	now := s.now().Unix()
	if histID == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO status_history (person_id, from_status, to_status, effective_date, applied, recorded_at, recorded_by, note) VALUES (?,?,?,?,1,?,?,?)`,
			id, string(from), string(to), date, now, by, note); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE status_history SET applied = 1 WHERE seq = ? AND applied = 0`, histID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE people SET status = ?, updated_at = ? WHERE id = ?`, string(to), now, id); err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM people WHERE id = ?`, id).Scan(&kind); err != nil {
		return err
	}
	if to == StActive {
		if _, err := tx.ExecContext(ctx, `UPDATE people SET start_date = ? WHERE id = ? AND start_date = ''`, date, id); err != nil {
			return err
		}
	}
	if to == StFormer {
		// The reporting line ends with the employment: reports lose their manager link (HR
		// reassigns them) and a former person can lead no team. Fails closed: nobody keeps
		// manager access through a terminated manager.
		for _, q := range []string{
			`UPDATE people SET manager_id = NULL, updated_at = ` + fmt.Sprint(now) + ` WHERE manager_id = ?`,
			`UPDATE teams SET lead_id = NULL WHERE lead_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE people SET ended_at = ?, end_date = ?, manager_id = NULL WHERE id = ?`, now, date, id); err != nil {
			return err
		}
	}
	// Leave can reveal health or family circumstances and event subscribers are not all HR, so
	// the event masks it exactly like the directory does for colleagues: leave is published as
	// active, and a change between active and leave publishes nothing.
	pf, pt := maskedLeave(from), maskedLeave(to)
	if pf != pt {
		if err := s.enqueue(ctx, tx, EventPersonStatusChanged, map[string]any{"party_id": id, "kind": kind, "from": string(pf), "to": string(pt), "effective_date": date}); err != nil {
			return err
		}
	}
	if to == StFormer {
		if err := s.enqueue(ctx, tx, EventPersonOffboarded, map[string]any{"party_id": id, "kind": kind, "effective_date": date}); err != nil {
			return err
		}
	}
	return s.autoChecklists(ctx, tx, id, to, Actor{Subject: by, PersonID: by})
}

// CancelPending removes a future-dated status change. hr or admin only.
func (s *Service) CancelPending(ctx context.Context, a Actor, id string) error {
	f, _, err := s.personFact(ctx, a, id)
	if err != nil {
		return err
	}
	if !Decide(a, ActDirStatus, f) {
		s.auditDeny(ctx, a, "write", "person:"+id, []string{"status"})
		return ErrForbidden
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM status_history WHERE person_id = ? AND applied = 0`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.auditWrite(ctx, a, "write", "person:"+id, []string{"status"})
}

// ApplyDue applies every pending status change whose effective date has arrived. It runs at
// start-up and hourly.
func (s *Service) ApplyDue(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, person_id, from_status, to_status, effective_date, recorded_by, note FROM status_history WHERE applied = 0 AND effective_date <= ? ORDER BY seq`, s.today())
	if err != nil {
		return 0, err
	}
	type due struct {
		seq                          int64
		id, from, to, date, by, note string
	}
	var ds []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.seq, &d.id, &d.from, &d.to, &d.date, &d.by, &d.note); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ds = append(ds, d)
	}
	_ = rows.Close()
	n := 0
	for _, d := range ds {
		applied := false
		err := s.tx(ctx, func(tx *sqlTx) error {
			// Re-read inside the transaction: with one connection a concurrent caller (the lazy
			// check at authentication and the background tick) has either finished or not
			// started, so a row that is no longer pending is simply skipped.
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM status_history WHERE seq = ? AND applied = 0`, d.seq).Scan(&pending); err != nil {
				return err
			}
			if pending == 0 {
				return nil
			}
			var cur string
			if err := tx.QueryRowContext(ctx, `SELECT status FROM people WHERE id = ?`, d.id).Scan(&cur); err != nil {
				return err
			}
			if cur != d.from || !slices.Contains(transitions[Status(cur)], Status(d.to)) {
				// stale: drop the pending row rather than apply it blindly; never an applied one
				_, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE seq = ? AND applied = 0`, d.seq)
				return err
			}
			applied = true
			return s.applyStatus(ctx, tx, d.id, Status(d.from), Status(d.to), d.date, d.by, d.note, d.seq)
		})
		if err != nil {
			return n, err
		}
		if applied {
			n++
		}
	}
	// Approved long leave moves people to (and back from) the leave status (timeoff.go).
	k, err := s.ReconcileLeaveStatus(ctx)
	return n + k, err
}

// History returns the status history. The person themself and hr/admin only.
func (s *Service) History(ctx context.Context, a Actor, id string) ([]HistoryEntry, error) {
	f, _, err := s.personFact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActDirHistory, f) {
		s.auditDeny(ctx, a, "read", "history:"+id, nil)
		return nil, ErrForbidden
	}
	if err := s.auditRead(ctx, a, "history:"+id, []string{"status_history"}); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT from_status, to_status, effective_date, applied, recorded_at, recorded_by, note FROM status_history WHERE person_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HistoryEntry{}
	for rows.Next() {
		var h HistoryEntry
		var applied int
		var at int64
		if err := rows.Scan(&h.From, &h.To, &h.EffectiveDate, &applied, &at, &h.RecordedBy, &h.Note); err != nil {
			return nil, err
		}
		h.Applied, h.RecordedAt = applied == 1, time.Unix(at, 0).UTC().Format(time.RFC3339)
		if h.Note != "" {
			if plain, err := s.keys.Open(h.Note, id+":status_note"); err == nil {
				h.Note = plain
			} else {
				h.Note = ""
			}
		}
		if !a.hrAdmin() {
			h.RecordedBy = "" // who recorded a change is for HR; employees see what and when
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SensitiveFieldsFor lists the sensitive fields the actor could read for the person, without
// reading or decrypting any (so nothing is audited as a read). It answers ErrNotFound when the
// person is invisible and ErrForbidden when no field is readable; the web UI uses it to decide
// whether to offer the reveal button.
func (s *Service) SensitiveFieldsFor(ctx context.Context, a Actor, id string) ([]string, error) {
	f, si, err := s.personFact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if si.erased {
		return nil, ErrNotFound
	}
	var out []string
	for _, fld := range SensitiveFields {
		if FieldAccess(a, false, fld, f) {
			out = append(out, fld)
		}
	}
	if len(out) == 0 {
		return nil, ErrForbidden
	}
	return out, nil
}

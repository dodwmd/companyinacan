package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---- shared helpers ----------------------------------------------------------------------------

// seal and open bind a sealed value to its row and column (AAD), so a ciphertext copied to another
// row or field does not open.
func (s *Service) seal(id, field, plain string) (string, error) {
	return s.keys.Seal(plain, id+":"+field)
}

func (s *Service) open(id, field string, sealed sql.NullString) (string, error) {
	if !sealed.Valid {
		return "", nil
	}
	return s.keys.Open(sealed.String, id+":"+field)
}

func (s *Service) sealNull(id, field, plain string) (any, error) {
	if plain == "" {
		return nil, nil
	}
	return s.seal(id, field, plain)
}

// hide returns ErrNotFound (and audits the refusal) when the actor may not see the record: an
// invisible record and a missing one look the same, so there is no existence oracle.
func (s *Service) hide(ctx context.Context, a Actor, act Action, f Fact, resource string) error {
	if !Decide(a, act, f) {
		s.auditDeny(ctx, a, "read", resource, nil)
		return ErrNotFound
	}
	return nil
}

// ---- relationship facts --------------------------------------------------------------------------

// memberRole is the actor's role in the body on a day ("" when not in term).
func (s *Service) memberRole(ctx context.Context, q querier, bodyID, person, day string) (string, error) {
	if person == "" {
		return "", nil
	}
	var role string
	err := q.QueryRowContext(ctx, `SELECT role FROM body_members WHERE body_id = ? AND person_id = ? AND start_date <= ? AND (end_date IS NULL OR end_date >= ?)`, bodyID, person, day, day).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (s *Service) exists(ctx context.Context, q querier, query string, args ...any) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM (`+query+`) LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) bodyFact(ctx context.Context, q querier, a Actor, bodyID string) (Fact, error) {
	f := Fact{}
	if a.PersonID == "" {
		return f, nil
	}
	role, err := s.memberRole(ctx, q, bodyID, a.PersonID, s.today())
	if err != nil {
		return f, err
	}
	switch role {
	case "chair":
		f.Member, f.Chair = true, true
	case "member":
		f.Member = true
	case "observer":
		f.ObserverOf = true
	}
	f.Invited, err = s.exists(ctx, q, `SELECT 1 FROM meeting_invitees i JOIN meetings m ON m.id = i.meeting_id WHERE m.body_id = ? AND i.person_id = ?`, bodyID, a.PersonID)
	return f, err
}

// entityMemberFact is the fact for a company record: the actor is a member (in term) of a body that is
// tied to this entity by its entity_ref. A body with no entity_ref gives no access to any entity record.
func (s *Service) entityMemberFact(ctx context.Context, q querier, a Actor, entityID string) (Fact, error) {
	f := Fact{}
	if a.PersonID == "" {
		return f, nil
	}
	m, err := s.exists(ctx, q, `SELECT 1 FROM body_members m JOIN bodies b ON b.id = m.body_id WHERE m.person_id = ? AND m.role IN ('chair', 'member') AND b.entity_ref = ? AND m.start_date <= ? AND (m.end_date IS NULL OR m.end_date >= ?)`,
		a.PersonID, entityID, s.today(), s.today())
	f.Member = m
	return f, err
}

// ---- bodies ----------------------------------------------------------------------------------------

// BodyInput creates a board or committee. Quorum and pass rules are data (a fraction and an optional
// minimum), not jurisdiction logic: leave them zero for "more than half present, simple majority".
type BodyInput struct {
	Name                  string `json:"name"`
	Kind                  string `json:"kind"`
	ParentID              string `json:"parent_id"`
	EntityRef             string `json:"entity_ref"`
	QuorumNum             int    `json:"quorum_num"`
	QuorumDen             int    `json:"quorum_den"`
	QuorumMin             int    `json:"quorum_min"`
	QuorumExcludesRecused *bool  `json:"quorum_excludes_recused"`
	PassNum               int    `json:"pass_num"`
	PassDen               int    `json:"pass_den"`
	PassInclusive         bool   `json:"pass_inclusive"`
	NoticeDays            int    `json:"notice_days"`
	NoticeBasis           string `json:"notice_basis"`
}

// Body is a board or committee.
type Body struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Kind                  string `json:"kind"`
	ParentID              string `json:"parent_id,omitempty"`
	EntityRef             string `json:"entity_ref,omitempty"`
	QuorumNum             int    `json:"quorum_num"`
	QuorumDen             int    `json:"quorum_den"`
	QuorumMin             int    `json:"quorum_min"`
	QuorumExcludesRecused bool   `json:"quorum_excludes_recused"`
	PassNum               int    `json:"pass_num"`
	PassDen               int    `json:"pass_den"`
	PassInclusive         bool   `json:"pass_inclusive"`
	NoticeDays            int    `json:"notice_days"`
	NoticeBasis           string `json:"notice_basis,omitempty"`
	CreatedAt             string `json:"created_at"`
}

const bodyCols = `id, name, kind, IFNULL(parent_id,''), entity_ref, quorum_num, quorum_den, quorum_min, quorum_excludes_recused, pass_num, pass_den, pass_inclusive, notice_days, notice_basis, created_at`

func scanBody(r interface{ Scan(...any) error }) (*Body, error) {
	var b Body
	var ex, inc int
	var at int64
	if err := r.Scan(&b.ID, &b.Name, &b.Kind, &b.ParentID, &b.EntityRef, &b.QuorumNum, &b.QuorumDen, &b.QuorumMin, &ex, &b.PassNum, &b.PassDen, &inc, &b.NoticeDays, &b.NoticeBasis, &at); err != nil {
		return nil, err
	}
	b.QuorumExcludesRecused, b.PassInclusive = ex == 1, inc == 1
	b.CreatedAt = time.Unix(at, 0).UTC().Format(time.RFC3339)
	return &b, nil
}

func (s *Service) loadBody(ctx context.Context, q querier, id string) (*Body, error) {
	if err := validID("bdy", id); err != nil {
		return nil, err
	}
	b, err := scanBody(q.QueryRowContext(ctx, `SELECT `+bodyCols+` FROM bodies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func validFraction(name string, num, den int) error {
	if den < 1 || den > 1000 || num < 0 || num > den {
		return invalid("%s must be a fraction num/den with 0 <= num <= den and 1 <= den <= 1000", name)
	}
	return nil
}

// CreateBody creates a body. secretary or admin.
func (s *Service) CreateBody(ctx context.Context, a Actor, in BodyInput) (*Body, error) {
	if err := s.authorize(ctx, a, ActBodyManage, Fact{}, "bodies"); err != nil {
		return nil, err
	}
	if err := validLine("name", in.Name, 120, true); err != nil {
		return nil, err
	}
	if in.Kind != "board" && in.Kind != "committee" {
		return nil, invalid("kind must be board or committee")
	}
	if in.QuorumDen == 0 {
		in.QuorumNum, in.QuorumDen = 1, 2
	}
	if in.PassDen == 0 {
		in.PassNum, in.PassDen, in.PassInclusive = 1, 2, false
	}
	if err := validFraction("quorum", in.QuorumNum, in.QuorumDen); err != nil {
		return nil, err
	}
	if err := validFraction("pass threshold", in.PassNum, in.PassDen); err != nil {
		return nil, err
	}
	if in.QuorumMin < 0 || in.QuorumMin > 1000 || in.NoticeDays < 0 || in.NoticeDays > 366 {
		return nil, invalid("quorum_min must be 0 to 1000 and notice_days 0 to 366")
	}
	if err := validRef("entity_ref", in.EntityRef, false); err != nil {
		return nil, err
	}
	if err := validLine("notice_basis", in.NoticeBasis, 200, false); err != nil {
		return nil, err
	}
	if in.ParentID != "" {
		if _, err := s.loadBody(ctx, s.db, in.ParentID); err != nil {
			return nil, invalid("parent_id is not a body")
		}
	}
	excl := 1
	if in.QuorumExcludesRecused != nil && !*in.QuorumExcludesRecused {
		excl = 0
	}
	id := newID("bdy")
	now := s.now().Unix()
	err := s.txAudit(ctx, a, "write", "body:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO bodies (id, name, kind, parent_id, entity_ref, quorum_num, quorum_den, quorum_min, quorum_excludes_recused, pass_num, pass_den, pass_inclusive, notice_days, notice_basis, created_by, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, in.Name, in.Kind, nullStr(in.ParentID), in.EntityRef, in.QuorumNum, in.QuorumDen, in.QuorumMin, excl, in.PassNum, in.PassDen, b2i(in.PassInclusive), in.NoticeDays, in.NoticeBasis, canonSubject(a.Subject), now, now)
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("a body with this name exists")
		}
		return nil, err
	}
	return s.loadBody(ctx, s.db, id)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetBody returns a body the caller can see (404 otherwise).
func (s *Service) GetBody(ctx context.Context, a Actor, id string) (*Body, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	b, err := s.loadBody(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	f, err := s.bodyFact(ctx, s.db, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActBodyRead, f, "body:"+id); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "body:"+id, nil); err != nil {
		return nil, err
	}
	return b, nil
}

// ListBodies returns the bodies the caller can see.
func (s *Service) ListBodies(ctx context.Context, a Actor) ([]*Body, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if !a.known() {
		s.auditDeny(ctx, a, "read", "bodies", nil)
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+bodyCols+` FROM bodies ORDER BY name LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	var all []*Body
	for rows.Next() {
		b, err := scanBody(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, b)
	}
	_ = rows.Close()
	out := []*Body{}
	var res []string
	for _, b := range all {
		f, err := s.bodyFact(ctx, s.db, a, b.ID)
		if err != nil {
			return nil, err
		}
		if Decide(a, ActBodyRead, f) {
			out = append(out, b)
			res = append(res, "body:"+b.ID)
		}
	}
	if err := s.auditReadMany(ctx, a, "read", res, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- members ---------------------------------------------------------------------------------------

// MemberInput appoints a person to a body for a term.
type MemberInput struct {
	PersonID  string `json:"person_id"`
	Role      string `json:"role"` // chair | member | secretary | observer
	StartDate string `json:"start_date"`
}

// Member is one term of one person on one body.
type Member struct {
	ID        string `json:"id"`
	PersonID  string `json:"person_id"`
	Role      string `json:"role"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date,omitempty"`
}

// AddMember starts a term. secretary. A person has one open term per body; one chair at a time.
func (s *Service) AddMember(ctx context.Context, a Actor, bodyID string, in MemberInput) (*Member, error) {
	b, err := s.loadBody(ctx, s.db, bodyID)
	if err != nil {
		return nil, err
	}
	f, err := s.bodyFact(ctx, s.db, a, bodyID)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActBodyRead, f, "body:"+bodyID); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActMemberManage, f, "body:"+bodyID); err != nil {
		return nil, err
	}
	if err := validPerson("person_id", in.PersonID); err != nil {
		return nil, err
	}
	if in.Role != "chair" && in.Role != "member" && in.Role != "secretary" && in.Role != "observer" {
		return nil, invalid("role must be chair, member, secretary or observer")
	}
	if in.StartDate == "" {
		in.StartDate = s.today()
	}
	if _, err := parseDate(in.StartDate); err != nil {
		return nil, err
	}
	id := newID("mbr")
	err = s.txAudit(ctx, a, "write", "body:"+b.ID+"/members", nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO body_members (id, body_id, person_id, role, start_date, created_by, created_at) VALUES (?,?,?,?,?,?,?)`,
			id, bodyID, in.PersonID, in.Role, in.StartDate, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("this person already has an open term on the body, or the body already has a chair: end that term first")
		}
		return nil, err
	}
	return &Member{ID: id, PersonID: in.PersonID, Role: in.Role, StartDate: in.StartDate}, nil
}

// EndMember ends a term on a date (default today). The history stays.
func (s *Service) EndMember(ctx context.Context, a Actor, bodyID, memberID, endDate string) (*Member, error) {
	if _, err := s.loadBody(ctx, s.db, bodyID); err != nil {
		return nil, err
	}
	if err := validID("mbr", memberID); err != nil {
		return nil, err
	}
	f, err := s.bodyFact(ctx, s.db, a, bodyID)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActBodyRead, f, "body:"+bodyID); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActMemberManage, f, "body:"+bodyID); err != nil {
		return nil, err
	}
	if endDate == "" {
		endDate = s.today()
	}
	if _, err := parseDate(endDate); err != nil {
		return nil, err
	}
	var m Member
	var end sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT id, person_id, role, start_date, end_date FROM body_members WHERE id = ? AND body_id = ?`, memberID, bodyID).Scan(&m.ID, &m.PersonID, &m.Role, &m.StartDate, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if end.Valid {
		return nil, conflict("the term has already ended")
	}
	if endDate < m.StartDate {
		return nil, invalid("end_date is before the start of the term")
	}
	err = s.txAudit(ctx, a, "write", "body:"+bodyID+"/members", nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE body_members SET end_date = ?, ended_by = ? WHERE id = ? AND end_date IS NULL`, endDate, canonSubject(a.Subject), memberID)
		return err
	})
	m.EndDate = endDate
	return &m, err
}

// ListMembers returns the terms in force on a date (default today), or the whole history with as_of "all".
func (s *Service) ListMembers(ctx context.Context, a Actor, bodyID, asOf string) ([]Member, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if _, err := s.loadBody(ctx, s.db, bodyID); err != nil {
		return nil, err
	}
	f, err := s.bodyFact(ctx, s.db, a, bodyID)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActBodyRead, f, "body:"+bodyID); err != nil {
		return nil, err
	}
	q := `SELECT id, person_id, role, start_date, IFNULL(end_date,'') FROM body_members WHERE body_id = ?`
	args := []any{bodyID}
	if asOf != "all" {
		if asOf == "" {
			asOf = s.today()
		}
		if _, err := parseDate(asOf); err != nil {
			return nil, err
		}
		q += ` AND start_date <= ? AND (end_date IS NULL OR end_date >= ?)`
		args = append(args, asOf, asOf)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY start_date, id LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.PersonID, &m.Role, &m.StartDate, &m.EndDate); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "body:"+bodyID+"/members", nil); err != nil {
		return nil, err
	}
	return out, nil
}

// votingMembers lists the people in term as chair or member on a day.
func (s *Service) votingMembers(ctx context.Context, q querier, bodyID, day string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT person_id FROM body_members WHERE body_id = ? AND role IN ('chair','member') AND start_date <= ? AND (end_date IS NULL OR end_date >= ?) ORDER BY person_id`, bodyID, day, day)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

var _ = fmt.Sprintf
var _ = strings.TrimSpace

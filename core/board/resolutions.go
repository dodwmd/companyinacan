package board

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Statuses of a resolution.
const (
	ResDraft     = "draft"
	ResProposed  = "proposed"
	ResVoting    = "voting"
	ResPassed    = "passed"
	ResFailed    = "failed"
	ResWithdrawn = "withdrawn"
)

type resRow struct {
	ID, BodyID, MeetingID, Kind, Status      string
	Secret                                   bool
	Version                                  int
	ProposerRef, ProposerPerson              string
	PassNum, PassDen                         int
	PassInclusive                            bool
	ConsentDeadline, ExtType, ExtRef, Supers string
	Eligible, QReq, QPresent                 int
	For, Against, Abstain                    int
	CloseReason, ClosedOn                    string
}

const resCols = `id, body_id, IFNULL(meeting_id,''), kind, status, secret_ballot, current_version, proposer_ref, proposer_person, pass_num, pass_den, pass_inclusive,
	consent_deadline, external_type, external_ref, IFNULL(supersedes_id,''), eligible_count, quorum_required, quorum_present, result_for, result_against, result_abstain, close_reason, IFNULL(closed_on,'')`

func scanRes(r interface{ Scan(...any) error }) (*resRow, error) {
	var x resRow
	var sec, inc int
	err := r.Scan(&x.ID, &x.BodyID, &x.MeetingID, &x.Kind, &x.Status, &sec, &x.Version, &x.ProposerRef, &x.ProposerPerson, &x.PassNum, &x.PassDen, &inc,
		&x.ConsentDeadline, &x.ExtType, &x.ExtRef, &x.Supers, &x.Eligible, &x.QReq, &x.QPresent, &x.For, &x.Against, &x.Abstain, &x.CloseReason, &x.ClosedOn)
	x.Secret, x.PassInclusive = sec == 1, inc == 1
	return &x, err
}

func (s *Service) loadResolutionRow(ctx context.Context, q querier, id string) (*resRow, error) {
	if err := validID("res", id); err != nil {
		return nil, err
	}
	r, err := scanRes(q.QueryRowContext(ctx, `SELECT `+resCols+` FROM resolutions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (r *resRow) final() bool {
	return r.Status == ResPassed || r.Status == ResFailed || r.Status == ResWithdrawn
}

// resFact is the body fact plus the relationship to this resolution.
func (s *Service) resFact(ctx context.Context, q querier, a Actor, r *resRow) (Fact, error) {
	f, err := s.bodyFact(ctx, q, a, r.BodyID)
	if err != nil {
		return f, err
	}
	f.Final, f.Secret = r.final(), r.Secret
	cn := canonSubject(a.Subject)
	f.Proposer = r.ProposerRef == cn || (a.PersonID != "" && r.ProposerPerson == a.PersonID) ||
		(a.has(RoleSecretary) && r.ExtType != "" && r.ProposerPerson == "")
	if a.PersonID == "" {
		return f, nil
	}
	if f.Subject, err = s.exists(ctx, q, `SELECT 1 FROM resolution_subjects WHERE resolution_id = ? AND person_id = ?`, r.ID, a.PersonID); err != nil {
		return f, err
	}
	var treat string
	switch err := q.QueryRowContext(ctx, `SELECT treatment FROM conflicts WHERE resolution_id = ? AND person_id = ?`, r.ID, a.PersonID).Scan(&treat); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return f, err
	default:
		f.Conflicted = true
		f.Recused = treat == "recuse"
	}
	return f, nil
}

func (s *Service) seeResolution(ctx context.Context, a Actor, id string) (*resRow, Fact, error) {
	r, err := s.loadResolutionRow(ctx, s.db, id)
	if err != nil {
		return nil, Fact{}, err
	}
	f, err := s.resFact(ctx, s.db, a, r)
	if err != nil {
		return nil, f, err
	}
	if err := s.hide(ctx, a, ActResolutionRead, f, "resolution:"+id); err != nil {
		return nil, f, err
	}
	return r, f, nil
}

func contentHash(id string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(id))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- types ---------------------------------------------------------------------------------------

// ResolutionInput drafts a resolution. A `meeting` resolution is voted in a meeting by roll call (or
// secret ballot); a `consent` resolution is passed by written consent: every eligible member signs
// before the deadline.
type ResolutionInput struct {
	MeetingID       string   `json:"meeting_id"`
	Kind            string   `json:"kind"`
	SecretBallot    bool     `json:"secret_ballot"`
	Title           string   `json:"title"`
	Text            string   `json:"text"`
	Subjects        []string `json:"subjects"` // person ids the resolution is about: they are recused automatically
	ConsentDeadline string   `json:"consent_deadline"`
	PassNum         int      `json:"pass_num"`
	PassDen         int      `json:"pass_den"`
	PassInclusive   *bool    `json:"pass_inclusive"`
	SupersedesID    string   `json:"supersedes_id"`
}

// AmendInput creates a new version of a draft or proposed resolution.
type AmendInput struct {
	Title     string `json:"title"`
	Text      string `json:"text"`
	MeetingID string `json:"meeting_id"`
}

// Tally is the final count of a decided resolution.
type Tally struct {
	For     int    `json:"for"`
	Against int    `json:"against"`
	Abstain int    `json:"abstain"`
	Reason  string `json:"reason"`
}

// Conflict is a declared interest.
type Conflict struct {
	ID        string `json:"id"`
	PersonID  string `json:"person_id"`
	Treatment string `json:"treatment"`
	Nature    string `json:"nature,omitempty"`
}

// Resolution is a resolution as the caller may see it: the title and text only when the field policy
// allows it, and the tally only once voting has closed.
type Resolution struct {
	ID              string     `json:"id"`
	BodyID          string     `json:"body_id"`
	MeetingID       string     `json:"meeting_id,omitempty"`
	Kind            string     `json:"kind"`
	Status          string     `json:"status"`
	SecretBallot    bool       `json:"secret_ballot"`
	Version         int        `json:"version"`
	Title           string     `json:"title,omitempty"`
	Text            string     `json:"text,omitempty"`
	TextWithheld    bool       `json:"text_withheld,omitempty"`
	WithdrawReason  string     `json:"withdraw_reason,omitempty"`
	PassNum         int        `json:"pass_num"`
	PassDen         int        `json:"pass_den"`
	PassInclusive   bool       `json:"pass_inclusive"`
	ConsentDeadline string     `json:"consent_deadline,omitempty"`
	ExternalType    string     `json:"external_type,omitempty"`
	ExternalRef     string     `json:"external_ref,omitempty"`
	SupersedesID    string     `json:"supersedes_id,omitempty"`
	Eligible        int        `json:"eligible,omitempty"`
	QuorumRequired  int        `json:"quorum_required,omitempty"`
	QuorumPresent   int        `json:"quorum_present,omitempty"`
	Result          *Tally     `json:"result,omitempty"`
	ClosedOn        string     `json:"closed_on,omitempty"`
	Voted           bool       `json:"voted,omitempty"`
	Conflicts       []Conflict `json:"conflicts,omitempty"`
}

func (s *Service) viewResolution(ctx context.Context, a Actor, r *resRow, f Fact, withText bool) (*Resolution, []string, error) {
	v := &Resolution{ID: r.ID, BodyID: r.BodyID, MeetingID: r.MeetingID, Kind: r.Kind, Status: r.Status, SecretBallot: r.Secret, Version: r.Version,
		PassNum: r.PassNum, PassDen: r.PassDen, PassInclusive: r.PassInclusive, ConsentDeadline: r.ConsentDeadline, ExternalType: r.ExtType, ExternalRef: r.ExtRef,
		SupersedesID: r.Supers, ClosedOn: r.ClosedOn}
	if r.Status == ResVoting || r.final() {
		v.Eligible, v.QuorumRequired, v.QuorumPresent = r.Eligible, r.QReq, r.QPresent
	}
	if r.Status == ResPassed || r.Status == ResFailed || (r.Status == ResWithdrawn && r.For+r.Against+r.Abstain > 0) {
		v.Result = &Tally{For: r.For, Against: r.Against, Abstain: r.Abstain, Reason: r.CloseReason}
	}
	var fields []string
	if withText {
		if FieldAccess(a, false, FieldResolutionText, f) {
			title, text, err := s.versionText(ctx, s.db, r.ID, r.Version)
			if err != nil {
				return nil, nil, err
			}
			v.Title, v.Text = title, text
			fields = append(fields, FieldResolutionText)
			var sr sql.NullString
			switch err := s.db.QueryRowContext(ctx, `SELECT sens_reason FROM resolution_withdrawals WHERE resolution_id = ?`, r.ID).Scan(&sr); {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return nil, nil, err
			default:
				if v.WithdrawReason, err = s.open(r.ID, "reason", sr); err != nil {
					return nil, nil, err
				}
			}
		} else {
			v.TextWithheld = true
		}
	}
	if a.PersonID != "" {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM votes WHERE resolution_id = ? AND voter_person = ?) + (SELECT COUNT(*) FROM vote_receipts WHERE resolution_id = ? AND voter_key = ?) + (SELECT COUNT(*) FROM receipt_staging WHERE resolution_id = ? AND voter_key = ?)`,
			r.ID, a.PersonID, r.ID, voterKey(r.ID, a.PersonID), r.ID, voterKey(r.ID, a.PersonID)).Scan(&n); err != nil {
			return nil, nil, err
		}
		v.Voted = n > 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, person_id, treatment, sens_nature FROM conflicts WHERE resolution_id = ? ORDER BY declared_at, id`, r.ID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var c Conflict
		var nat sql.NullString
		if err := rows.Scan(&c.ID, &c.PersonID, &c.Treatment, &nat); err != nil {
			return nil, nil, err
		}
		cf := f
		cf.Self = a.PersonID != "" && c.PersonID == a.PersonID
		if withText && nat.Valid && FieldAccess(a, false, FieldConflictNature, cf) {
			if c.Nature, err = s.open(c.ID, "nature", nat); err != nil {
				return nil, nil, err
			}
			fields = append(fields, FieldConflictNature)
		}
		v.Conflicts = append(v.Conflicts, c)
	}
	return v, fields, rows.Err()
}

// versionText opens a version's title and text and verifies its integrity hash.
func (s *Service) versionText(ctx context.Context, q querier, id string, version int) (string, string, error) {
	var st, sx sql.NullString
	var sum string
	if err := q.QueryRowContext(ctx, `SELECT sens_title, sens_text, content_sha256 FROM resolution_versions WHERE resolution_id = ? AND version = ?`, id, version).Scan(&st, &sx, &sum); err != nil {
		return "", "", err
	}
	vid := fmt.Sprintf("%s#%d", id, version)
	title, err := s.open(vid, "title", st)
	if err != nil {
		return "", "", err
	}
	text, err := s.open(vid, "text", sx)
	if err != nil {
		return "", "", err
	}
	if contentHash(vid, title, text) != sum {
		return "", "", errors.New("resolution text failed its integrity check")
	}
	return title, text, nil
}

func voterKey(resID, person string) string {
	h := sha256.Sum256([]byte("receipt|" + resID + "|" + person))
	return hex.EncodeToString(h[:])
}

func (s *Service) insertVersion(ctx context.Context, tx *sqlTx, resID string, version int, title, text, by string) error {
	vid := fmt.Sprintf("%s#%d", resID, version)
	st, err := s.seal(vid, "title", title)
	if err != nil {
		return err
	}
	sx, err := s.seal(vid, "text", text)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO resolution_versions (resolution_id, version, sens_title, sens_text, content_sha256, created_by, created_at) VALUES (?,?,?,?,?,?,?)`,
		resID, version, st, sx, contentHash(vid, title, text), by, s.now().Unix())
	return err
}

func validResText(title, text string) error {
	if err := validLine("title", title, 200, true); err != nil {
		return err
	}
	return validText("text", text, 20000, true)
}

// ---- create, amend, propose --------------------------------------------------------------------

// CreateResolution drafts a resolution in a body. A director who is a member, or the secretary. The
// drafter is the proposer: they cannot be the sole approver of what they proposed. Subjects are
// recused automatically.
func (s *Service) CreateResolution(ctx context.Context, a Actor, bodyID string, in ResolutionInput, idemKey string, body []byte) (string, bool, error) {
	b, err := s.loadBody(ctx, s.db, bodyID)
	if err != nil {
		return "", false, err
	}
	bf, err := s.bodyFact(ctx, s.db, a, bodyID)
	if err != nil {
		return "", false, err
	}
	if err := s.hide(ctx, a, ActBodyRead, bf, "body:"+bodyID); err != nil {
		return "", false, err
	}
	if err := s.authorize(ctx, a, ActResolutionDraft, bf, "body:"+bodyID); err != nil {
		return "", false, err
	}
	return s.createResolution(ctx, a, b, in, canonSubject(a.Subject), a.PersonID, "", "", idemKey, body)
}

func (s *Service) createResolution(ctx context.Context, a Actor, b *Body, in ResolutionInput, proposerRef, proposerPerson, extType, extRef, idemKey string, body []byte) (string, bool, error) {
	if in.Kind == "" {
		in.Kind = "meeting"
	}
	if in.Kind != "meeting" && in.Kind != "consent" {
		return "", false, invalid("kind must be meeting or consent")
	}
	if err := validResText(in.Title, in.Text); err != nil {
		return "", false, err
	}
	if in.Kind == "consent" {
		if in.SecretBallot {
			return "", false, invalid("written consent is signed: it cannot be a secret ballot")
		}
		if _, err := parseDate(in.ConsentDeadline); err != nil || in.ConsentDeadline <= s.today() {
			return "", false, invalid("consent_deadline must be a date after today")
		}
	} else {
		in.ConsentDeadline = ""
	}
	if in.PassDen == 0 {
		in.PassNum, in.PassDen = b.PassNum, b.PassDen
		in.PassInclusive = &b.PassInclusive
	}
	if err := validFraction("pass threshold", in.PassNum, in.PassDen); err != nil {
		return "", false, err
	}
	inc := b.PassInclusive
	if in.PassInclusive != nil {
		inc = *in.PassInclusive
	}
	if len(in.Subjects) > 20 {
		return "", false, invalid("at most 20 subjects")
	}
	for _, p := range in.Subjects {
		if err := validPerson("subject", p); err != nil {
			return "", false, err
		}
	}
	if in.MeetingID != "" {
		m, err := s.loadMeeting(ctx, s.db, in.MeetingID)
		if err != nil || m.BodyID != b.ID {
			return "", false, invalid("meeting_id is not a meeting of this body")
		}
	}
	if in.Kind == "consent" && in.MeetingID != "" {
		return "", false, invalid("a written consent is not tied to a meeting")
	}
	if in.SupersedesID != "" {
		old, err := s.loadResolutionRow(ctx, s.db, in.SupersedesID)
		if err != nil || old.BodyID != b.ID {
			return "", false, invalid("supersedes_id is not a resolution of this body")
		}
	}
	return s.idempotent(ctx, a, "POST /v1/bodies/"+b.ID+"/resolutions", idemKey, body, func(ctx context.Context) (string, error) {
		id := newID("res")
		now := s.now().Unix()
		err := s.txAudit(ctx, a, "write", "resolution:"+id, []string{FieldResolutionText}, func(ctx context.Context, tx *sqlTx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO resolutions (id, body_id, meeting_id, kind, secret_ballot, status, current_version, proposer_ref, proposer_person, pass_num, pass_den, pass_inclusive, consent_deadline, external_type, external_ref, supersedes_id, created_by, created_at, updated_at)
				VALUES (?,?,?,?,?,'draft',1,?,?,?,?,?,?,?,?,?,?,?,?)`,
				id, b.ID, nullStr(in.MeetingID), in.Kind, b2i(in.SecretBallot), proposerRef, proposerPerson, in.PassNum, in.PassDen, b2i(inc), in.ConsentDeadline, extType, extRef, nullStr(in.SupersedesID), canonSubject(a.Subject), now, now); err != nil {
				return err
			}
			for _, p := range in.Subjects {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO resolution_subjects (resolution_id, person_id) VALUES (?,?)`, id, p); err != nil {
					return err
				}
			}
			if err := s.insertVersion(ctx, tx, id, 1, in.Title, in.Text, canonSubject(a.Subject)); err != nil {
				return err
			}
			setIdem(ctx, id)
			return nil
		})
		return id, err
	})
}

// GetResolution returns a resolution the caller can see. Title and text are returned only to those the
// field policy allows (and that read is audited before it is returned); the tally only once closed.
func (s *Service) GetResolution(ctx context.Context, a Actor, id string) (*Resolution, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	r, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	v, fields, err := s.viewResolution(ctx, a, r, f, true)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "resolution:"+id, fields); err != nil {
		return nil, err
	}
	return v, nil
}

// ListResolutions lists the resolutions of a body the caller can see (no text).
func (s *Service) ListResolutions(ctx context.Context, a Actor, bodyID string) ([]*Resolution, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if _, err := s.loadBody(ctx, s.db, bodyID); err != nil {
		return nil, err
	}
	bf, err := s.bodyFact(ctx, s.db, a, bodyID)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActBodyRead, bf, "body:"+bodyID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+resCols+` FROM resolutions WHERE body_id = ? ORDER BY created_at DESC, id LIMIT 1000`, bodyID)
	if err != nil {
		return nil, err
	}
	var all []*resRow
	for rows.Next() {
		r, err := scanRes(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	_ = rows.Close()
	out := []*Resolution{}
	var res []string
	for _, r := range all {
		f, err := s.resFact(ctx, s.db, a, r)
		if err != nil {
			return nil, err
		}
		if !Decide(a, ActResolutionRead, f) {
			continue
		}
		v, _, err := s.viewResolution(ctx, a, r, f, false)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		res = append(res, "resolution:"+r.ID)
	}
	if err := s.auditReadMany(ctx, a, "read", res, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// AmendResolution creates a new version of the title and text (and may attach the meeting). The old
// versions stay. Only the proposer, and only before voting opens.
func (s *Service) AmendResolution(ctx context.Context, a Actor, id string, in AmendInput) (*Resolution, error) {
	r, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActResolutionEdit, f, "resolution:"+id); err != nil {
		return nil, err
	}
	if err := validResText(in.Title, in.Text); err != nil {
		return nil, err
	}
	if r.Status != ResDraft && r.Status != ResProposed {
		return nil, conflict("a %s resolution cannot be amended: withdraw it and draft a new one", r.Status)
	}
	if in.MeetingID != "" {
		m, err := s.loadMeeting(ctx, s.db, in.MeetingID)
		if err != nil || m.BodyID != r.BodyID || r.Kind == "consent" {
			return nil, invalid("meeting_id is not a meeting of this body, or the resolution is a written consent")
		}
	}
	next := r.Version + 1
	err = s.txAudit(ctx, a, "write", "resolution:"+id, []string{FieldResolutionText}, func(ctx context.Context, tx *sqlTx) error {
		if err := s.insertVersion(ctx, tx, id, next, in.Title, in.Text, canonSubject(a.Subject)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE resolutions SET current_version = ?, meeting_id = COALESCE(?, meeting_id), updated_at = ? WHERE id = ? AND current_version = ? AND status IN ('draft','proposed')`,
			next, nullStr(in.MeetingID), s.now().Unix(), id, r.Version)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return conflict("the resolution changed: reload it")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetResolution(ctx, a, id)
}

// ProposeResolution moves a draft to proposed. The proposer.
func (s *Service) ProposeResolution(ctx context.Context, a Actor, id string) (*Resolution, error) {
	r, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActResolutionEdit, f, "resolution:"+id); err != nil {
		return nil, err
	}
	if r.Status != ResDraft {
		return nil, conflict("the resolution is %s", r.Status)
	}
	err = s.txAudit(ctx, a, "write", "resolution:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE resolutions SET status = 'proposed', updated_at = ? WHERE id = ? AND status = 'draft'`, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetResolution(ctx, a, id)
}

// ConflictInput declares an interest. A director declares their own; the secretary can record one for
// a director. Treatment: recuse (cannot vote, excluded from the quorum when the body says so), abstain
// (must abstain) or disclosed (may vote, the interest is on record).
type ConflictInput struct {
	PersonID  string `json:"person_id"`
	Treatment string `json:"treatment"`
	Nature    string `json:"nature"`
}

// DeclareConflict records a declared interest before voting opens.
func (s *Service) DeclareConflict(ctx context.Context, a Actor, resID string, in ConflictInput) (*Conflict, error) {
	r, f, err := s.seeResolution(ctx, a, resID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActConflictDeclare, f, "resolution:"+resID); err != nil {
		return nil, err
	}
	if in.PersonID == "" {
		in.PersonID = a.PersonID
	}
	if err := validPerson("person_id", in.PersonID); err != nil {
		return nil, err
	}
	if !a.has(RoleSecretary) && in.PersonID != a.PersonID {
		s.auditDeny(ctx, a, "conflict.declare", "resolution:"+resID, nil)
		return nil, ErrForbidden
	}
	if in.Treatment != "recuse" && in.Treatment != "abstain" && in.Treatment != "disclosed" {
		return nil, invalid("treatment must be recuse, abstain or disclosed")
	}
	if err := validText("nature", in.Nature, 1000, false); err != nil {
		return nil, err
	}
	if r.Status != ResDraft && r.Status != ResProposed {
		return nil, conflict("interests are declared before voting opens: withdraw the resolution and propose it again")
	}
	role, err := s.memberRole(ctx, s.db, r.BodyID, in.PersonID, s.today())
	if err != nil {
		return nil, err
	}
	if role != "chair" && role != "member" {
		return nil, invalid("the person is not a voting member of the body")
	}
	id := newID("cfl")
	nat, err := s.sealNull(id, "nature", in.Nature)
	if err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "write", "resolution:"+resID+"/conflicts", []string{FieldConflictNature}, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO conflicts (id, resolution_id, person_id, treatment, sens_nature, declared_by, declared_at) VALUES (?,?,?,?,?,?,?)`,
			id, resID, in.PersonID, in.Treatment, nat, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("an interest is already declared by this person on this resolution")
		}
		return nil, err
	}
	return &Conflict{ID: id, PersonID: in.PersonID, Treatment: in.Treatment}, nil
}

// ---- voting -------------------------------------------------------------------------------------

// OpenVote opens voting on a proposed resolution: it fixes the list of people who may vote (members in
// term, minus the subjects and the recused), checks quorum for a meeting resolution (exact integers),
// and starts the clock. The chair or the secretary.
func (s *Service) OpenVote(ctx context.Context, a Actor, id string) (*Resolution, error) {
	r, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActVoteOpen, f, "resolution:"+id); err != nil {
		return nil, err
	}
	if r.Status != ResProposed {
		return nil, conflict("the resolution is %s: only a proposed resolution can be opened for voting", r.Status)
	}
	b, err := s.loadBody(ctx, s.db, r.BodyID)
	if err != nil {
		return nil, err
	}
	day := s.today()
	excluded := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT person_id FROM resolution_subjects WHERE resolution_id = ? UNION SELECT person_id FROM conflicts WHERE resolution_id = ? AND treatment = 'recuse'`, id, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return nil, err
		}
		excluded[p] = true
	}
	_ = rows.Close()
	members, err := s.votingMembers(ctx, s.db, r.BodyID, day)
	if err != nil {
		return nil, err
	}
	var voters []string
	for _, p := range members {
		if !excluded[p] {
			voters = append(voters, p)
		}
	}
	if len(voters) == 0 {
		return nil, conflict("nobody is eligible to vote: every member is recused or a subject")
	}
	qreq, qpres := 0, 0
	if r.Kind == "meeting" {
		if r.MeetingID == "" {
			return nil, conflict("attach the resolution to a meeting before opening the vote")
		}
		m, err := s.loadMeeting(ctx, s.db, r.MeetingID)
		if err != nil {
			return nil, err
		}
		if m.Status == "cancelled" || m.StartsOn > day {
			return nil, conflict("the meeting is cancelled or has not started")
		}
		q, err := s.quorumFor(ctx, s.db, b, &Meeting{ID: m.ID, BodyID: m.BodyID, StartsOn: day}, excluded)
		if err != nil {
			return nil, err
		}
		qreq, qpres = q.Required, q.Present
		if !q.Met {
			return nil, conflict("there is no quorum: %d present, %d required", q.Present, q.Required)
		}
	}
	err = s.txAudit(ctx, a, "write", "resolution:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		for _, p := range voters {
			if _, err := tx.ExecContext(ctx, `INSERT INTO resolution_voters (resolution_id, person_id) VALUES (?,?)`, id, p); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE resolutions SET status = 'voting', eligible_count = ?, quorum_required = ?, quorum_present = ?, opened_by = ?, opened_at = ?, updated_at = ? WHERE id = ? AND status = 'proposed'`,
			len(voters), qreq, qpres, canonSubject(a.Subject), s.now().Unix(), s.now().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return conflict("the resolution changed: reload it")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetResolution(ctx, a, id)
}

// VoteInput casts a vote or signs a written consent.
type VoteInput struct {
	Choice       string `json:"choice"`        // for | against | abstain (consent: for or against)
	SignatureRef string `json:"signature_ref"` // consent: opaque reference of the signature record
}

// CastVote records one vote, signature or secret ballot from a person on the voter list. A person
// votes once; votes never change. Recused people and the subjects of the resolution are not on the list.
func (s *Service) CastVote(ctx context.Context, a Actor, id string, in VoteInput) (*Resolution, error) {
	r, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActVoteCast, f, "resolution:"+id); err != nil {
		return nil, err
	}
	if in.Choice != "for" && in.Choice != "against" && in.Choice != "abstain" {
		return nil, invalid("choice must be for, against or abstain")
	}
	if r.Kind == "consent" {
		if in.Choice == "abstain" {
			return nil, invalid("a written consent is signed or declined: it cannot abstain")
		}
		if err := validRef("signature_ref", in.SignatureRef, true); err != nil {
			return nil, err
		}
	} else {
		in.SignatureRef = ""
	}
	if r.Status != ResVoting {
		return nil, conflict("voting is not open (the resolution is %s)", r.Status)
	}
	onList, err := s.exists(ctx, s.db, `SELECT 1 FROM resolution_voters WHERE resolution_id = ? AND person_id = ?`, id, a.PersonID)
	if err != nil {
		return nil, err
	}
	if !onList {
		s.auditDeny(ctx, a, "vote", "resolution:"+id, nil)
		return nil, ErrForbidden
	}
	if f.Conflicted && !f.Recused {
		var treat string
		if err := s.db.QueryRowContext(ctx, `SELECT treatment FROM conflicts WHERE resolution_id = ? AND person_id = ?`, id, a.PersonID).Scan(&treat); err != nil {
			return nil, err
		}
		if treat == "abstain" && in.Choice != "abstain" {
			return nil, conflict("you declared an interest with treatment abstain: you may only abstain")
		}
	}
	if r.Kind == "meeting" {
		b, err := s.loadBody(ctx, s.db, r.BodyID)
		if err != nil {
			return nil, err
		}
		m, err := s.loadMeeting(ctx, s.db, r.MeetingID)
		if err != nil {
			return nil, err
		}
		ok, err := s.exists(ctx, s.db, `SELECT 1 FROM attendance WHERE meeting_id = ? AND person_id = ? AND status IN ('present','remote')`, m.ID, a.PersonID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, conflict("you are not recorded as present at the meeting")
		}
		excl, err := s.excludedFor(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		q, err := s.quorumFor(ctx, s.db, b, &Meeting{ID: m.ID, BodyID: m.BodyID, StartsOn: s.today()}, excl)
		if err != nil {
			return nil, err
		}
		if !q.Met {
			return nil, conflict("quorum is lost: %d present, %d required", q.Present, q.Required)
		}
	}
	castFn := func(ctx context.Context, tx *sqlTx) error {
		if r.Secret {
			// Staged: no timestamp, no audit row per vote. Written to ballots and receipts, shuffled, at close.
			if _, err := tx.ExecContext(ctx, `INSERT INTO receipt_staging (resolution_id, voter_key, person_id) VALUES (?,?,?)`, id, voterKey(id, a.PersonID), a.PersonID); err != nil {
				return err
			}
			var rb [12]byte
			_, _ = rand.Read(rb[:])
			bid := "bal_" + hex.EncodeToString(rb[:])
			sc, err := s.seal(bid, "choice", in.Choice)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO ballot_staging (id, resolution_id, sens_choice) VALUES (?,?,?)`, bid, id, sc); err != nil {
				return err
			}
		} else {
			method := "roll_call"
			if r.Kind == "consent" {
				method = "consent"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO votes (id, resolution_id, voter_person, voter_ref, choice, method, signature_ref, cast_at) VALUES (?,?,?,?,?,?,?,?)`,
				newID("vot"), id, a.PersonID, canonSubject(a.Subject), in.Choice, method, in.SignatureRef, s.now().Unix()); err != nil {
				return err
			}
		}
		if r.Kind == "consent" {
			return s.settleConsent(ctx, tx, r, a)
		}
		return nil
	}
	if r.Secret {
		err = s.tx(ctx, castFn)
	} else {
		err = s.txAudit(ctx, a, "vote", "resolution:"+id, nil, castFn)
	}
	if err != nil {
		if isUnique(err) {
			return nil, conflict("you have already voted on this resolution")
		}
		return nil, err
	}
	return s.GetResolution(ctx, a, id)
}

func (s *Service) excludedFor(ctx context.Context, q querier, id string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT person_id FROM resolution_subjects WHERE resolution_id = ? UNION SELECT person_id FROM conflicts WHERE resolution_id = ? AND treatment = 'recuse'`, id, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// randInt is a uniform integer in [0, n) from crypto/rand.
func randInt(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// shuffle is a Fisher-Yates shuffle driven by crypto/rand.
func shuffle[T any](xs []T) error {
	for i := len(xs) - 1; i > 0; i-- {
		j, err := randInt(i + 1)
		if err != nil {
			return err
		}
		xs[i], xs[j] = xs[j], xs[i]
	}
	return nil
}

// finaliseBallots moves the staged secret ballots and receipts of a resolution into the final tables in a
// shuffled order, writes one "voted" audit entry per voter (sorted by person id, at close time: no per-vote
// time or sequence exists), and deletes the staging rows. The caller vacuums after the transaction commits.
func (s *Service) finaliseBallots(ctx context.Context, tx *sqlTx, r *resRow) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, sens_choice FROM ballot_staging WHERE resolution_id = ?`, r.ID)
	if err != nil {
		return err
	}
	type ballot struct{ id, sealed string }
	var bs []ballot
	for rows.Next() {
		var b ballot
		if err := rows.Scan(&b.id, &b.sealed); err != nil {
			_ = rows.Close()
			return err
		}
		bs = append(bs, b)
	}
	_ = rows.Close()
	if err := shuffle(bs); err != nil {
		return err
	}
	for _, b := range bs {
		plain, err := s.keys.Open(b.sealed, b.id+":choice")
		if err != nil {
			return err
		}
		var rb [12]byte
		if _, err := rand.Read(rb[:]); err != nil {
			return err
		}
		nid := "bal_" + hex.EncodeToString(rb[:])
		sc, err := s.seal(nid, "choice", plain)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ballots (id, resolution_id, sens_choice) VALUES (?,?,?)`, nid, r.ID, sc); err != nil {
			return err
		}
	}
	rrows, err := tx.QueryContext(ctx, `SELECT voter_key, person_id FROM receipt_staging WHERE resolution_id = ? ORDER BY person_id`, r.ID)
	if err != nil {
		return err
	}
	type receipt struct{ key, person string }
	var rs []receipt
	for rrows.Next() {
		var x receipt
		if err := rrows.Scan(&x.key, &x.person); err != nil {
			_ = rrows.Close()
			return err
		}
		rs = append(rs, x)
	}
	_ = rrows.Close()
	for _, x := range rs { // sorted by person id for the audit entries
		if err := s.auditIn(ctx, tx, Actor{PersonID: x.person}, "voted", "resolution:"+r.ID, nil); err != nil {
			return err
		}
	}
	if err := shuffle(rs); err != nil {
		return err
	}
	for _, x := range rs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO vote_receipts (resolution_id, voter_key) VALUES (?,?)`, r.ID, x.key); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ballot_staging WHERE resolution_id = ?`, r.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM receipt_staging WHERE resolution_id = ?`, r.ID)
	return err
}

// counts reads the tally of an open-vote resolution. Secret ballots are opened one by one here.
func (s *Service) counts(ctx context.Context, q querier, r *resRow) (forN, against, abstain int, forByOthers int, err error) {
	if r.Secret {
		rows, err := q.QueryContext(ctx, `SELECT id, sens_choice FROM ballots WHERE resolution_id = ?`, r.ID)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var bid string
			var sc sql.NullString
			if err := rows.Scan(&bid, &sc); err != nil {
				return 0, 0, 0, 0, err
			}
			c, err := s.open(bid, "choice", sc)
			if err != nil {
				return 0, 0, 0, 0, err
			}
			switch c {
			case "for":
				forN++
			case "against":
				against++
			case "abstain":
				abstain++
			}
		}
		// Who voted for is unknowable: at least two "for" ballots are needed so that the proposer cannot be the only one.
		if forN >= 2 {
			forByOthers = forN - 1
		}
		return forN, against, abstain, forByOthers, rows.Err()
	}
	rows, err := q.QueryContext(ctx, `SELECT voter_person, voter_ref, choice FROM votes WHERE resolution_id = ?`, r.ID)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var person, ref, choice string
		if err := rows.Scan(&person, &ref, &choice); err != nil {
			return 0, 0, 0, 0, err
		}
		switch choice {
		case "for":
			forN++
			// the proposer is never the sole approver: one function decides who counts as the same person
			if sodOK(Actor{Subject: ref, PersonID: person}, r.ProposerRef, r.ProposerPerson) {
				forByOthers++
			}
		case "against":
			against++
		case "abstain":
			abstain++
		}
	}
	return forN, against, abstain, forByOthers, rows.Err()
}

// settleConsent closes a written consent when it is decided: any decline fails it; when every eligible
// member has signed it passes (unless the proposer alone signed).
func (s *Service) settleConsent(ctx context.Context, tx *sqlTx, r *resRow, a Actor) error {
	forN, against, abstain, others, err := s.counts(ctx, tx, r)
	if err != nil {
		return err
	}
	switch {
	case against > 0:
		return s.closeRes(ctx, tx, r, a, ResFailed, "declined", forN, against, abstain)
	case forN >= r.Eligible && others > 0:
		return s.closeRes(ctx, tx, r, a, ResPassed, "all_signed", forN, against, abstain)
	case forN >= r.Eligible:
		return s.closeRes(ctx, tx, r, a, ResFailed, "proposer_sole_approver", forN, against, abstain)
	}
	return nil
}

// closeRes writes the final state of a resolution in the caller's transaction and, for a pass, the
// event (ids and the result only).
func (s *Service) closeRes(ctx context.Context, tx *sqlTx, r *resRow, a Actor, status, reason string, forN, against, abstain int) error {
	by := canonSubject(a.Subject)
	if a.Subject == "" {
		by = "svc:sweep"
	}
	res, err := tx.ExecContext(ctx, `UPDATE resolutions SET status = ?, close_reason = ?, result_for = ?, result_against = ?, result_abstain = ?, closed_by = ?, closed_on = ?, updated_at = ? WHERE id = ? AND status = 'voting'`,
		status, reason, forN, against, abstain, by, s.today(), s.now().Unix(), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return conflict("the resolution is no longer open for voting")
	}
	if status == ResPassed {
		data := map[string]any{"resolution_id": r.ID, "body_id": r.BodyID, "result": "passed", "passed_on": s.today()}
		if r.MeetingID != "" {
			data["meeting_id"] = r.MeetingID
		}
		if r.ExtType != "" {
			data["external_type"], data["external_ref"] = r.ExtType, r.ExtRef
		}
		return s.enqueue(ctx, tx, EventResolutionPassed, data)
	}
	return nil
}

// CloseVote ends voting on a meeting resolution and records the tally, which is final. The chair or the
// secretary. A written consent closes by itself when everyone has answered; after its deadline this
// closes it as failed. Quorum is checked again; the proposer alone cannot carry a resolution.
func (s *Service) CloseVote(ctx context.Context, a Actor, id string) (*Resolution, error) {
	r, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActVoteClose, f, "resolution:"+id); err != nil {
		return nil, err
	}
	if r.Status != ResVoting {
		return nil, conflict("voting is not open (the resolution is %s)", r.Status)
	}
	err = s.txAudit(ctx, a, "close", "resolution:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		if r.Secret {
			if err := s.finaliseBallots(ctx, tx, r); err != nil {
				return err
			}
		}
		forN, against, abstain, others, err := s.counts(ctx, tx, r)
		if err != nil {
			return err
		}
		if r.Kind == "consent" {
			if r.ConsentDeadline >= s.today() {
				return conflict("the consent is open until %s unless everyone answers sooner", r.ConsentDeadline)
			}
			return s.closeRes(ctx, tx, r, a, ResFailed, "deadline", forN, against, abstain)
		}
		b, err := s.loadBody(ctx, tx, r.BodyID)
		if err != nil {
			return err
		}
		excl, err := s.excludedFor(ctx, tx, id)
		if err != nil {
			return err
		}
		q, err := s.quorumFor(ctx, tx, b, &Meeting{ID: r.MeetingID, BodyID: r.BodyID, StartsOn: s.today()}, excl)
		if err != nil {
			return err
		}
		switch {
		case !q.Met:
			return s.closeRes(ctx, tx, r, a, ResFailed, "no_quorum", forN, against, abstain)
		case others == 0 && forN > 0 && TallyPasses(forN, against, abstain, r.PassNum, r.PassDen, r.PassInclusive):
			return s.closeRes(ctx, tx, r, a, ResFailed, "proposer_sole_approver", forN, against, abstain)
		case TallyPasses(forN, against, abstain, r.PassNum, r.PassDen, r.PassInclusive):
			return s.closeRes(ctx, tx, r, a, ResPassed, "tally", forN, against, abstain)
		}
		return s.closeRes(ctx, tx, r, a, ResFailed, "tally", forN, against, abstain)
	})
	if err != nil {
		return nil, err
	}
	if r.Secret { // the staging order must not survive in free pages or the write-ahead log
		if err := s.scrub(ctx, true); err != nil {
			return nil, err
		}
	}
	return s.GetResolution(ctx, a, id)
}

// WithdrawInput carries the reason for withdrawing a resolution that is already being voted on.
type WithdrawInput struct {
	Reason string `json:"reason"`
}

// WithdrawResolution withdraws a resolution that is not decided. The proposer or the secretary. A
// resolution withdrawn before voting opens records no tally. One withdrawn while voting is open needs a
// reason (sealed, audited) and keeps the votes cast so far: they are finalised, the tally is recorded and
// stays readable by those who may read votes.
func (s *Service) WithdrawResolution(ctx context.Context, a Actor, id, reason string) (*Resolution, error) {
	r, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActResolutionWithdraw, f, "resolution:"+id); err != nil {
		return nil, err
	}
	if r.final() {
		return nil, conflict("the resolution is already %s", r.Status)
	}
	if err := validText("reason", reason, 1000, r.Status == ResVoting); err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "withdraw", "resolution:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		forN, against, abstain := 0, 0, 0
		if r.Status == ResVoting {
			if r.Secret {
				if err := s.finaliseBallots(ctx, tx, r); err != nil {
					return err
				}
			}
			if forN, against, abstain, _, err = s.counts(ctx, tx, r); err != nil {
				return err
			}
			sealed, err := s.seal(id, "reason", reason)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO resolution_withdrawals (resolution_id, sens_reason, withdrawn_by, withdrawn_at) VALUES (?,?,?,?)`, id, sealed, canonSubject(a.Subject), s.now().Unix()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE resolutions SET status = 'withdrawn', close_reason = 'withdrawn', result_for = ?, result_against = ?, result_abstain = ?, closed_by = ?, closed_on = ?, updated_at = ? WHERE id = ? AND status IN ('draft','proposed','voting')`,
			forN, against, abstain, canonSubject(a.Subject), s.today(), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if r.Secret && r.Status == ResVoting {
		if err := s.scrub(ctx, true); err != nil {
			return nil, err
		}
	}
	return s.GetResolution(ctx, a, id)
}

// VoteRecord is one open vote or signature.
type VoteRecord struct {
	PersonID     string `json:"person_id"`
	Choice       string `json:"choice"`
	Method       string `json:"method"`
	SignatureRef string `json:"signature_ref,omitempty"`
	CastOn       string `json:"cast_on"`
}

// ListVotes returns who voted how, once voting has closed, to those the field policy allows. A secret
// ballot has no such list for anyone.
func (s *Service) ListVotes(ctx context.Context, a Actor, id string) ([]VoteRecord, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	_, f, err := s.seeResolution(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if !FieldAccess(a, false, FieldVoteDetail, f) {
		s.auditDeny(ctx, a, "read", "resolution:"+id+"/votes", []string{FieldVoteDetail})
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT voter_person, choice, method, signature_ref, cast_at FROM votes WHERE resolution_id = ? ORDER BY voter_person`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []VoteRecord{}
	var res []string
	for rows.Next() {
		var v VoteRecord
		var at int64
		if err := rows.Scan(&v.PersonID, &v.Choice, &v.Method, &v.SignatureRef, &at); err != nil {
			return nil, err
		}
		v.CastOn = time.Unix(at, 0).UTC().Format(dateLayout)
		out = append(out, v)
		res = append(res, "vote:"+id+"/"+v.PersonID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldVoteDetail}); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- integration seam for the approval engine of other modules (equity grants) -------------------

// ResolutionRequest is what another module (through can-core) sends when a decision needs a board
// resolution: the module's own type and reference, the body that decides, and who asked.
type ResolutionRequest struct {
	Type      string   `json:"type"`      // e.g. equity.grant
	Ref       string   `json:"ref"`       // opaque id in the requesting module
	BodyID    string   `json:"body_id"`   // the body that decides
	Requester string   `json:"requester"` // subject of the person who asked: cannot be the sole approver
	Title     string   `json:"title"`
	Subjects  []string `json:"subjects"` // person ids the decision is about (a grantee who is a director is recused)
}

var extTypeRe = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*$`)

// RequestResolution creates (or returns the existing) draft resolution for an external request. The
// secretary completes, proposes and runs it like any other. integration only.
func (s *Service) RequestResolution(ctx context.Context, a Actor, in ResolutionRequest) (*ResolutionOutcome, error) {
	if err := s.authorize(ctx, a, ActResolutionRequest, Fact{}, "resolution-requests"); err != nil {
		return nil, err
	}
	if !extTypeRe.MatchString(in.Type) || len(in.Type) > 64 {
		return nil, invalid("type must look like equity.grant")
	}
	if err := validRef("ref", in.Ref, true); err != nil {
		return nil, err
	}
	if !subjectRe.MatchString(in.Requester) {
		return nil, invalid("requester must be the subject of the person who asked")
	}
	if in.Title == "" {
		in.Title = "Decision requested: " + in.Type
	}
	b, err := s.loadBody(ctx, s.db, in.BodyID)
	if err != nil {
		return nil, invalid("body_id is not a body")
	}
	if ex, err := s.outcomeBy(ctx, a, in.Type, in.Ref); err == nil && ex.Outcome != "cancelled" {
		return ex, nil
	}
	text := fmt.Sprintf("To be completed by the company secretary. Requested by another module: %s %s.", in.Type, in.Ref)
	if _, _, err := s.createResolution(ctx, a, b, ResolutionInput{Kind: "meeting", Title: in.Title, Text: text, Subjects: in.Subjects}, canonSubject(in.Requester), "", in.Type, in.Ref, "", nil); err != nil {
		if isUnique(err) { // a concurrent identical request created it first: return that one
			return s.outcomeBy(ctx, a, in.Type, in.Ref)
		}
		return nil, err
	}
	return s.outcomeBy(ctx, a, in.Type, in.Ref)
}

// ResolutionOutcome is the answer an approval engine needs: pending, approved, rejected or cancelled.
// It carries ids and a date, never text or votes.
type ResolutionOutcome struct {
	ResolutionID string `json:"resolution_id"`
	BodyID       string `json:"body_id"`
	Type         string `json:"type"`
	Ref          string `json:"ref"`
	Outcome      string `json:"outcome"` // pending | approved | rejected | cancelled
	DecidedOn    string `json:"decided_on,omitempty"`
}

func (s *Service) outcomeBy(ctx context.Context, a Actor, typ, ref string) (*ResolutionOutcome, error) {
	var id, body, status, closed string
	err := s.db.QueryRowContext(ctx, `SELECT id, body_id, status, IFNULL(closed_on,'') FROM resolutions WHERE external_type = ? AND external_ref = ? AND created_by = ? ORDER BY (status = 'withdrawn'), created_at DESC LIMIT 1`, typ, ref, canonSubject(a.Subject)).Scan(&id, &body, &status, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	o := &ResolutionOutcome{ResolutionID: id, BodyID: body, Type: typ, Ref: ref, Outcome: "pending", DecidedOn: closed}
	switch status {
	case ResPassed:
		o.Outcome = "approved"
	case ResFailed:
		o.Outcome = "rejected"
	case ResWithdrawn:
		o.Outcome = "cancelled"
	}
	return o, nil
}

// OutcomeByRef returns the outcome of the resolution requested for (type, ref). integration only. An
// unknown reference is 404.
func (s *Service) OutcomeByRef(ctx context.Context, a Actor, typ, ref string) (*ResolutionOutcome, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if err := s.authorize(ctx, a, ActResolutionOutcome, Fact{}, "resolution-outcome"); err != nil {
		return nil, err
	}
	o, err := s.outcomeBy(ctx, a, typ, ref)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "resolution:"+o.ResolutionID, nil); err != nil {
		return nil, err
	}
	return o, nil
}

// ApplyDue closes written consents past their deadline (failed, reason deadline), announces
// filings that are coming due (entity.filing.due.v1) and prunes idempotency keys. Safe to run any
// time and as often as wanted. It returns the number of changes.
func (s *Service) ApplyDue(ctx context.Context) (int, error) {
	changed := 0
	today := s.today()
	rows, err := s.db.QueryContext(ctx, `SELECT `+resCols+` FROM resolutions WHERE status = 'voting' AND kind = 'consent' AND consent_deadline < ?`, today)
	if err != nil {
		return 0, err
	}
	var due []*resRow
	for rows.Next() {
		r, err := scanRes(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		due = append(due, r)
	}
	_ = rows.Close()
	for _, r := range due {
		err := s.tx(ctx, func(ctx context.Context, tx *sqlTx) error {
			forN, against, abstain, _, err := s.counts(ctx, tx, r)
			if err != nil {
				return err
			}
			if err := s.closeRes(ctx, tx, r, Actor{}, ResFailed, "deadline", forN, against, abstain); err != nil {
				return err
			}
			return s.auditIn(ctx, tx, Actor{Subject: "svc:sweep"}, "close", "resolution:"+r.ID, nil)
		})
		if err != nil && !errors.Is(err, ErrConflict) {
			return changed, err
		}
		if err == nil {
			changed++
		}
	}
	n, err := s.applyFilingsDue(ctx)
	changed += n
	if err != nil {
		return changed, err
	}
	return changed, s.pruneIdempotency(ctx)
}

var _ = strings.TrimSpace

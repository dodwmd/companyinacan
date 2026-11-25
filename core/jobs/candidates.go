package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Candidate is a person who applied or was sourced. Sealed and contact fields are pointers:
// absent means the caller's role may not see them (RestrictedFields lists which), present and
// empty means there is no value.
type Candidate struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Email             *string    `json:"email,omitempty"`
	Phone             *string    `json:"phone,omitempty"`
	Address           *string    `json:"address,omitempty"`
	SalaryExpectation *string    `json:"salary_expectation,omitempty"`
	Notes             *string    `json:"notes,omitempty"`
	Tags              []string   `json:"tags"`
	Source            string     `json:"source"`
	TalentPool        bool       `json:"talent_pool"`
	TalentConsentAt   *time.Time `json:"talent_consent_at"`
	ConsentAt         time.Time  `json:"consent_at"`
	ConsentVersion    string     `json:"consent_version"`
	RetainUntil       *time.Time `json:"retain_until"`
	LegalHold         bool       `json:"legal_hold"`
	Erased            bool       `json:"erased"`
	// RestrictedFields names the fields left out because of the caller's role.
	RestrictedFields []string  `json:"restricted_fields,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ResumeMeta is what the database knows about an uploaded resume. The bytes live in the
// ResumeStore.
type ResumeMeta struct {
	Name   string `json:"name"`
	Type   string `json:"content_type"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Scan   string `json:"scan"` // clean, skipped (never scanned) or infected
	key    string
}

// PublicApplication is one submission from the careers page.
type PublicApplication struct {
	JobSlug     string
	Name        string
	Email       string
	Phone       string
	CoverLetter string
	Source      string
	Consent     bool
	TalentPool  bool
	Resume      *ResumeMeta
	EEO         *EEOInput
}

// ApplyResult tells the handler what happened without telling the applicant.
type ApplyResult struct {
	ApplicationID string
	Created       bool // false: this person already applied to this job
	NewCandidate  bool
}

var (
	phoneRE  = regexp.MustCompile(`^[0-9+()./ -]{5,30}$`)
	sourceRE = regexp.MustCompile(`^[a-z0-9_-]{0,40}$`)
	tagRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,29}$`)
)

func validCandidateFields(p *problems, name, email, phone, cover, source string) (string, bool) {
	p.text("name", strings.TrimSpace(name), 100, true, false)
	norm, ok := normEmail(email)
	if !ok {
		p.add("email", "a valid email address is required")
	}
	if phone != "" && !phoneRE.MatchString(phone) {
		p.add("phone", "digits, spaces and + ( ) . - / only, 5 to 30 characters")
	}
	p.text("cover_letter", cover, 5000, false, true)
	if !sourceRE.MatchString(source) {
		p.add("source", "lowercase letters, digits, - and _, at most 40 characters")
	}
	return norm, ok
}

// Apply stores a public application. It never modifies an existing candidate's profile (an
// unauthenticated form must not be able to rewrite someone else's record by guessing their
// address): a repeat applicant gets a new application on the same candidate, and a repeat
// application for the same job is dropped silently, so the response cannot be used to learn
// whether an address is known. The caller deletes an uploaded resume when Created is false.
func (s *Store) Apply(ctx context.Context, in PublicApplication) (*ApplyResult, error) {
	var p problems
	email, _ := validCandidateFields(&p, in.Name, in.Email, in.Phone, in.CoverLetter, in.Source)
	if !in.Consent {
		p.add("consent", "you must agree to the processing of your application data")
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	hash := s.emailHash(email)
	now := s.now()
	var res ApplyResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE slug = ?`, in.JobSlug))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !j.accepting(now) {
			return ErrClosed
		}
		initial, err := s.initialStage(ctx, tx)
		if err != nil {
			return err
		}
		var cid string
		var erased sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT id, erased_at FROM candidates WHERE email_hash = ?`, hash).Scan(&cid, &erased)
		switch {
		case err == nil:
			var dup int
			if e := tx.QueryRowContext(ctx, `SELECT 1 FROM applications WHERE candidate_id = ? AND job_id = ?`, cid, j.ID).Scan(&dup); e == nil {
				return nil // already applied: accepted, not stored again
			}
		case errors.Is(err, sql.ErrNoRows):
			cid = newID("cand")
			res.NewCandidate = true
			if err := s.insertCandidate(ctx, tx, cid, hash, email, in); err != nil {
				return err
			}
		default:
			return err
		}
		aid := newID("app")
		cover, err := s.seal("applications", aid, "cover_letter_sealed", in.CoverLetter)
		if err != nil {
			return err
		}
		var rm ResumeMeta
		if in.Resume != nil {
			rm = *in.Resume
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO applications (id, candidate_id, job_id, stage, status, cover_letter_sealed, source, resume_key, resume_name, resume_type, resume_size, resume_sha256, resume_scan, created_at, updated_at)
			VALUES (?,?,?,?,'active',?,?,?,?,?,?,?,?,?,?)`, aid, cid, j.ID, initial, cover, in.Source, rm.key, rm.Name, rm.Type, rm.Size, rm.SHA256, rm.Scan, ts(now), ts(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO stage_history (application_id, from_stage, to_stage, status, actor, at) VALUES (?, '', ?, 'active', 'applicant', ?)`, aid, initial, ts(now)); err != nil {
			return err
		}
		if err := s.touchRetention(ctx, tx, cid); err != nil {
			return err
		}
		if in.EEO != nil && s.cfg.Apply.CollectEEO {
			if err := s.insertEEO(ctx, tx, *in.EEO); err != nil {
				return err
			}
		}
		res.ApplicationID, res.Created = aid, true
		return s.emit(ctx, tx, EventApplicationRecv, applicationReceivedData{ApplicationID: aid, JobID: j.ID, CandidateID: cid, Source: in.Source, NewCandidate: res.NewCandidate, ReceivedAt: now.UTC()})
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (s *Store) insertCandidate(ctx context.Context, tx dbq, id, hash, email string, in PublicApplication) error {
	em, err := s.seal("candidates", id, "email_sealed", email)
	if err != nil {
		return err
	}
	ph, err := s.seal("candidates", id, "phone_sealed", in.Phone)
	if err != nil {
		return err
	}
	now := ts(s.now())
	pool := 0
	var poolAt any
	if in.TalentPool {
		pool, poolAt = 1, now
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO candidates (id, name, email_sealed, email_hash, phone_sealed, source, consent_at, consent_version, talent_pool, talent_consent_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, id, strings.TrimSpace(in.Name), em, hash, ph, in.Source, now, s.cfg.Apply.ConsentVersion, pool, poolAt, now, now)
	return err
}

// ManualApplication is a recruiter adding a sourced candidate to a job.
type ManualApplication struct {
	JobID       string `json:"job_id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	CoverLetter string `json:"cover_letter"`
	Source      string `json:"source"`
	// ConsentRecorded is the recruiter's attestation that the candidate agreed to the processing
	// of their data (for example by replying to an email). It is audited.
	ConsentRecorded bool `json:"consent_recorded"`
	TalentPool      bool `json:"talent_pool"`
}

// CreateApplication adds a candidate to a job as staff. A job the caller cannot reach is
// reported as missing. Unlike the public form this tells the recruiter about a duplicate:
// ErrConflict with the existing application, which they are entitled to see.
func (s *Store) CreateApplication(ctx context.Context, p *Principal, in ManualApplication) (*ApplicationView, error) {
	if !p.Can(ActAppCreate) {
		return nil, ErrForbidden
	}
	var pr problems
	email, _ := validCandidateFields(&pr, in.Name, in.Email, in.Phone, in.CoverLetter, in.Source)
	if !in.ConsentRecorded {
		pr.add("consent_recorded", "you must attest that the candidate agreed to the processing of their data")
	}
	if err := pr.err(); err != nil {
		return nil, err
	}
	if _, err := s.GetJob(ctx, p, in.JobID); err != nil {
		return nil, err
	}
	hash := s.emailHash(email)
	now := s.now()
	var aid string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, in.JobID).Scan(&status); err != nil {
			return err
		}
		if status == StatusClosed {
			return conflict("the job is closed")
		}
		initial, err := s.initialStage(ctx, tx)
		if err != nil {
			return err
		}
		var cid string
		newCand := false
		err = tx.QueryRowContext(ctx, `SELECT id FROM candidates WHERE email_hash = ?`, hash).Scan(&cid)
		switch {
		case err == nil:
			var existing string
			if e := tx.QueryRowContext(ctx, `SELECT id FROM applications WHERE candidate_id = ? AND job_id = ?`, cid, in.JobID).Scan(&existing); e == nil {
				return conflict("this candidate already applied to this job (application %s)", existing)
			}
		case errors.Is(err, sql.ErrNoRows):
			cid, newCand = newID("cand"), true
			if err := s.insertCandidate(ctx, tx, cid, hash, email, PublicApplication{Name: in.Name, Phone: in.Phone, Source: in.Source, TalentPool: in.TalentPool}); err != nil {
				return err
			}
		default:
			return err
		}
		aid = newID("app")
		cover, err := s.seal("applications", aid, "cover_letter_sealed", in.CoverLetter)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO applications (id, candidate_id, job_id, stage, status, cover_letter_sealed, source, created_at, updated_at) VALUES (?,?,?,?,'active',?,?,?,?)`,
			aid, cid, in.JobID, initial, cover, in.Source, ts(now), ts(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO stage_history (application_id, from_stage, to_stage, status, actor, at) VALUES (?, '', ?, 'active', ?, ?)`, aid, initial, p.Subject, ts(now)); err != nil {
			return err
		}
		if err := s.touchRetention(ctx, tx, cid); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventApplicationRecv, applicationReceivedData{ApplicationID: aid, JobID: in.JobID, CandidateID: cid, Source: in.Source, NewCandidate: newCand, ReceivedAt: now.UTC()}); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "application.create", "application:"+aid, []string{"consent_attested"}, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.GetApplication(ctx, p, aid)
}

// ---- candidate reads ---------------------------------------------------------------------------

func (p *Principal) candidatePred(c string) (string, []any) {
	if p.Role == RoleAdmin || p.Role == RoleRecruiter {
		if p.Unscoped() {
			return "(1)", nil
		}
	}
	ap, args := p.appPred("a", "j")
	return "(EXISTS (SELECT 1 FROM applications a JOIN jobs j ON j.id = a.job_id WHERE a.candidate_id = " + c + ".id AND " + ap + "))", args
}

const candCols = `c.id, c.name, c.email_sealed, c.phone_sealed, c.address_sealed, c.salary_exp_sealed, c.notes_sealed, c.tags, c.source, c.talent_pool, c.talent_consent_at,
	c.consent_at, c.consent_version, c.retain_until, c.legal_hold, c.erased_at, c.created_at, c.updated_at`

// candView unseals a candidate row according to the caller's role. fields lists what was
// actually returned, for the audit record.
func (s *Store) candView(p *Principal, sc scanner) (*Candidate, []string, error) {
	var c Candidate
	var email, phone, addr, sal, notes sql.NullString
	var tags string
	var pool, hold int
	var tcAt, retain, erased sql.NullInt64
	var consent, created, updated int64
	if err := sc.Scan(&c.ID, &c.Name, &email, &phone, &addr, &sal, &notes, &tags, &c.Source, &pool, &tcAt, &consent, &c.ConsentVersion, &retain, &hold, &erased, &created, &updated); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(tags), &c.Tags); err != nil {
		return nil, nil, err
	}
	if c.Tags == nil {
		c.Tags = []string{}
	}
	c.TalentPool, c.LegalHold, c.Erased = pool == 1, hold == 1, erased.Valid
	c.TalentConsentAt, c.RetainUntil = nullTime(tcAt), nullTime(retain)
	c.ConsentAt, c.CreatedAt, c.UpdatedAt = fromUnix(consent), fromUnix(created), fromUnix(updated)
	fields := []string{"name"}
	for _, f := range []struct {
		name string
		col  string
		v    sql.NullString
		dst  **string
	}{
		{fieldEmail, "email_sealed", email, &c.Email}, {fieldPhone, "phone_sealed", phone, &c.Phone}, {fieldAddress, "address_sealed", addr, &c.Address},
		{fieldSalaryExp, "salary_exp_sealed", sal, &c.SalaryExpectation}, {fieldNotes, "notes_sealed", notes, &c.Notes},
	} {
		if !p.canSee(f.name) {
			c.RestrictedFields = append(c.RestrictedFields, f.name)
			continue
		}
		plain, err := s.unseal("candidates", c.ID, f.col, f.v)
		if err != nil {
			return nil, nil, err
		}
		if c.Erased {
			plain = ""
		}
		v := plain
		*f.dst = &v
		if plain != "" {
			fields = append(fields, f.name)
		}
	}
	return &c, fields, nil
}

// GetCandidate returns one candidate, audited. Interviewers cannot read candidate records
// (they see a name on the application they were assigned).
func (s *Store) GetCandidate(ctx context.Context, p *Principal, id string) (*Candidate, error) {
	if !p.Can(ActCandidateRead) {
		return nil, ErrForbidden
	}
	pred, args := p.candidatePred("c")
	c, fields, err := s.candView(p, s.rdb.QueryRowContext(ctx, `SELECT `+candCols+` FROM candidates c WHERE (c.id = ?) AND `+pred, append([]any{id}, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, p, "candidate:"+id, fields); err != nil {
		return nil, err
	}
	return c, nil
}

// CandidateFilter narrows a candidate list.
type CandidateFilter struct {
	Email  string // exact match through the dedupe hash
	Name   string // case-insensitive prefix
	Tag    string
	Cursor int64
	Limit  int
}

// ListCandidates lists candidates the caller can reach. An email lookup is how duplicates are
// found. Every list is audited as one read.
func (s *Store) ListCandidates(ctx context.Context, p *Principal, f CandidateFilter) ([]*Candidate, int64, error) {
	if !p.Can(ActCandidateRead) {
		return nil, 0, ErrForbidden
	}
	hash := ""
	if f.Email != "" {
		e, ok := normEmail(f.Email)
		if !ok {
			return nil, 0, invalid("email", "not a valid email address")
		}
		hash = s.emailHash(e)
	}
	if f.Tag != "" && !tagRE.MatchString(f.Tag) {
		return nil, 0, invalid("tag", "not a valid tag")
	}
	return s.queryCandidates(ctx, p, `(? = '' OR c.email_hash = ?) AND (? = '' OR c.name LIKE ? ESCAPE '\') AND (? = '' OR EXISTS (SELECT 1 FROM json_each(c.tags) WHERE value = ?))`,
		[]any{hash, hash, f.Name, likePrefix(f.Name), f.Tag, f.Tag}, f.Cursor, f.Limit, "candidates:list")
}

func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s) + "%"
}

func (s *Store) queryCandidates(ctx context.Context, p *Principal, where string, wargs []any, cursor int64, limit int, auditRes string) ([]*Candidate, int64, error) {
	limit = clampLimit(limit)
	if cursor <= 0 {
		cursor = 1 << 62
	}
	pred, pargs := p.candidatePred("c")
	args := append(append(append([]any{}, wargs...), cursor), pargs...)
	rows, err := s.rdb.QueryContext(ctx, `SELECT c.rowid, `+candCols+` FROM candidates c WHERE (`+where+`) AND (c.rowid < ?) AND `+pred+` ORDER BY c.rowid DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Candidate
	var rowids []int64
	seen := map[string]bool{}
	for rows.Next() {
		var rid int64
		c, fields, err := s.candView(p, &prefixScanner{rows: rows, dst: &rid})
		if err != nil {
			return nil, 0, err
		}
		for _, f := range fields {
			seen[f] = true
		}
		out = append(out, c)
		rowids = append(rowids, rid)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	_ = rows.Close()
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = rowids[limit-1]
	}
	if out == nil {
		out = []*Candidate{}
	}
	names := make([]string, 0, len(seen))
	for f := range seen {
		names = append(names, f)
	}
	slices.Sort(names)
	if err := s.auditRead(ctx, p, auditRes, names); err != nil {
		return nil, 0, err
	}
	return out, next, nil
}

// ListTalentPool lists candidates who opted in and are not erased.
func (s *Store) ListTalentPool(ctx context.Context, p *Principal, tag string, cursor int64, limit int) ([]*Candidate, int64, error) {
	if !p.Can(ActTalentPool) {
		return nil, 0, ErrForbidden
	}
	if tag != "" && !tagRE.MatchString(tag) {
		return nil, 0, invalid("tag", "not a valid tag")
	}
	return s.queryCandidates(ctx, p, `c.talent_pool = 1 AND c.erased_at IS NULL AND (? = '' OR EXISTS (SELECT 1 FROM json_each(c.tags) WHERE value = ?))`, []any{tag, tag}, cursor, limit, "talent-pool:list")
}

// CandidatePatch edits recruiter-owned fields of a candidate.
type CandidatePatch struct {
	Tags              *[]string `json:"tags"`
	Notes             *string   `json:"notes"`
	Address           *string   `json:"address"`
	SalaryExpectation *string   `json:"salary_expectation"`
	Phone             *string   `json:"phone"`
	// IfUpdatedAt, when set, must equal the candidate's last update (unix seconds) or the patch is
	// refused with ErrVersionMismatch. The web UI uses it for the notes form.
	IfUpdatedAt *int64 `json:"-"`
}

// PatchCandidate updates tags and the sealed recruiter fields.
func (s *Store) PatchCandidate(ctx context.Context, p *Principal, id string, in CandidatePatch) (*Candidate, error) {
	if !p.Can(ActCandidateEdit) {
		return nil, ErrForbidden
	}
	pred, args := p.candidatePred("c")
	var one int
	if err := s.rdb.QueryRowContext(ctx, `SELECT 1 FROM candidates c WHERE (c.id = ? AND c.erased_at IS NULL) AND `+pred, append([]any{id}, args...)...).Scan(&one); err != nil {
		return nil, ErrNotFound
	}
	var pr problems
	if in.Tags != nil {
		if len(*in.Tags) > 20 {
			pr.add("tags", "at most 20 tags")
		}
		for _, t := range *in.Tags {
			if !tagRE.MatchString(t) {
				pr.add("tags", "%q must be lowercase letters, digits, - or _ (30 characters at most)", t)
			}
		}
	}
	if in.Notes != nil {
		pr.text("notes", *in.Notes, 5000, false, true)
	}
	if in.Address != nil {
		pr.text("address", *in.Address, 300, false, true)
	}
	if in.SalaryExpectation != nil {
		pr.text("salary_expectation", *in.SalaryExpectation, 100, false, false)
	}
	if in.Phone != nil && *in.Phone != "" && !phoneRE.MatchString(*in.Phone) {
		pr.add("phone", "digits, spaces and + ( ) . - / only, 5 to 30 characters")
	}
	if err := pr.err(); err != nil {
		return nil, err
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if in.IfUpdatedAt != nil {
			var cur int64
			if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM candidates WHERE id = ?`, id).Scan(&cur); err != nil {
				return err
			}
			if cur != *in.IfUpdatedAt {
				return ErrVersionMismatch
			}
		}
		var changed []string
		set := func(col, name string, v *string) error {
			if v == nil {
				return nil
			}
			sealed, err := s.seal("candidates", id, col, *v)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE candidates SET `+col+` = ?, updated_at = ? WHERE id = ?`, sealed, ts(s.now()), id); err != nil { //nolint:gosec // col is a literal from this function's callers
				return err
			}
			changed = append(changed, name)
			return nil
		}
		for _, x := range []struct {
			col, name string
			v         *string
		}{{"notes_sealed", fieldNotes, in.Notes}, {"address_sealed", fieldAddress, in.Address}, {"salary_exp_sealed", fieldSalaryExp, in.SalaryExpectation}, {"phone_sealed", fieldPhone, in.Phone}} {
			if err := set(x.col, x.name, x.v); err != nil {
				return err
			}
		}
		if in.Tags != nil {
			tj, _ := json.Marshal(dedupe(*in.Tags))
			if _, err := tx.ExecContext(ctx, `UPDATE candidates SET tags = ?, updated_at = ? WHERE id = ?`, string(tj), ts(s.now()), id); err != nil {
				return err
			}
			changed = append(changed, "tags")
		}
		return s.audit(ctx, tx, p.Subject, "candidate.update", "candidate:"+id, changed, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.GetCandidate(ctx, p, id)
}

// SetTalentPool records or withdraws the candidate's talent-pool consent. Recruiters record it
// on the candidate's behalf and attest to it in the audit trail; withdrawing returns the
// candidate to the normal rejected-candidate retention.
func (s *Store) SetTalentPool(ctx context.Context, p *Principal, id string, in bool) (*Candidate, error) {
	if !p.Can(ActTalentPool) {
		return nil, ErrForbidden
	}
	pred, args := p.candidatePred("c")
	var one int
	if err := s.rdb.QueryRowContext(ctx, `SELECT 1 FROM candidates c WHERE (c.id = ? AND c.erased_at IS NULL) AND `+pred, append([]any{id}, args...)...).Scan(&one); err != nil {
		return nil, notFoundOr(err)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := ts(s.now())
		if in {
			if _, err := tx.ExecContext(ctx, `UPDATE candidates SET talent_pool = 1, talent_consent_at = ?, updated_at = ? WHERE id = ?`, now, now, id); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE candidates SET talent_pool = 0, talent_consent_at = NULL, updated_at = ? WHERE id = ?`, now, id); err != nil {
			return err
		}
		if err := s.touchRetention(ctx, tx, id); err != nil {
			return err
		}
		act := "talentpool.add"
		if !in {
			act = "talentpool.remove"
		}
		return s.audit(ctx, tx, p.Subject, act, "candidate:"+id, nil, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.GetCandidate(ctx, p, id)
}

// OpenResume returns the resume file of an application the caller may see, audited. Infected
// files are never served. Interviewers can open the resume of the applications they are
// assigned to.
func (s *Store) OpenResume(ctx context.Context, p *Principal, appID string) (*ResumeMeta, io.ReadCloser, error) {
	if !p.Can(ActAppRead) {
		return nil, nil, ErrForbidden
	}
	pred, args := p.appPred("a", "j")
	var m ResumeMeta
	err := s.rdb.QueryRowContext(ctx, `SELECT a.resume_key, a.resume_name, a.resume_type, a.resume_size, a.resume_sha256, a.resume_scan FROM applications a JOIN jobs j ON j.id = a.job_id WHERE (a.id = ?) AND `+pred,
		append([]any{appID}, args...)...).Scan(&m.key, &m.Name, &m.Type, &m.Size, &m.SHA256, &m.Scan)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && m.key == "") {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if m.Scan == string(ScanInfected) || s.resumes == nil {
		return nil, nil, ErrNotFound
	}
	if err := s.auditRead(ctx, p, "application:"+appID, []string{"resume"}); err != nil {
		return nil, nil, err
	}
	rc, err := s.resumes.Open(ctx, m.key)
	if err != nil {
		return nil, nil, err
	}
	return &m, rc, nil
}

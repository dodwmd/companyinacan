package jobs

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// ---- stages ----------------------------------------------------------------------------------

func (s *Store) seedStages(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM stages`).Scan(&n); err != nil || n > 0 {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error { return writeStages(ctx, tx, DefaultStages()) })
}

func writeStages(ctx context.Context, tx dbq, ss []Stage) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM stages`); err != nil {
		return err
	}
	for i, st := range ss {
		if _, err := tx.ExecContext(ctx, `INSERT INTO stages (key, name, kind, position) VALUES (?,?,?,?)`, st.Key, st.Name, st.Kind, i+1); err != nil {
			return err
		}
	}
	return nil
}

// ListStages returns the pipeline in order. Every authenticated role may read it.
func (s *Store) ListStages(ctx context.Context) ([]Stage, error) {
	return listStages(ctx, s.rdb)
}

func listStages(ctx context.Context, q dbq) ([]Stage, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, name, kind, position FROM stages ORDER BY position`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Stage{}
	for rows.Next() {
		var st Stage
		if err := rows.Scan(&st.Key, &st.Name, &st.Kind, &st.Position); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) initialStage(ctx context.Context, q dbq) (string, error) {
	var k string
	err := q.QueryRowContext(ctx, `SELECT key FROM stages WHERE kind = 'initial'`).Scan(&k)
	return k, err
}

// SetStages replaces the pipeline. A stage that applications still sit in cannot be removed
// or have its kind changed to hired; rename by keeping the key.
func (s *Store) SetStages(ctx context.Context, p *Principal, ss []Stage) ([]Stage, error) {
	if !p.Can(ActStages) {
		return nil, ErrForbidden
	}
	if err := validateStages(ss); err != nil {
		return nil, err
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		keep := map[string]string{}
		for _, st := range ss {
			keep[st.Key] = st.Kind
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT stage, status FROM applications`)
		if err != nil {
			return err
		}
		var bad []string
		for rows.Next() {
			var stg, status string
			if err := rows.Scan(&stg, &status); err != nil {
				_ = rows.Close()
				return err
			}
			if _, ok := keep[stg]; !ok {
				bad = append(bad, stg)
			} else if status == AppHired && keep[stg] != KindHired {
				bad = append(bad, stg)
			}
		}
		_ = rows.Close()
		if len(bad) > 0 {
			return conflict("stages still in use by applications cannot be removed or re-kinded: %v", bad)
		}
		if err := writeStages(ctx, tx, ss); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "stages.set", "stages", nil, "ok")
	})
	if err != nil {
		return nil, err
	}
	return s.ListStages(ctx)
}

// ---- applications ----------------------------------------------------------------------------

// StageChange is one entry of an application's history.
type StageChange struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	Status string    `json:"status"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

// ApplicationView is an application as the API returns it. Sealed text is present only for
// roles allowed to read it (RestrictedFields names what was left out).
type ApplicationView struct {
	ID               string        `json:"id"`
	JobID            string        `json:"job_id"`
	JobTitle         string        `json:"job_title"`
	CandidateID      string        `json:"candidate_id"`
	CandidateName    string        `json:"candidate_name"`
	Stage            string        `json:"stage"`
	Status           string        `json:"status"`
	RejectReason     string        `json:"reject_reason,omitempty"`
	RejectNote       *string       `json:"reject_note,omitempty"`
	CoverLetter      *string       `json:"cover_letter,omitempty"`
	Source           string        `json:"source"`
	Resume           *ResumeMeta   `json:"resume"`
	Interviewers     []string      `json:"interviewers,omitempty"`
	History          []StageChange `json:"history,omitempty"`
	RestrictedFields []string      `json:"restricted_fields,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	DecidedAt        *time.Time    `json:"decided_at"`
}

const appCols = `a.id, a.job_id, j.title, a.candidate_id, c.name, a.stage, a.status, a.reject_reason, a.reject_note_sealed, a.cover_letter_sealed, a.source,
	a.resume_key, a.resume_name, a.resume_type, a.resume_size, a.resume_sha256, a.resume_scan, a.created_at, a.updated_at, a.decided_at`

const appFrom = ` FROM applications a JOIN jobs j ON j.id = a.job_id JOIN candidates c ON c.id = a.candidate_id`

func (s *Store) appView(p *Principal, sc scanner, detail bool) (*ApplicationView, []string, error) {
	var v ApplicationView
	var rejNote, cover sql.NullString
	var rm ResumeMeta
	var created, updated int64
	var decided sql.NullInt64
	if err := sc.Scan(&v.ID, &v.JobID, &v.JobTitle, &v.CandidateID, &v.CandidateName, &v.Stage, &v.Status, &v.RejectReason, &rejNote, &cover, &v.Source,
		&rm.key, &rm.Name, &rm.Type, &rm.Size, &rm.SHA256, &rm.Scan, &created, &updated, &decided); err != nil {
		return nil, nil, err
	}
	v.CreatedAt, v.UpdatedAt, v.DecidedAt = fromUnix(created), fromUnix(updated), nullTime(decided)
	fields := []string{"name"}
	if rm.key != "" {
		v.Resume = &rm
		fields = append(fields, "resume_meta")
	}
	if detail {
		for _, f := range []struct {
			name, col string
			val       sql.NullString
			dst       **string
		}{{fieldCover, "cover_letter_sealed", cover, &v.CoverLetter}, {fieldRejNote, "reject_note_sealed", rejNote, &v.RejectNote}} {
			if !p.canSee(f.name) {
				v.RestrictedFields = append(v.RestrictedFields, f.name)
				continue
			}
			plain, err := s.unseal("applications", v.ID, f.col, f.val)
			if err != nil {
				return nil, nil, err
			}
			x := plain
			*f.dst = &x
			if plain != "" {
				fields = append(fields, f.name)
			}
		}
	}
	return &v, fields, nil
}

// GetApplication returns one application the caller may see, audited.
func (s *Store) GetApplication(ctx context.Context, p *Principal, id string) (*ApplicationView, error) {
	if !p.Can(ActAppRead) {
		return nil, ErrForbidden
	}
	pred, args := p.appPred("a", "j")
	v, fields, err := s.appView(p, s.rdb.QueryRowContext(ctx, `SELECT `+appCols+appFrom+` WHERE (a.id = ?) AND `+pred, append([]any{id}, args...)...), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.Role != RoleInterviewer {
		if v.Interviewers, err = s.interviewersOf(ctx, id); err != nil {
			return nil, err
		}
	}
	if v.History, err = s.historyOf(ctx, id); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, p, "application:"+id, fields); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Store) interviewersOf(ctx context.Context, appID string) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT subject FROM interviewers WHERE application_id = ? ORDER BY subject`, appID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) historyOf(ctx context.Context, appID string) ([]StageChange, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT from_stage, to_stage, status, actor, at FROM stage_history WHERE application_id = ? ORDER BY seq`, appID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []StageChange{}
	for rows.Next() {
		var c StageChange
		var at int64
		if err := rows.Scan(&c.From, &c.To, &c.Status, &c.Actor, &at); err != nil {
			return nil, err
		}
		c.At = fromUnix(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppFilter narrows an application list.
type AppFilter struct {
	JobID       string
	CandidateID string
	Stage       string
	Status      string
	Cursor      int64
	Limit       int
}

// ListApplications lists applications the caller may see, newest first. Lists carry names and
// pipeline state only (no sealed text); one audit record covers the page.
func (s *Store) ListApplications(ctx context.Context, p *Principal, f AppFilter) ([]*ApplicationView, int64, error) {
	if !p.Can(ActAppRead) {
		return nil, 0, ErrForbidden
	}
	if f.Status != "" && !slices.Contains([]string{AppActive, AppRejected, AppWithdrawn, AppHired}, f.Status) {
		return nil, 0, invalid("status", "must be active, rejected, withdrawn or hired")
	}
	limit := clampLimit(f.Limit)
	if f.Cursor <= 0 {
		f.Cursor = 1 << 62
	}
	pred, pargs := p.appPred("a", "j")
	args := []any{f.JobID, f.JobID, f.CandidateID, f.CandidateID, f.Stage, f.Stage, f.Status, f.Status, f.Cursor}
	rows, err := s.rdb.QueryContext(ctx, `SELECT a.rowid, `+appCols+appFrom+` WHERE (? = '' OR a.job_id = ?) AND (? = '' OR a.candidate_id = ?) AND (? = '' OR a.stage = ?) AND (? = '' OR a.status = ?) AND (a.rowid < ?) AND `+pred+` ORDER BY a.rowid DESC LIMIT ?`,
		append(append(args, pargs...), limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ApplicationView
	var rowids []int64
	for rows.Next() {
		var rid int64
		v, _, err := s.appView(p, &prefixScanner{rows: rows, dst: &rid}, false)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
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
		out = []*ApplicationView{}
	}
	if err := s.auditRead(ctx, p, "applications:list", []string{"name"}); err != nil {
		return nil, 0, err
	}
	return out, next, nil
}

type appState struct {
	id, candidate, job, stage, status string
	jobTitle, dept, emp               string
}

// loadAppForWrite reads an application inside the caller's scope, within tx.
func (s *Store) loadAppForWrite(ctx context.Context, tx dbq, p *Principal, id string) (*appState, error) {
	pred, args := p.appPred("a", "j")
	var st appState
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.candidate_id, a.job_id, a.stage, a.status, j.title, j.department, j.employment_type FROM applications a JOIN jobs j ON j.id = a.job_id WHERE (a.id = ?) AND `+pred,
		append([]any{id}, args...)...).Scan(&st.id, &st.candidate, &st.job, &st.stage, &st.status, &st.jobTitle, &st.dept, &st.emp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &st, err
}

func stageKind(ctx context.Context, tx dbq, key string) (string, error) {
	var k string
	err := tx.QueryRowContext(ctx, `SELECT kind FROM stages WHERE key = ?`, key).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalid("stage", "unknown stage %q", key)
	}
	return k, err
}

// MoveApplication moves an active application to another pipeline stage. Hiring is a separate
// action; moving into the hired stage is refused.
func (s *Store) MoveApplication(ctx context.Context, p *Principal, id, to string) (*ApplicationView, error) {
	if !p.Can(ActAppMove) {
		return nil, ErrForbidden
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		st, err := s.loadAppForWrite(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if st.status != AppActive {
			return conflict("the application is %s; only active applications move", st.status)
		}
		kind, err := stageKind(ctx, tx, to)
		if err != nil {
			return err
		}
		if kind == KindHired {
			return conflict("use the hire action to complete a hire")
		}
		if to == st.stage {
			return nil
		}
		now := s.now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE applications SET stage = ?, updated_at = ? WHERE id = ?`, to, ts(now), id); err != nil {
			return err
		}
		return s.recordStage(ctx, tx, p, st, to, AppActive, now, "application.move")
	})
	if err != nil {
		return nil, err
	}
	return s.GetApplication(ctx, p, id)
}

func (s *Store) recordStage(ctx context.Context, tx dbq, p *Principal, st *appState, to, status string, now time.Time, auditAction string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO stage_history (application_id, from_stage, to_stage, status, actor, at) VALUES (?,?,?,?,?,?)`, st.id, st.stage, to, status, p.Subject, ts(now)); err != nil {
		return err
	}
	if err := s.emit(ctx, tx, EventApplicationStage, applicationStageData{ApplicationID: st.id, JobID: st.job, CandidateID: st.candidate, From: st.stage, To: to, Status: status, Actor: p.Subject, ChangedAt: now}); err != nil {
		return err
	}
	return s.audit(ctx, tx, p.Subject, auditAction, "application:"+st.id, nil, "ok")
}

// RejectApplication rejects an active application with a reason from the fixed vocabulary and
// an optional sealed note. The candidate then follows the rejected-candidate retention.
func (s *Store) RejectApplication(ctx context.Context, p *Principal, id, reason, note string) (*ApplicationView, error) {
	if !p.Can(ActAppMove) {
		return nil, ErrForbidden
	}
	var pr problems
	if !slices.Contains(RejectReasons, reason) {
		pr.add("reason", "must be one of %v", RejectReasons)
	}
	pr.text("note", note, 2000, false, true)
	if err := pr.err(); err != nil {
		return nil, err
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		st, err := s.loadAppForWrite(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if st.status != AppActive {
			return conflict("the application is %s; only active applications can be rejected", st.status)
		}
		sealed, err := s.seal("applications", id, "reject_note_sealed", note)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE applications SET status = 'rejected', reject_reason = ?, reject_note_sealed = ?, decided_at = ?, updated_at = ? WHERE id = ?`, reason, sealed, ts(now), ts(now), id); err != nil {
			return err
		}
		if err := s.recordStage(ctx, tx, p, st, st.stage, AppRejected, now, "application.reject"); err != nil {
			return err
		}
		return s.touchRetention(ctx, tx, st.candidate)
	})
	if err != nil {
		return nil, err
	}
	return s.GetApplication(ctx, p, id)
}

// HireApplication completes a hire: the application must sit in an offer stage. It moves to the
// hired stage and publishes candidate.hired.v1 for the people module to pick up.
func (s *Store) HireApplication(ctx context.Context, p *Principal, id string) (*ApplicationView, error) {
	if !p.Can(ActHire) {
		return nil, ErrForbidden
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		st, err := s.loadAppForWrite(ctx, tx, p, id)
		if err != nil {
			return err
		}
		if st.status != AppActive {
			return conflict("the application is %s; only active applications can be hired", st.status)
		}
		kind, err := stageKind(ctx, tx, st.stage)
		if err != nil {
			return err
		}
		if kind != KindOffer {
			return conflict("only an application in an offer stage can be hired (it is in %q)", st.stage)
		}
		var hired string
		if err := tx.QueryRowContext(ctx, `SELECT key FROM stages WHERE kind = 'hired'`).Scan(&hired); err != nil {
			return err
		}
		now := s.now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE applications SET stage = ?, status = 'hired', decided_at = ?, updated_at = ? WHERE id = ?`, hired, ts(now), ts(now), id); err != nil {
			return err
		}
		if err := s.recordStage(ctx, tx, p, st, hired, AppHired, now, "application.hire"); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventCandidateHired, candidateHiredData{CandidateID: st.candidate, ApplicationID: id, JobID: st.job, JobTitle: st.jobTitle, Department: st.dept,
			EmploymentType: st.emp, Actor: p.Subject, HiredAt: now}); err != nil {
			return err
		}
		return s.touchRetention(ctx, tx, st.candidate)
	})
	if err != nil {
		return nil, err
	}
	return s.GetApplication(ctx, p, id)
}

// AssignInterviewer lets a person write a scorecard for an application and, as an interviewer,
// see it. Anyone with ActAssign in scope may assign; the subject is a login, not a candidate.
func (s *Store) AssignInterviewer(ctx context.Context, p *Principal, appID, subject string) error {
	if !p.Can(ActAssign) {
		return ErrForbidden
	}
	if !validSubject(subject) {
		return invalid("subject", "must be the login of the interviewer")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.loadAppForWrite(ctx, tx, p, appID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO interviewers (application_id, subject, assigned_by, at) VALUES (?,?,?,?)`, appID, subject, p.Subject, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "interviewer.assign", "application:"+appID, nil, "ok")
	})
}

// RemoveInterviewer removes an assignment and any draft scorecard (a submitted one stays).
func (s *Store) RemoveInterviewer(ctx context.Context, p *Principal, appID, subject string) error {
	if !p.Can(ActAssign) {
		return ErrForbidden
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.loadAppForWrite(ctx, tx, p, appID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM interviewers WHERE application_id = ? AND subject = ?`, appID, subject)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM scorecards WHERE application_id = ? AND author = ? AND status = 'draft'`, appID, subject); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "interviewer.remove", "application:"+appID, nil, "ok")
	})
}

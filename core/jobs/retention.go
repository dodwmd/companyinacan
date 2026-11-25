package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Data classes held by this module (modules/jobs/module.yaml). Class names match the manifest
// pattern ^[a-z][a-z0-9-]*$ and double as the `class` of erasure ledger entries.
const (
	ClassPII         = "candidate-pii"         // name, contact details, notes, cover letters
	ClassResumes     = "candidate-resumes"     // resume files and their metadata
	ClassEvaluations = "candidate-evaluations" // scorecards and rejection notes
	ClassEEO         = "eeo-aggregate"         // anonymous diversity responses
)

// Classes lists the candidate classes in purge order (files first, so a failure leaves the
// metadata that makes a retry possible).
var Classes = []string{ClassResumes, ClassEvaluations, ClassPII}

var candidateIDRE = regexp.MustCompile(`^cand_[0-9a-f]{16}$`)

// touchRetention recomputes when a candidate's data expires. While any application is active
// the timer is off. After the last decision it runs from that decision (or from the
// talent-pool consent when later): rejected_days normally, talent_pool_days when the
// candidate opted in, hired_days after a hire (0 keeps the record until erased).
func (s *Store) touchRetention(ctx context.Context, tx dbq, candidateID string) error {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM applications WHERE candidate_id = ? AND status = 'active'`, candidateID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		_, err := tx.ExecContext(ctx, `UPDATE candidates SET retain_until = NULL WHERE id = ?`, candidateID)
		return err
	}
	var decided, poolAt sql.NullInt64
	var hired, pool int
	var created int64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(decided_at), COALESCE(MAX(status = 'hired'), 0) FROM applications WHERE candidate_id = ?`, candidateID).Scan(&decided, &hired); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT talent_pool, talent_consent_at, created_at FROM candidates WHERE id = ?`, candidateID).Scan(&pool, &poolAt, &created); err != nil {
		return err
	}
	base := max(decided.Int64, created)
	if pool == 1 && poolAt.Valid {
		base = max(base, poolAt.Int64)
	}
	days := s.cfg.Retention.RejectedDays
	switch {
	case hired == 1:
		days = s.cfg.Retention.HiredDays
	case pool == 1:
		days = s.cfg.Retention.TalentPoolDays
	}
	var until any
	if days > 0 {
		until = fromUnix(base).AddDate(0, 0, days).Unix()
	}
	_, err := tx.ExecContext(ctx, `UPDATE candidates SET retain_until = ? WHERE id = ?`, until, candidateID)
	return err
}

// purgeResumeFiles deletes the resume files of a candidate. It runs before the metadata is
// cleared, so a failure leaves the keys in place and the whole purge can be retried.
func (s *Store) purgeResumeFiles(ctx context.Context, candidateID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT resume_key FROM applications WHERE candidate_id = ? AND resume_key != ''`, candidateID)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	_ = rows.Close()
	if s.resumes == nil {
		return nil
	}
	for _, k := range keys {
		if err := s.resumes.Delete(ctx, k); err != nil {
			return fmt.Errorf("delete resume file: %w", err)
		}
	}
	return nil
}

// purgeClass removes one data class for one candidate inside tx. It is idempotent and shared by
// retention, erasure and the restore-time ledger replay.
func (s *Store) purgeClass(ctx context.Context, tx dbq, id, class string) error {
	now := ts(s.now())
	var stmts []string
	switch class {
	case ClassResumes:
		stmts = []string{`UPDATE applications SET resume_key='', resume_name='', resume_type='', resume_size=0, resume_sha256='', resume_scan='' WHERE candidate_id = ?`}
	case ClassEvaluations:
		stmts = []string{
			`DELETE FROM scorecards WHERE application_id IN (SELECT id FROM applications WHERE candidate_id = ?)`,
			`UPDATE applications SET reject_note_sealed = NULL WHERE candidate_id = ?`,
		}
	case ClassPII:
		stmts = []string{
			fmt.Sprintf(`UPDATE candidates SET name='[erased]', email_sealed=NULL, email_hash='erased:'||id, phone_sealed=NULL, address_sealed=NULL, salary_exp_sealed=NULL, notes_sealed=NULL,
				tags='[]', source='', talent_pool=0, talent_consent_at=NULL, retain_until=NULL, erased_at=COALESCE(erased_at, %d), updated_at=%d WHERE id = ?`, now, now),
			`UPDATE applications SET cover_letter_sealed = NULL WHERE candidate_id = ?`,
			fmt.Sprintf(`UPDATE applications SET status='withdrawn', decided_at=COALESCE(decided_at, %d), updated_at=%d WHERE candidate_id = ? AND status='active'`, now, now),
		}
	default:
		return invalid("class", "unknown data class %q", class)
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st, id); err != nil {
			return err
		}
	}
	return nil
}

// Erasure is the result of an erasure request: an opaque subject id and the classes purged.
type Erasure struct {
	Subject string   `json:"subject"`
	Classes []string `json:"classes"`
	Date    string   `json:"date"`
}

func (s *Store) today() string { return s.now().UTC().Format("2006-01-02") }

// RequestErasure purges a candidate on request and records tombstones the backup ledger takes.
// Admin only; a legal hold blocks it. Pipeline shells (ids, stages, dates) remain so reports
// stay consistent; no personal data does. Anonymous diversity responses are not linked to the
// candidate and are not touched (docs/recruiting.md).
func (s *Store) RequestErasure(ctx context.Context, p *Principal, id string) (*Erasure, error) {
	if !p.Can(ActAdmin) {
		return nil, ErrForbidden
	}
	var hold int
	err := s.db.QueryRowContext(ctx, `SELECT legal_hold FROM candidates WHERE id = ?`, id).Scan(&hold)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if hold == 1 {
		s.auditDeny(ctx, p, "candidate.erase", "candidate:"+id)
		return nil, conflict("erasure is blocked by a legal hold")
	}
	if err := s.purgeResumeFiles(ctx, id); err != nil {
		return nil, err
	}
	date := s.today()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		for _, c := range Classes {
			if err := s.purgeClass(ctx, tx, id, c); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO erasures (subject, class, date, created_at) VALUES (?,?,?,?)`, id, c, date, ts(s.now())); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, p.Subject, "candidate.erase", "candidate:"+id, nil, "ok")
	})
	if err != nil {
		return nil, err
	}
	// The erasure and its audit record are committed; now remove it from the file's free pages
	// and the write-ahead log. A failure here is reported and the request can be repeated.
	if err := s.scrub(ctx, true); err != nil {
		return nil, fmt.Errorf("the candidate is erased and audited, but scrubbing the database files failed (repeat the request: it is idempotent and retries the scrub): %w", err)
	}
	return &Erasure{Subject: id, Classes: Classes, Date: date}, nil
}

// SetLegalHold places or lifts a legal hold on a candidate. Admin only. A hold blocks erasure
// requests and retention purges.
func (s *Store) SetLegalHold(ctx context.Context, p *Principal, id string, hold bool) error {
	if !p.Can(ActAdmin) {
		return ErrForbidden
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var erased sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT erased_at FROM candidates WHERE id = ?`, id).Scan(&erased); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if erased.Valid {
			return conflict("the record is already erased")
		}
		n := 0
		if hold {
			n = 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE candidates SET legal_hold = ?, updated_at = ? WHERE id = ?`, n, ts(s.now()), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "candidate.hold", "candidate:"+id, nil, "ok")
	})
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
}

// ListErasures returns tombstones after a cursor. Admin only (the can-core service account).
func (s *Store) ListErasures(ctx context.Context, p *Principal, after int64) ([]ErasureEntry, error) {
	if !p.Can(ActAdmin) {
		return nil, ErrForbidden
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT seq, subject, class, date FROM erasures WHERE seq > ? ORDER BY seq LIMIT 1000`, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ErasureEntry{}
	for rows.Next() {
		var e ErasureEntry
		if err := rows.Scan(&e.Seq, &e.Subject, &e.Class, &e.Date); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeSubject is the purge(subject, class) hook: idempotent, ignores legal holds (the erasure
// was authorised when the tombstone was written) and safe to replay after a restore. An unknown
// subject is a no-op.
func (s *Store) PurgeSubject(ctx context.Context, p *Principal, subject, class string) error {
	if !p.Can(ActAdmin) {
		return ErrForbidden
	}
	if !candidateIDRE.MatchString(subject) {
		return invalid("subject", "must be a candidate id")
	}
	if class != ClassResumes && class != ClassEvaluations && class != ClassPII {
		return invalid("class", "must be one of %v", Classes)
	}
	if class == ClassResumes {
		if err := s.purgeResumeFiles(ctx, subject); err != nil {
			return err
		}
	}
	if err := s.inTx(ctx, func(tx *sql.Tx) error { return s.purgeClass(ctx, tx, subject, class) }); err != nil {
		return err
	}
	return s.scrub(ctx, false)
}

// RetentionReport counts what a sweep did.
type RetentionReport struct {
	CandidatesPurged int `json:"candidates_purged"`
	SkippedLegalHold int `json:"skipped_legal_hold"`
	JobsExpired      int `json:"jobs_expired"`
	JobsDeleted      int `json:"jobs_deleted"`
	EEODeleted       int `json:"eeo_deleted"`
}

// RunRetention enforces the retention periods. It is safe to run at any time and repeatedly.
func (s *Store) RunRetention(ctx context.Context) (*RetentionReport, error) {
	rep := &RetentionReport{}
	now := s.now()
	// 1. Open jobs past validThrough close (the careers page already treats them as closed).
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE status = 'open' AND valid_through IS NOT NULL AND valid_through <= ?`, ts(now))
	if err != nil {
		return rep, err
	}
	var expired []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			_ = rows.Close()
			return rep, err
		}
		expired = append(expired, j)
	}
	_ = rows.Close()
	for _, j := range expired {
		res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status='closed', closed_at=?, close_reason='expired', updated_at=?, version=version+1 WHERE id = ? AND status='open'`, ts(now), ts(now), j.ID)
		if err != nil {
			return rep, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rep.JobsExpired++
			j.Status = StatusClosed
			s.syndicate(ctx, j, "close")
		}
	}
	// 2. Candidates past their retention date.
	crow, err := s.db.QueryContext(ctx, `SELECT id, legal_hold FROM candidates WHERE erased_at IS NULL AND retain_until IS NOT NULL AND retain_until <= ? ORDER BY retain_until LIMIT 500`, ts(now))
	if err != nil {
		return rep, err
	}
	type due struct {
		id   string
		hold bool
	}
	var dues []due
	for crow.Next() {
		var d due
		var h int
		if err := crow.Scan(&d.id, &h); err != nil {
			_ = crow.Close()
			return rep, err
		}
		d.hold = h == 1
		dues = append(dues, d)
	}
	_ = crow.Close()
	for _, d := range dues {
		if d.hold {
			rep.SkippedLegalHold++
			continue
		}
		if err := s.purgeResumeFiles(ctx, d.id); err != nil {
			return rep, err
		}
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			for _, c := range Classes {
				if err := s.purgeClass(ctx, tx, d.id, c); err != nil {
					return err
				}
			}
			return s.audit(ctx, tx, "system", "candidate.retention_purge", "candidate:"+d.id, nil, "ok")
		})
		if err != nil {
			return rep, err
		}
		rep.CandidatesPurged++
	}
	// 3. Closed jobs past their period, once no application (even an erased shell) points at
	// them, and erased candidate shells that no application points at any more.
	if d := s.cfg.Retention.ClosedJobDays; d > 0 {
		res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE status = 'closed' AND closed_at IS NOT NULL AND closed_at < ? AND NOT EXISTS (SELECT 1 FROM applications a WHERE a.job_id = jobs.id)`,
			ts(now.AddDate(0, 0, -d)))
		if err != nil {
			return rep, err
		}
		n, _ := res.RowsAffected()
		rep.JobsDeleted = int(n)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM candidates WHERE erased_at IS NOT NULL AND legal_hold = 0 AND NOT EXISTS (SELECT 1 FROM applications a WHERE a.candidate_id = candidates.id)`); err != nil {
		return rep, err
	}
	// 4. Anonymous diversity responses.
	if d := s.cfg.Retention.EEODays; d > 0 {
		res, err := s.db.ExecContext(ctx, `DELETE FROM eeo_responses WHERE month < ?`, now.AddDate(0, 0, -d).UTC().Format("2006-01"))
		if err != nil {
			return rep, err
		}
		n, _ := res.RowsAffected()
		rep.EEODeleted = int(n)
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error { return s.purgeOutbox(ctx, tx, now) })
	if err != nil {
		return rep, err
	}
	if rep.CandidatesPurged+rep.JobsDeleted+rep.EEODeleted > 0 {
		if err := s.scrub(ctx, true); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// SweepInterval is how often the background sweep runs.
func (s *Store) sweepInterval() time.Duration {
	return time.Duration(s.cfg.Retention.SweepIntervalHours) * time.Hour
}

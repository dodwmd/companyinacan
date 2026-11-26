package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Data classes held by this module (modules/files/module.yaml). Class names match the manifest
// pattern ^[a-z][a-z0-9-]*$ and double as the `class` of erasure ledger entries.
const (
	ClassContent  = "file-content"  // the bytes of every version
	ClassMetadata = "file-metadata" // names, descriptions and share labels
)

// Classes lists the classes in purge order.
var Classes = []string{ClassContent, ClassMetadata}

const pendingBlobGrace = time.Hour

// SetHold places or lifts a legal hold, and changes the retention date only when retainUntil is
// given: nil leaves it as it is, a pointer to the zero time clears it. files.admin
// only: the people who manage a space must not be able to lift a hold placed on it. A held file
// cannot be deleted, purged, erased or swept; a retention date blocks deletion until it passes.
// The admin acts on ids and sees no content.
func (s *Store) SetHold(ctx context.Context, p *Principal, fileID string, hold bool, retainUntil *time.Time) error {
	if !p.Has(PermAdmin) {
		return ErrForbidden
	}
	if !validID("fil", fileID) {
		return ErrNotFound
	}
	var until any
	change := retainUntil != nil
	if change && !retainUntil.IsZero() {
		if retainUntil.Before(s.now().AddDate(-1, 0, 0)) || retainUntil.After(s.now().AddDate(100, 0, 0)) {
			return invalid("retain_until is out of range")
		}
		until = ts(*retainUntil)
	}
	h := 0
	if hold {
		h = 1
	}
	return s.txFor(ctx, p, "file.hold", "file:"+fileID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE files SET legal_hold = ?, retain_until = CASE WHEN ? THEN ? ELSE retain_until END, rev = rev + 1, updated_at = ? WHERE (id = ?) AND (status IN ('active','deleted'))`, h, change, until, ts(s.now()), fileID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var st string
			if err := tx.QueryRowContext(ctx, `SELECT status FROM files WHERE (id = ?)`, fileID).Scan(&st); err == nil && st == "purging" {
				return conflict("the file is being purged: it is too late to hold it")
			}
			return ErrNotFound
		}
		var fields []string
		if change {
			fields = append(fields, "retain_until")
		}
		if hold {
			fields = append(fields, "hold:on")
		} else {
			fields = append(fields, "hold:off")
		}
		return s.audit(ctx, tx, p.Subject, "file.hold", "file:"+fileID, fields, "ok")
	})
}

// AdminFile is a file as the administrator's locating list shows it: ids, state and sizes, no
// names, descriptions or content.
type AdminFile struct {
	ID          string     `json:"id"`
	SpaceID     string     `json:"space_id"`
	Subject     string     `json:"subject,omitempty"`
	Ref         string     `json:"ref,omitempty"`
	Status      string     `json:"status"`
	Versions    int        `json:"versions"`
	Size        int64      `json:"size"`
	LegalHold   bool       `json:"legal_hold"`
	RetainUntil *time.Time `json:"retain_until,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// AdminFiles lists files by subject for holds and erasure, in id order. files.admin only.
func (s *Store) AdminFiles(ctx context.Context, p *Principal, subject, cursor string, limit int) (Page[AdminFile], error) {
	out := Page[AdminFile]{Items: []AdminFile{}}
	if !p.Has(PermAdmin) {
		return out, ErrForbidden
	}
	if (subject != "" && !subjectRe.MatchString(subject)) || (cursor != "" && !validID("fil", cursor)) {
		return out, invalid("subject or cursor is malformed")
	}
	limit = clampLimit(limit)
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.space_id, f.subject, f.ref, f.status, f.current_version, COALESCE(v.size, 0), f.legal_hold, f.retain_until, f.created_at, f.deleted_at
		FROM files f LEFT JOIN versions v ON (v.file_id = f.id) AND (v.n = f.current_version)
		WHERE (f.id > ?) AND ((? = '') OR (f.subject = ?)) ORDER BY f.id LIMIT ?`, cursor, subject, subject, limit+1)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a AdminFile
		var hold int
		var retain, deleted sql.NullInt64
		var created int64
		if err := rows.Scan(&a.ID, &a.SpaceID, &a.Subject, &a.Ref, &a.Status, &a.Versions, &a.Size, &hold, &retain, &created, &deleted); err != nil {
			return out, err
		}
		a.LegalHold, a.RetainUntil, a.CreatedAt, a.DeletedAt = hold == 1, nullTime(retain), fromUnix(created), nullTime(deleted)
		out.Items = append(out.Items, a)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].ID
	}
	return out, rows.Err()
}

// ---- purging -------------------------------------------------------------------------------------
//
// A purge has two halves. The decision is one transaction: it re-checks holds, retention dates and
// state, marks the files `purging` and writes the tombstones. From then on nothing can hold,
// restore, version or share them. The destruction follows: delete exactly the blobs the claimed
// versions name, then record it. A crash or a storage failure in between leaves files `purging`,
// which the next sweep (or a repeat of the request) finishes.

const (
	purgeBatch = 200
	// heldOrLocked matches a file a purge decided by policy must not touch. Its one argument is now.
	heldOrLocked = `((legal_hold = 1) OR ((retain_until IS NOT NULL) AND (retain_until > ?)))`
	ofSubject    = `((subject = ?) OR (id = ?))`
)

type purgeIntent struct{ reason, actor, action string }

// markPurging claims the active and deleted files matching where, inside tx.
func (s *Store) markPurging(ctx context.Context, tx dbq, in purgeIntent, where string, args ...any) (int64, error) {
	all := append([]any{in.reason, in.actor, in.action, ts(s.now())}, args...)
	res, err := tx.ExecContext(ctx, `UPDATE files SET status = 'purging', purge_reason = ?, purge_actor = ?, purge_action = ?, rev = rev + 1, updated_at = ?
		WHERE (status IN ('active','deleted')) AND `+where, all...) //nolint:gosec // fixed clauses
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// tombstoneClaimed writes one tombstone per class for every file claimed by action. The subject is
// the file's own id: purging one file must never take the data subject's other files with it on a
// restore.
func (s *Store) tombstoneClaimed(ctx context.Context, tx dbq, action string) error {
	for _, class := range Classes {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO erasures (subject, class, date, created_at) SELECT id, ?, ?, ? FROM files WHERE (status = 'purging') AND (purge_action = ?)`,
			class, s.now().UTC().Format(time.DateOnly), ts(s.now()), action); err != nil {
			return err
		}
	}
	return nil
}

// maxReported bounds how many failures one pass spells out; the rest are counted.
const maxReported = 20

// failures collects what went wrong in a pass that carries on past each failure.
type failures struct {
	errs []error
	more int
}

func (f *failures) add(err error) {
	switch {
	case err == nil:
	case len(f.errs) < maxReported:
		f.errs = append(f.errs, err)
	default:
		f.more++
	}
}

func (f *failures) err() error {
	if f.more > 0 {
		return errors.Join(append(f.errs[:len(f.errs):len(f.errs)], fmt.Errorf("and %d more failures", f.more))...)
	}
	return errors.Join(f.errs...)
}

// finishPurges destroys every claimed file matching where, in batches until none is left. A file
// that cannot be destroyed stays claimed and does not stop the others; the failures are returned
// joined.
func (s *Store) finishPurges(ctx context.Context, where string, args ...any) (int, error) {
	total := 0
	last := ""
	var failed failures
	for {
		ids, err := s.claimedIDs(ctx, last, where, args...)
		if err != nil || len(ids) == 0 {
			if total > 0 {
				s.wake()
			}
			failed.add(err)
			return total, failed.err()
		}
		for _, id := range ids {
			done, err := s.finishPurge(ctx, id)
			failed.add(err)
			if done {
				total++
			}
			if ctx.Err() != nil {
				failed.add(ctx.Err())
				return total, failed.err()
			}
		}
		last = ids[len(ids)-1]
	}
}

func (s *Store) claimedIDs(ctx context.Context, after, where string, args ...any) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	all := append(append([]any{after}, args...), purgeBatch)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM files WHERE (status = 'purging') AND (id > ?) AND `+where+` ORDER BY id LIMIT ?`, all...) //nolint:gosec // fixed clauses
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// claimedBlobs reads what finishPurge needs about one claimed file.
func (s *Store) claimedBlobs(ctx context.Context, id string) (space string, in purgeIntent, keys []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err = s.db.QueryRowContext(ctx, `SELECT space_id, purge_reason, purge_actor, purge_action FROM files WHERE (id = ?) AND (status = 'purging')`, id).
		Scan(&space, &in.reason, &in.actor, &in.action); err != nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT blob_key FROM versions WHERE (file_id = ?) AND (purged_at IS NULL)`, id)
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k string
		if err = rows.Scan(&k); err != nil {
			return
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	return
}

// finishPurge deletes the blobs of one claimed file and then, in one transaction, blanks its
// sealed names and those version references, drops its share links and records the audit entry and
// the event. It reports false when someone else finished it first.
func (s *Store) finishPurge(ctx context.Context, id string) (bool, error) {
	space, in, keys, err := s.claimedBlobs(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Every blob is tried, so one that cannot be deleted does not keep the others' bytes around.
	stuck := 0
	var cause error
	for _, k := range keys {
		if err := s.backend.Delete(ctx, k); err != nil {
			stuck, cause = stuck+1, err
		}
	}
	if stuck > 0 {
		return false, fmt.Errorf("%w: a stored object could not be deleted; the file stays marked for purging, repeat the request or let the sweep finish it (%s: %d of %d objects: %v)", ErrUnavailable, id, stuck, len(keys), cause)
	}
	done := false
	now := s.now()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE files SET status = 'purged', name_sealed = NULL, description_sealed = NULL, folder_id = NULL, legal_hold = 0, retain_until = NULL,
			erased_at = ?, rev = rev + 1, updated_at = ? WHERE (id = ?) AND (status = 'purging')`, ts(now), ts(now), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, `UPDATE versions SET blob_key = '', sha256 = '', purged_at = ? WHERE (file_id = ?) AND (blob_key = ?)`, ts(now), id, k); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE (file_id = ?)`, id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, in.actor, in.action, "file:"+id, nil, "ok"); err != nil {
			return err
		}
		done = true
		return s.emit(ctx, tx, EventDeleted, deletedEvent(id, space, in.reason, in.actor, now))
	})
	return done && err == nil, err
}

// errScrub is an erasure that destroyed the files but could not scrub the database files.
var errScrub = errors.New("the files are erased and audited, but scrubbing the database files failed (repeat the request: it is idempotent and retries the scrub)")

// Erasure is the result of an erasure request: an opaque subject id and the classes purged.
type Erasure struct {
	Subject string   `json:"subject"`
	Classes []string `json:"classes"`
	Date    string   `json:"date"`
	Files   int      `json:"files"`
}

// EraseSubject purges every file of a data subject on request and records a tombstone for each of
// those files for the backup ledger. files.admin only; a legal hold or an unpassed retention date on any of
// the files blocks the whole request. Idempotent: a repeat finishes an interrupted erasure and
// retries the scrub of the database files, which is the last step.
func (s *Store) EraseSubject(ctx context.Context, p *Principal, subject string) (*Erasure, error) {
	if !p.Has(PermAdmin) {
		return nil, ErrForbidden
	}
	if !subjectRe.MatchString(subject) {
		return nil, invalid("the subject must be an opaque id")
	}
	date := s.now().UTC().Format(time.DateOnly)
	blocked := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE `+ofSubject+` AND (status IN ('active','deleted')) AND `+heldOrLocked, subject, subject, ts(s.now())).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			blocked = true
			return conflict("erasure is blocked by a legal hold or a retention date")
		}
		if _, err := s.markPurging(ctx, tx, purgeIntent{"erasure", p.Subject, "file.erase"}, ofSubject, subject, subject); err != nil {
			return err
		}
		// The tombstones name the files, not the data subject: a replay must never take a file
		// uploaded for the subject afterwards.
		if err := s.tombstoneClaimed(ctx, tx, "file.erase"); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "subject.erase", "subject:"+subject, nil, "ok")
	})
	if blocked {
		s.auditDeny(ctx, p.Subject, "file.erase", "subject:"+subject)
	}
	if err != nil {
		return nil, err
	}
	n, err := s.finishPurges(ctx, ofSubject, subject, subject)
	if err != nil {
		if n > 0 { // what could be destroyed is gone: do not leave its names in free pages
			err = errors.Join(err, s.scrub(ctx, true))
		}
		return nil, err
	}
	// The erasure and its audit records are committed; now remove it from the file's free pages
	// and the write-ahead log. A failure is reported and the request can be repeated.
	if err := s.scrub(ctx, true); err != nil {
		return nil, fmt.Errorf("%w: %w", errScrub, err)
	}
	return &Erasure{Subject: subject, Classes: Classes, Date: date, Files: n}, nil
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
	// At is when the data was erased: a restore leaves alone a backup made after it.
	At string `json:"at,omitempty"`
}

// ListErasures returns tombstones after a cursor. files.admin only (the can-core service account).
func (s *Store) ListErasures(ctx context.Context, p *Principal, after int64) ([]ErasureEntry, error) {
	if !p.Has(PermAdmin) {
		return nil, ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT seq, subject, class, date, created_at FROM erasures WHERE (seq > ?) ORDER BY seq LIMIT 1000`, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ErasureEntry{}
	for rows.Next() {
		var e ErasureEntry
		var created int64
		if err := rows.Scan(&e.Seq, &e.Subject, &e.Class, &e.Date, &created); err != nil {
			return nil, err
		}
		e.At = time.Unix(created, 0).UTC().Format(time.RFC3339)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeSubject is the purge(subject, class) hook the backup restore replays: idempotent, and an
// unknown subject is a no-op. The subject is a file id, or a data subject id (a tombstone written
// by hand or by an earlier release). An administrator is refused when any matching file is under a legal hold or a
// retention date. Only the restore-only credential (Principal.Restore) ignores both, because a
// restore re-applies an erasure that was authorised when its tombstone was written; it is audited
// as file.restore_purge.
func (s *Store) PurgeSubject(ctx context.Context, p *Principal, subject, class string) error {
	if !p.Restore && !p.Has(PermAdmin) {
		return ErrForbidden
	}
	if !subjectRe.MatchString(subject) {
		return invalid("subject must be an opaque id")
	}
	if class != ClassContent && class != ClassMetadata {
		return invalid("class must be one of %v", Classes)
	}
	action := "file.purge"
	if p.Restore {
		action = "file.restore_purge"
	}
	blocked := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if !p.Restore {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE `+ofSubject+` AND (status IN ('active','deleted')) AND `+heldOrLocked, subject, subject, ts(s.now())).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				blocked = true
				return conflict("a legal hold or a retention date forbids purging; only a restore replay may")
			}
		}
		n, err := s.markPurging(ctx, tx, purgeIntent{"erasure", p.Subject, action}, ofSubject, subject, subject)
		if err != nil || n == 0 {
			return err
		}
		return s.tombstoneClaimed(ctx, tx, action)
	})
	if blocked {
		s.auditDeny(ctx, p.Subject, action, "subject:"+subject)
	}
	if err != nil {
		return err
	}
	n, err := s.finishPurges(ctx, ofSubject, subject, subject)
	if err != nil && n == 0 {
		return err
	}
	return errors.Join(err, s.scrub(ctx, false))
}

// RetentionReport counts what a sweep did.
type RetentionReport struct {
	Purged        int `json:"purged"`
	SkippedHold   int `json:"skipped_legal_hold"`
	SkippedLocked int `json:"skipped_retention_lock"`
	Orphans       int `json:"orphan_blobs_removed"`
}

// sweepTrash finishes interrupted purges, then claims and purges the files whose trash period has
// ended. Held and retention-locked files are excluded in the query, so they can never fill a batch
// and starve the others.
func (s *Store) sweepTrash(ctx context.Context, rep *RetentionReport) error {
	// stuck is what the latest pass could not destroy. Each pass retries every claimed file, so the
	// latest one says it all; claiming goes on regardless.
	n, stuck := s.finishPurges(ctx, `(1 = 1)`)
	rep.Purged += n
	d := *s.cfg.Retention.TrashDays
	if d <= 0 {
		return stuck
	}
	const due = `(status = 'deleted') AND (deleted_at <= ?)`
	cutoff := ts(s.now().AddDate(0, 0, -d))
	counted := s.countSkipped(ctx, rep, due, cutoff)
	for {
		var claimed int64
		if err := s.inTx(ctx, func(tx *sql.Tx) error {
			var err error
			claimed, err = s.markPurging(ctx, tx, purgeIntent{"retention", "system", "file.retention_purge"},
				`(id IN (SELECT id FROM files WHERE `+due+` AND NOT `+heldOrLocked+` ORDER BY deleted_at LIMIT ?))`, cutoff, ts(s.now()), purgeBatch)
			if err != nil || claimed == 0 {
				return err
			}
			return s.tombstoneClaimed(ctx, tx, "file.retention_purge")
		}); err != nil {
			return errors.Join(counted, stuck, err)
		}
		if claimed == 0 {
			return errors.Join(counted, stuck)
		}
		n, stuck = s.finishPurges(ctx, `(1 = 1)`)
		rep.Purged += n
	}
}

func (s *Store) countSkipped(ctx context.Context, rep *RetentionReport, due string, cutoff int64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(legal_hold = 1), 0), COALESCE(SUM((legal_hold = 0) AND (retain_until IS NOT NULL) AND (retain_until > ?)), 0) FROM files WHERE `+due, //nolint:gosec // fixed clause
		ts(s.now()), cutoff).Scan(&rep.SkippedHold, &rep.SkippedLocked)
}

// RunRetention enforces the retention rules: files in the trash for trash_days are purged (a hold
// or an unpassed retention date skips them), interrupted purges are finished, blobs that were
// written but never recorded are removed, and old outbox rows are dropped. Safe to run at any time
// and repeatedly. A failure does not stop the rest of the work: every failure is returned, joined,
// beside the report of what was done.
func (s *Store) RunRetention(ctx context.Context) (*RetentionReport, error) {
	rep := &RetentionReport{}
	now := s.now()
	var failed failures
	failed.add(s.sweepTrash(ctx, rep))
	s.sweepOrphans(ctx, now.Add(-pendingBlobGrace), rep, &failed)
	failed.add(s.inTx(ctx, func(tx *sql.Tx) error { return s.purgeOutbox(ctx, tx, now) }))
	if rep.Purged > 0 {
		failed.add(s.scrub(ctx, true))
	}
	return rep, failed.err()
}

const orphanBatch = 500

// sweepOrphans removes every blob registered before cutoff and never recorded. It pages by blob_key,
// so blobs that cannot be deleted (and stay registered) never keep the later ones from being tried.
func (s *Store) sweepOrphans(ctx context.Context, cutoff time.Time, rep *RetentionReport, failed *failures) {
	after := ""
	for {
		keys, err := s.orphanBlobs(ctx, cutoff, after)
		if err != nil || len(keys) == 0 {
			failed.add(err)
			return
		}
		for _, k := range keys {
			if ctx.Err() != nil {
				failed.add(ctx.Err())
				return
			}
			if err := s.backend.Delete(ctx, k); err != nil {
				failed.add(fmt.Errorf("orphan blob %s could not be deleted: %w", k, err))
				continue
			}
			if err := s.inTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `DELETE FROM pending_blobs WHERE (blob_key = ?)`, k)
				return err
			}); err != nil {
				failed.add(err)
				continue
			}
			rep.Orphans++
		}
		after = keys[len(keys)-1]
	}
}

// orphanBlobs is one page, in key order after `after`, of the blobs registered before cutoff.
func (s *Store) orphanBlobs(ctx context.Context, cutoff time.Time, after string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT blob_key FROM pending_blobs WHERE (blob_key > ?) AND (created_at <= ?) ORDER BY blob_key LIMIT ?`, after, ts(cutoff), orphanBatch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

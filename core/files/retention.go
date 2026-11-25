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

// SetHold places or lifts a legal hold and sets the retention date (nil clears it). files.admin
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
	if retainUntil != nil {
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
		res, err := tx.ExecContext(ctx, `UPDATE files SET legal_hold = ?, retain_until = ?, rev = rev + 1, updated_at = ? WHERE (id = ?) AND (status != 'purged')`, h, until, ts(s.now()), fileID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		fields := []string{"retain_until"}
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

// purgeFiles destroys the content and metadata of files: the blobs first (so a failure leaves
// every reference in place and the whole step can be retried), then, in one transaction, the
// sealed names, the version references, the share links, the tombstones and the audit entries.
// It ignores holds: callers decide whether a hold applies.
func (s *Store) purgeFiles(ctx context.Context, ids []string, actor, action, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	type fileInfo struct{ id, space, tomb string }
	var infos []fileInfo
	var keys []string
	if err := func() error {
		cctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		for _, id := range ids {
			var fi fileInfo
			var subject string
			if err := s.db.QueryRowContext(cctx, `SELECT id, space_id, subject FROM files WHERE (id = ?) AND (status != 'purged')`, id).Scan(&fi.id, &fi.space, &subject); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			fi.tomb = fi.id
			if subject != "" {
				fi.tomb = subject
			}
			infos = append(infos, fi)
			rows, err := s.db.QueryContext(cctx, `SELECT blob_key FROM versions WHERE (file_id = ?) AND (purged_at IS NULL)`, id)
			if err != nil {
				return err
			}
			for rows.Next() {
				var k string
				if err := rows.Scan(&k); err != nil {
					_ = rows.Close()
					return err
				}
				keys = append(keys, k)
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		return 0, err
	}
	for _, k := range keys {
		if err := s.backend.Delete(ctx, k); err != nil {
			return 0, fmt.Errorf("%w: a stored object could not be deleted; nothing was recorded as purged, repeat the request", ErrUnavailable)
		}
	}
	now, date := s.now(), s.now().UTC().Format(time.DateOnly)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, fi := range infos {
			if _, err := tx.ExecContext(ctx, `UPDATE files SET status = 'purged', name_sealed = NULL, description_sealed = NULL, folder_id = NULL, legal_hold = 0, retain_until = NULL,
				erased_at = ?, rev = rev + 1, updated_at = ? WHERE (id = ?)`, ts(now), ts(now), fi.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE versions SET blob_key = '', sha256 = '', purged_at = ? WHERE (file_id = ?) AND (purged_at IS NULL)`, ts(now), fi.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE (file_id = ?)`, fi.id); err != nil {
				return err
			}
			for _, class := range Classes {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO erasures (subject, class, date, created_at) VALUES (?,?,?,?)`, fi.tomb, class, date, ts(now)); err != nil {
					return err
				}
			}
			if err := s.audit(ctx, tx, actor, action, "file:"+fi.id, nil, "ok"); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, EventDeleted, deletedEvent(fi.id, fi.space, reason, actor, now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.wake()
	return len(infos), nil
}

// Erasure is the result of an erasure request: an opaque subject id and the classes purged.
type Erasure struct {
	Subject string   `json:"subject"`
	Classes []string `json:"classes"`
	Date    string   `json:"date"`
	Files   int      `json:"files"`
}

func (s *Store) subjectFileIDs(ctx context.Context, subject string, includeHeld bool) (ids []string, held int, err error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, legal_hold FROM files WHERE ((subject = ?) OR (id = ?)) AND (status != 'purged') ORDER BY id LIMIT 5000`, subject, subject)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var h int
		if err := rows.Scan(&id, &h); err != nil {
			return nil, 0, err
		}
		if h == 1 {
			held++
			if !includeHeld {
				continue
			}
		}
		ids = append(ids, id)
	}
	return ids, held, rows.Err()
}

// EraseSubject purges every file of a data subject on request and records tombstones the backup
// ledger takes. files.admin only; a legal hold on any of them blocks the request. Idempotent: a
// repeat retries the scrub of the database files, which is the last step.
func (s *Store) EraseSubject(ctx context.Context, p *Principal, subject string) (*Erasure, error) {
	if !p.Has(PermAdmin) {
		return nil, ErrForbidden
	}
	if !subjectRe.MatchString(subject) {
		return nil, invalid("the subject must be an opaque id")
	}
	ids, held, err := s.subjectFileIDs(ctx, subject, false)
	if err != nil {
		return nil, err
	}
	if held > 0 {
		s.auditDeny(ctx, p.Subject, "file.erase", "subject:"+subject)
		return nil, conflict("erasure is blocked by a legal hold")
	}
	n, err := s.purgeFiles(ctx, ids, p.Subject, "file.erase", "erasure")
	if err != nil {
		return nil, err
	}
	date := s.now().UTC().Format(time.DateOnly)
	if err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, c := range Classes {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO erasures (subject, class, date, created_at) VALUES (?,?,?,?)`, subject, c, date, ts(s.now())); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, p.Subject, "subject.erase", "subject:"+subject, nil, "ok")
	}); err != nil {
		return nil, err
	}
	// The erasure and its audit record are committed; now remove it from the file's free pages
	// and the write-ahead log. A failure is reported and the request can be repeated.
	if err := s.scrub(ctx, true); err != nil {
		return nil, fmt.Errorf("the files are erased and audited, but scrubbing the database files failed (repeat the request: it is idempotent and retries the scrub): %w", err)
	}
	return &Erasure{Subject: subject, Classes: Classes, Date: date, Files: n}, nil
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
}

// ListErasures returns tombstones after a cursor. files.admin only (the can-core service account).
func (s *Store) ListErasures(ctx context.Context, p *Principal, after int64) ([]ErasureEntry, error) {
	if !p.Has(PermAdmin) {
		return nil, ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT seq, subject, class, date FROM erasures WHERE (seq > ?) ORDER BY seq LIMIT 1000`, after)
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

// PurgeSubject is the purge(subject, class) hook the backup restore replays: idempotent, it
// ignores legal holds (the erasure was authorised when the tombstone was written), and an unknown
// subject is a no-op. The subject is a data subject id or a file id (retention tombstones use the
// file id of files that have no subject).
func (s *Store) PurgeSubject(ctx context.Context, p *Principal, subject, class string) error {
	if !p.Has(PermAdmin) {
		return ErrForbidden
	}
	if !subjectRe.MatchString(subject) {
		return invalid("subject must be an opaque id")
	}
	if class != ClassContent && class != ClassMetadata {
		return invalid("class must be one of %v", Classes)
	}
	ids, _, err := s.subjectFileIDs(ctx, subject, true)
	if err != nil {
		return err
	}
	if _, err := s.purgeFiles(ctx, ids, p.Subject, "file.purge", "erasure"); err != nil {
		return err
	}
	return s.scrub(ctx, false)
}

// RetentionReport counts what a sweep did.
type RetentionReport struct {
	Purged        int `json:"purged"`
	SkippedHold   int `json:"skipped_legal_hold"`
	SkippedLocked int `json:"skipped_retention_lock"`
	Orphans       int `json:"orphan_blobs_removed"`
}

// RunRetention enforces the retention rules: files in the trash for trash_days are purged (a hold
// or an unpassed retention date skips them), blobs that were written but never recorded are
// removed, and old outbox rows are dropped. Safe to run at any time and repeatedly.
func (s *Store) RunRetention(ctx context.Context) (*RetentionReport, error) {
	rep := &RetentionReport{}
	now := s.now()
	if d := s.cfg.Retention.TrashDays; d > 0 {
		cctx, cancel := context.WithTimeout(ctx, dbTimeout)
		rows, err := s.db.QueryContext(cctx, `SELECT id, legal_hold, retain_until FROM files WHERE (status = 'deleted') AND (deleted_at <= ?) ORDER BY deleted_at LIMIT 500`, ts(now.AddDate(0, 0, -d)))
		if err != nil {
			cancel()
			return rep, err
		}
		var due []string
		for rows.Next() {
			var id string
			var hold int
			var retain sql.NullInt64
			if err := rows.Scan(&id, &hold, &retain); err != nil {
				_ = rows.Close()
				cancel()
				return rep, err
			}
			switch {
			case hold == 1:
				rep.SkippedHold++
			case retain.Valid && retain.Int64 > ts(now):
				rep.SkippedLocked++
			default:
				due = append(due, id)
			}
		}
		_ = rows.Close()
		cancel()
		n, err := s.purgeFiles(ctx, due, "system", "file.retention_purge", "retention")
		rep.Purged = n
		if err != nil {
			return rep, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	rows, err := s.db.QueryContext(cctx, `SELECT blob_key FROM pending_blobs WHERE (created_at <= ?) LIMIT 500`, ts(now.Add(-pendingBlobGrace)))
	if err != nil {
		cancel()
		return rep, err
	}
	var orphans []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			cancel()
			return rep, err
		}
		orphans = append(orphans, k)
	}
	_ = rows.Close()
	cancel()
	for _, k := range orphans {
		if err := s.backend.Delete(ctx, k); err != nil {
			return rep, err
		}
		if err := s.inTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM pending_blobs WHERE (blob_key = ?)`, k)
			return err
		}); err != nil {
			return rep, err
		}
		rep.Orphans++
	}
	if err := s.inTx(ctx, func(tx *sql.Tx) error { return s.purgeOutbox(ctx, tx, now) }); err != nil {
		return rep, err
	}
	if rep.Purged > 0 {
		if err := s.scrub(ctx, true); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

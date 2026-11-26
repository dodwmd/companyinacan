package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// UploadRequest is a new file. Body is streamed: it is never held in memory.
type UploadRequest struct {
	SpaceID     string
	FolderID    string
	Name        string
	Description string
	Subject     string // opaque id of the data subject (a candidate, a person), for erasure
	Ref         string // opaque reference of the owning object, "<module>:<id>"
	Body        io.Reader
	Size        int64 // the declared length, when known (0 or less: unknown)
}

// ---- reading rows -------------------------------------------------------------------------------

const fileCols = `f.id, f.space_id, f.folder_id, f.name_sealed, f.description_sealed, f.ext, f.subject, f.ref, f.status, f.current_version, f.rev,
	f.legal_hold, f.retain_until, f.deleted_at, f.created_by, f.created_at, f.updated_at, v.size, v.content_type, v.sha256, v.scan, v.created_by`

const fileFrom = `FROM files f LEFT JOIN versions v ON (v.file_id = f.id) AND (v.n = f.current_version)`

type fileRow struct {
	id, spaceID, ext, subject, ref, status, createdBy string
	folder, nameS, descS, ct, sha, scan, vby          sql.NullString
	cur, rev, hold                                    int
	retain, deleted                                   sql.NullInt64
	created, updated                                  int64
	size                                              sql.NullInt64
}

type scanner interface{ Scan(dest ...any) error }

func scanFileRow(r scanner) (*fileRow, error) {
	var f fileRow
	err := r.Scan(&f.id, &f.spaceID, &f.folder, &f.nameS, &f.descS, &f.ext, &f.subject, &f.ref, &f.status, &f.cur, &f.rev,
		&f.hold, &f.retain, &f.deleted, &f.createdBy, &f.created, &f.updated, &f.size, &f.ct, &f.sha, &f.scan, &f.vby)
	return &f, err
}

// getFile loads a file the caller may see at the given minimum role. No access (the space is not
// theirs, the file is purged, or it is in the trash and they are not an editor) is ErrNotFound,
// the same value a missing id gives.
func (s *Store) getFile(ctx context.Context, q dbq, p *Principal, id string, min Role) (*fileRow, Role, error) {
	if !validID("fil", id) {
		return nil, RoleNone, ErrNotFound
	}
	f, err := scanFileRow(q.QueryRowContext(ctx, `SELECT `+fileCols+` `+fileFrom+` WHERE (f.id = ?)`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, RoleNone, ErrNotFound
	}
	if err != nil {
		return nil, RoleNone, err
	}
	role, err := s.roleIn(ctx, q, f.spaceID, p)
	if err != nil {
		return nil, RoleNone, err
	}
	if role == RoleNone || f.status == "purged" || f.status == "purging" || (f.status == "deleted" && role < RoleEditor) {
		return nil, RoleNone, ErrNotFound
	}
	if role < min {
		return f, role, ErrForbidden
	}
	return f, role, nil
}

// view renders a file for a role: higher roles see more fields.
func (s *Store) view(f *fileRow, role Role) (*FileView, error) {
	name, err := s.unseal("files", f.id, "name_sealed", f.nameS)
	if err != nil {
		return nil, err
	}
	desc, err := s.unseal("files", f.id, "description_sealed", f.descS)
	if err != nil {
		return nil, err
	}
	v := &FileView{ID: f.id, SpaceID: f.spaceID, FolderID: f.folder.String, Name: name, Description: desc, ContentType: f.ct.String,
		Size: f.size.Int64, Version: f.cur, Status: f.status, Rev: f.rev, CreatedAt: fromUnix(f.created), UpdatedAt: fromUnix(f.updated), Role: role.String()}
	if role >= RoleEditor {
		v.SHA256, v.Scan, v.CreatedBy, v.DeletedAt = f.sha.String, f.scan.String, f.vby.String, nullTime(f.deleted)
	}
	if role >= RoleManager {
		v.Ref, v.Subject, v.RetainUntil = f.ref, f.subject, nullTime(f.retain)
		h := f.hold == 1
		v.LegalHold = &h
	}
	return v, nil
}

// ---- upload --------------------------------------------------------------------------------------

type staged struct {
	path string
	size int64
	sha  string
	ct   string
}

func (st *staged) remove() { _ = os.Remove(st.path) }

// stage reads the body into a private temporary file while hashing it, after checking the type
// from the first bytes and before a byte more than the limit is accepted. Nothing reaches the
// backend yet: the scanner and the database checks come first.
func (s *Store) stage(ctx context.Context, body io.Reader, name string) (*staged, error) {
	max := s.cfg.Limits.MaxFileBytes
	lr := io.LimitReader(body, max+1)
	head := make([]byte, SniffSize)
	n, err := io.ReadFull(lr, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, invalid("the upload could not be read")
	}
	head = head[:n]
	if n == 0 {
		return nil, invalid("the file is empty")
	}
	ct, err := DetectType(name, head, s.cfg.Limits.AllowedTypes)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(s.stageDir, "up-*")
	if err != nil {
		return nil, fmt.Errorf("%w: staging", ErrUnavailable)
	}
	st := &staged{path: f.Name(), ct: ct}
	fail := func(err error) (*staged, error) {
		_ = f.Close()
		st.remove()
		return nil, err
	}
	h := sha256.New()
	// One io.Copy, from the head already read and then the rest, into the private file and the hash.
	total, err := io.Copy(io.MultiWriter(f, h), &ctxReader{ctx: ctx, r: io.MultiReader(bytes.NewReader(head), lr)})
	if err != nil {
		return fail(invalid("the upload was interrupted"))
	}
	if total > max {
		return fail(ErrTooLarge)
	}
	if err := f.Close(); err != nil {
		st.remove()
		return nil, fmt.Errorf("%w: staging", ErrUnavailable)
	}
	st.size, st.sha = total, hex.EncodeToString(h.Sum(nil))
	return st, nil
}

// uploadSlot reserves one of the bounded upload slots for subject without waiting; call the
// returned function to give it back. One subject can hold only its share of them.
func (s *Store) uploadSlot(subject string) (func(), error) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if s.uploadsBy[subject] >= s.cfg.Limits.MaxConcurrentUploadsPerSubject {
		return nil, fmt.Errorf("%w: you have too many uploads in progress, retry shortly", ErrUnavailable)
	}
	select {
	case s.uploads <- struct{}{}:
	default:
		return nil, fmt.Errorf("%w: too many uploads in progress, retry shortly", ErrUnavailable)
	}
	s.uploadsBy[subject]++
	return func() {
		s.uploadMu.Lock()
		defer s.uploadMu.Unlock()
		if s.uploadsBy[subject]--; s.uploadsBy[subject] <= 0 {
			delete(s.uploadsBy, subject)
		}
		<-s.uploads
	}, nil
}

// checkRoom refuses, before a byte is read or staged, an upload the space owner's quota cannot
// take. The transaction that records the file checks again with the real size.
func (s *Store) checkRoom(ctx context.Context, spaceID string, declared int64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var owner string
	if err := s.db.QueryRowContext(ctx, `SELECT owner FROM spaces WHERE (id = ?)`, spaceID).Scan(&owner); err != nil {
		return err
	}
	used, err := s.ownerUsage(ctx, s.db, owner)
	if err != nil {
		return err
	}
	return checkQuota(used, max(declared, 1), s.cfg.Limits.QuotaBytesPerOwner)
}

func (s *Store) scannerReady() error {
	if _, noop := s.scanner.(NoopScanner); noop && s.cfg.ScannerRequired() {
		return fmt.Errorf("%w: no virus scanner is configured, so uploads are closed (scanner.required)", ErrUnavailable)
	}
	return nil
}

// scanStaged runs the scanner on the staged bytes. Only a clean or (when scanning is optional) a
// skipped verdict continues; infected is ErrInfected; anything else is "could not scan", refused.
func (s *Store) scanStaged(ctx context.Context, st *staged) (ScanVerdict, error) {
	f, err := os.Open(st.path)
	if err != nil {
		return "", fmt.Errorf("%w: staging", ErrUnavailable)
	}
	defer func() { _ = f.Close() }()
	v, err := s.scanner.Scan(ctx, f)
	if err != nil {
		return "", fmt.Errorf("%w: the file could not be scanned", ErrUnavailable)
	}
	switch v {
	case ScanClean:
		return v, nil
	case ScanSkipped:
		if s.cfg.ScannerRequired() {
			return "", fmt.Errorf("%w: the file could not be scanned", ErrUnavailable)
		}
		return v, nil
	case ScanInfected:
		return v, ErrInfected
	}
	return "", fmt.Errorf("%w: the file could not be scanned", ErrUnavailable)
}

// commitBlob writes the staged file to the backend under a fresh random key. The key is
// registered first, so a crash between the write and the metadata leaves a row for the sweep.
func (s *Store) commitBlob(ctx context.Context, st *staged) (string, error) {
	key := newBlobKey()
	if err := s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO pending_blobs (blob_key, created_at) VALUES (?,?)`, key, ts(s.now()))
		return err
	}); err != nil {
		return "", err
	}
	f, err := os.Open(st.path)
	if err == nil {
		err = s.backend.Put(ctx, key, f, st.size, st.sha)
		_ = f.Close()
	}
	if err != nil {
		s.discardBlob(key)
		return "", fmt.Errorf("%w: the file could not be stored", ErrUnavailable)
	}
	return key, nil
}

// discardBlob removes a blob whose metadata was not recorded. Best effort; the sweep is the net.
func (s *Store) discardBlob(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := s.backend.Delete(ctx, key); err == nil {
		_ = s.inTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM pending_blobs WHERE (blob_key = ?)`, key)
			return err
		})
	}
}

func (s *Store) ownerUsage(ctx context.Context, q dbq, owner string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(v.size), 0) FROM versions v JOIN files f ON (f.id = v.file_id) JOIN spaces sp ON (sp.id = f.space_id)
		WHERE (sp.owner = ?) AND (v.purged_at IS NULL)`, owner).Scan(&n)
	return n, err
}

func checkQuota(used, add, limit int64) error {
	if used+add > limit {
		return fmt.Errorf("%w: the space owner's storage quota would be exceeded", ErrQuota)
	}
	return nil
}

// Upload stores a new file. Editors and managers of the space. The checks run in the order that
// costs least: permission and role, name and type from the first bytes, size while streaming,
// the scanner, then the transaction that re-checks quota and counts and records the file, its
// first version, the audit entry and the event together.
func (s *Store) Upload(ctx context.Context, p *Principal, req UploadRequest) (*FileView, error) {
	if !p.Has(PermWrite) {
		return nil, ErrForbidden
	}
	if !validID("spc", req.SpaceID) {
		return nil, ErrNotFound
	}
	name := SafeFilename(req.Name)
	if len(req.Name) > maxNameBytes || name == "" {
		return nil, invalid("the file needs a name of 1 to %d bytes", maxNameBytes)
	}
	desc, err := cleanText(req.Description, maxDescBytes)
	if err != nil {
		return nil, err
	}
	if (req.Subject != "" && !subjectRe.MatchString(req.Subject)) || (req.Ref != "" && !refRe.MatchString(req.Ref)) ||
		(req.FolderID != "" && !validID("fld", req.FolderID)) {
		return nil, invalid("subject, ref or folder is malformed")
	}
	if !slices.Contains(s.cfg.Limits.AllowedTypes, extOf(name)) {
		return nil, fmt.Errorf("%w: the extension is not allowed", ErrType)
	}
	if err := s.scannerReady(); err != nil {
		return nil, err
	}
	if err := s.checkWritable(ctx, p, req.SpaceID, "file.upload"); err != nil {
		return nil, err
	}
	if err := s.checkRoom(ctx, req.SpaceID, req.Size); err != nil {
		return nil, err
	}
	release, err := s.uploadSlot(p.Subject)
	if err != nil {
		return nil, err
	}
	defer release()
	st, err := s.stage(ctx, req.Body, name)
	if err != nil {
		return nil, err
	}
	defer st.remove()
	verdict, err := s.scanStaged(ctx, st)
	if errors.Is(err, ErrInfected) {
		s.recordInfected(ctx, p, req.SpaceID, "", st)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	key, err := s.commitBlob(ctx, st)
	if err != nil {
		return nil, err
	}
	id, now := newID("fil"), s.now()
	nameS, err := s.seal("files", id, "name_sealed", name)
	if err != nil {
		s.discardBlob(key)
		return nil, err
	}
	descS, err := s.seal("files", id, "description_sealed", desc)
	if err != nil {
		s.discardBlob(key)
		return nil, err
	}
	var view *FileView
	err = s.txFor(ctx, p, "file.upload", "space:"+req.SpaceID, func(tx *sql.Tx) error {
		role, err := s.need(ctx, tx, p, req.SpaceID, RoleEditor, "file.upload")
		if err != nil {
			return err
		}
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT owner FROM spaces WHERE (id = ?)`, req.SpaceID).Scan(&owner); err != nil {
			return err
		}
		var folder any
		if req.FolderID != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE (id = ?) AND (space_id = ?)`, req.FolderID, req.SpaceID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return invalid("the folder is not in this space")
			}
			folder = req.FolderID
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE (space_id = ?) AND (status != 'purged')`, req.SpaceID).Scan(&count); err != nil {
			return err
		}
		if count >= s.cfg.Limits.MaxFilesPerSpace {
			return fmt.Errorf("%w: the space holds the most files it may", ErrQuota)
		}
		used, err := s.ownerUsage(ctx, tx, owner)
		if err != nil {
			return err
		}
		if err := checkQuota(used, st.size, s.cfg.Limits.QuotaBytesPerOwner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO files (id, space_id, folder_id, name_sealed, description_sealed, ext, subject, ref, status, current_version, rev, created_by, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?, 'active', 1, 1, ?,?,?)`,
			id, req.SpaceID, folder, nameS.String, nullStr(descS), extOf(name), req.Subject, req.Ref, p.Subject, ts(now), ts(now)); err != nil {
			return err
		}
		if err := insertVersion(ctx, tx, id, 1, key, st, verdict, p.Subject, now); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, p.Subject, "file.upload", "file:"+id, []string{"name", "version:1"}, "ok"); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventUploaded, uploadedEvent(id, req.SpaceID, 1, st, p.Subject, req.Ref, now)); err != nil {
			return err
		}
		fr, err := scanFileRow(tx.QueryRowContext(ctx, `SELECT `+fileCols+` `+fileFrom+` WHERE (f.id = ?)`, id))
		if err != nil {
			return err
		}
		view, err = s.view(fr, role)
		return err
	})
	if err != nil {
		s.discardBlob(key)
		return nil, err
	}
	s.wake()
	return view, nil
}

func nullStr(n sql.NullString) any {
	if !n.Valid {
		return nil
	}
	return n.String
}

func insertVersion(ctx context.Context, tx dbq, fileID string, n int, key string, st *staged, v ScanVerdict, actor string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO versions (file_id, n, blob_key, size, sha256, content_type, scan, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		fileID, n, key, st.size, st.sha, st.ct, string(v), actor, ts(now)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM pending_blobs WHERE (blob_key = ?)`, key)
	return err
}

// checkWritable fails early (before the body is read) when the caller cannot write to the space.
func (s *Store) checkWritable(ctx context.Context, p *Principal, spaceID, action string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	_, err := s.need(ctx, s.db, p, spaceID, RoleEditor, action)
	return s.forbid(ctx, p, action, "space:"+spaceID, err)
}

// UploadVersion adds an immutable version to a file. Editors and managers. The type must match
// the file's extension like any upload.
func (s *Store) UploadVersion(ctx context.Context, p *Principal, fileID string, body io.Reader, declared ...int64) (*FileView, error) {
	if !p.Has(PermWrite) {
		return nil, ErrForbidden
	}
	if err := s.scannerReady(); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	f, _, err := s.getFile(cctx, s.db, p, fileID, RoleEditor)
	cancel()
	if err != nil {
		return nil, s.forbid(ctx, p, "file.version", "file:"+fileID, err)
	}
	if f.status != "active" {
		return nil, conflict("restore the file before adding a version")
	}
	var size int64
	if len(declared) > 0 {
		size = declared[0]
	}
	if err := s.checkRoom(ctx, f.spaceID, size); err != nil {
		return nil, err
	}
	release, err := s.uploadSlot(p.Subject)
	if err != nil {
		return nil, err
	}
	defer release()
	st, err := s.stage(ctx, body, "x."+f.ext)
	if err != nil {
		return nil, err
	}
	defer st.remove()
	verdict, err := s.scanStaged(ctx, st)
	if errors.Is(err, ErrInfected) {
		s.recordInfected(ctx, p, f.spaceID, fileID, st)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	key, err := s.commitBlob(ctx, st)
	if err != nil {
		return nil, err
	}
	var view *FileView
	err = s.txFor(ctx, p, "file.version", "file:"+fileID, func(tx *sql.Tx) error {
		cur, role, err := s.getFile(ctx, tx, p, fileID, RoleEditor)
		if err != nil {
			return err
		}
		if cur.status != "active" {
			return conflict("restore the file before adding a version")
		}
		if cur.cur >= s.cfg.Limits.MaxVersionsPerFile {
			return conflict("the file has the most versions it may")
		}
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT owner FROM spaces WHERE (id = ?)`, cur.spaceID).Scan(&owner); err != nil {
			return err
		}
		used, err := s.ownerUsage(ctx, tx, owner)
		if err != nil {
			return err
		}
		if err := checkQuota(used, st.size, s.cfg.Limits.QuotaBytesPerOwner); err != nil {
			return err
		}
		n, now := cur.cur+1, s.now()
		if err := insertVersion(ctx, tx, fileID, n, key, st, verdict, p.Subject, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET current_version = ?, rev = rev + 1, updated_at = ? WHERE (id = ?)`, n, ts(now), fileID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, p.Subject, "file.version", "file:"+fileID, []string{"version:" + strconv.Itoa(n)}, "ok"); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventUploaded, uploadedEvent(fileID, cur.spaceID, n, st, p.Subject, cur.ref, now)); err != nil {
			return err
		}
		fr, err := scanFileRow(tx.QueryRowContext(ctx, `SELECT `+fileCols+` `+fileFrom+` WHERE (f.id = ?)`, fileID))
		if err != nil {
			return err
		}
		view, err = s.view(fr, role)
		return err
	})
	if err != nil {
		s.discardBlob(key)
		return nil, err
	}
	s.wake()
	return view, nil
}

// recordInfected audits a detection and queues file.infected.v1, then leaves nothing behind.
func (s *Store) recordInfected(ctx context.Context, p *Principal, spaceID, fileID string, st *staged) {
	_ = s.inTx(ctx, func(tx *sql.Tx) error {
		res := "space:" + spaceID
		if fileID != "" {
			res = "file:" + fileID
		}
		if err := s.audit(ctx, tx, p.Subject, "file.infected", res, nil, "refused"); err != nil {
			return err
		}
		return s.emit(ctx, tx, EventInfected, infectedEvent(spaceID, fileID, st, p.Subject, s.now()))
	})
	s.wake()
}

// ---- reading -------------------------------------------------------------------------------------

// GetFile returns a file's metadata as the caller's role allows. The read is audited first.
func (s *Store) GetFile(ctx context.Context, p *Principal, id string) (*FileView, error) {
	if !p.Has(PermRead) {
		return nil, ErrForbidden
	}
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	f, role, err := s.getFile(cctx, s.db, p, id, RoleViewer)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, p.Subject, "file.read", "file:"+id); err != nil {
		return nil, err
	}
	return s.view(f, role)
}

// ListQuery filters ListFiles. With no SpaceID the list spans every space the caller can see.
type ListQuery struct {
	SpaceID  string
	FolderID string
	Ref      string
	Cursor   string
	Limit    int
	Deleted  bool // the trash of one space: editors and managers
}

// ListFiles lists files the caller can see, in id order with a keyset cursor. Every clause is
// parenthesised: the visibility test is ANDed with the filters and cannot be bypassed by one.
func (s *Store) ListFiles(ctx context.Context, p *Principal, q ListQuery) (Page[FileView], error) {
	out := Page[FileView]{Items: []FileView{}}
	if !p.Has(PermRead) {
		return out, ErrForbidden
	}
	if (q.SpaceID != "" && !validID("spc", q.SpaceID)) || (q.FolderID != "" && !validID("fld", q.FolderID)) || (q.Cursor != "" && !validID("fil", q.Cursor)) ||
		(q.Ref != "" && !refRe.MatchString(q.Ref)) {
		return out, invalid("a filter or the cursor is malformed")
	}
	if q.Deleted && q.SpaceID == "" {
		return out, invalid("deleted needs a space")
	}
	limit := clampLimit(q.Limit)
	keys := p.memberKeys()
	minRole, status := 1, "active"
	if q.Deleted {
		minRole, status = 2, "deleted"
	}
	args := anyArgs(keys)
	where := `(m.r >= ?) AND (f.status = ?) AND (f.id > ?)`
	args = append(args, minRole, status, q.Cursor)
	if q.SpaceID != "" {
		where += ` AND (f.space_id = ?)`
		args = append(args, q.SpaceID)
	}
	if q.FolderID != "" {
		where += ` AND (f.folder_id = ?)`
		args = append(args, q.FolderID)
	}
	if q.Ref != "" {
		where += ` AND (f.ref = ?)`
		args = append(args, q.Ref)
	}
	args = append(args, limit+1)
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if q.SpaceID != "" {
		min := RoleViewer
		if q.Deleted {
			min = RoleEditor
		}
		if _, err := s.need(cctx, s.db, p, q.SpaceID, min, "file.list"); err != nil {
			return out, s.forbid(ctx, p, "file.list", "space:"+q.SpaceID, err)
		}
	}
	listed := "files"
	if q.SpaceID != "" {
		listed = "space:" + q.SpaceID
	}
	if err := s.auditRead(cctx, p.Subject, "file.list", listed); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(cctx, `SELECT `+fileCols+`, m.r FROM files f
		JOIN (SELECT space_id, MAX(CASE role WHEN 'manager' THEN 3 WHEN 'editor' THEN 2 ELSE 1 END) AS r FROM space_members WHERE (principal IN (`+placeholders(len(keys))+`)) GROUP BY space_id) m ON (m.space_id = f.space_id)
		LEFT JOIN versions v ON (v.file_id = f.id) AND (v.n = f.current_version)
		WHERE `+where+` ORDER BY f.id LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	type item struct {
		f    *fileRow
		role int
	}
	var items []item
	for rows.Next() {
		var f fileRow
		var role int
		if err := rows.Scan(&f.id, &f.spaceID, &f.folder, &f.nameS, &f.descS, &f.ext, &f.subject, &f.ref, &f.status, &f.cur, &f.rev,
			&f.hold, &f.retain, &f.deleted, &f.createdBy, &f.created, &f.updated, &f.size, &f.ct, &f.sha, &f.scan, &f.vby, &role); err != nil {
			_ = rows.Close()
			return out, err
		}
		items = append(items, item{&f, role})
	}
	_ = rows.Close()
	if len(items) > limit {
		items = items[:limit]
		out.Next = items[limit-1].f.id
	}
	for _, it := range items {
		v, err := s.view(it.f, Role(it.role))
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, *v)
	}
	return out, nil
}

// ListVersions lists a file's versions, newest first.
func (s *Store) ListVersions(ctx context.Context, p *Principal, id string) ([]VersionView, error) {
	if !p.Has(PermRead) {
		return nil, ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	_, role, err := s.getFile(ctx, s.db, p, id, RoleViewer)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, p.Subject, "file.versions", "file:"+id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT n, size, content_type, sha256, scan, created_by, created_at, purged_at FROM versions WHERE (file_id = ?) ORDER BY n DESC LIMIT 1000`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []VersionView{}
	for rows.Next() {
		var v VersionView
		var created int64
		var purged sql.NullInt64
		if err := rows.Scan(&v.N, &v.Size, &v.ContentType, &v.SHA256, &v.Scan, &v.CreatedBy, &created, &purged); err != nil {
			return nil, err
		}
		v.CreatedAt, v.Purged = fromUnix(created), purged.Valid
		if role < RoleEditor {
			v.SHA256, v.Scan, v.CreatedBy = "", "", ""
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Content is an opened download. Body is the stored object; Stream copies it out verified.
type Content struct {
	FileID      string
	Version     int
	Name        string // sanitised
	ContentType string // always application/octet-stream
	StoredType  string // the detected type, used only when inline display is enabled for it
	Ext         string
	Size        int64
	SHA256      string
	Body        io.ReadCloser
	InlineOK    bool // the operator enabled inline display for this type

	store *Store
	actor string
}

// Stream writes the content to w, holding back the final chunk until the checksum has been
// verified (VerifiedCopy). A mismatch is audited and returned as ErrIntegrity; the caller must
// then abort the response rather than end it normally.
func (c *Content) Stream(ctx context.Context, w io.Writer) error {
	err := VerifiedCopy(w, c.Body, c.Size, c.SHA256)
	if errors.Is(err, ErrIntegrity) && c.store != nil {
		_ = c.store.inTx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
			return c.store.audit(ctx, tx, c.actor, "file.integrity_failed", "file:"+c.FileID, []string{"version:" + strconv.Itoa(c.Version)}, "failed")
		})
	}
	return err
}

// openVersion resolves a version and opens its object. ctx is the request's context and must live as
// long as the returned Body is read: only the database query is bounded here, because a backend's
// reader (S3) dies with the context it was opened with.
func (s *Store) openVersion(ctx context.Context, f *fileRow, version int) (*Content, error) {
	var key string
	c := &Content{FileID: f.id, store: s, ContentType: "application/octet-stream", Ext: f.ext}
	var purged sql.NullInt64
	var scan string
	qctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.db.QueryRowContext(qctx, `SELECT n, blob_key, size, sha256, content_type, purged_at, scan FROM versions WHERE (file_id = ?) AND (n = CASE WHEN ? = 0 THEN ? ELSE ? END)`,
		f.id, version, f.cur, version).Scan(&c.Version, &key, &c.Size, &c.SHA256, &c.StoredType, &purged, &scan)
	if errors.Is(err, sql.ErrNoRows) || purged.Valid {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if s.cfg.ScannerRequired() && scan != string(ScanClean) {
		return nil, conflict("this version was never scanned and scanning is now required")
	}
	name, err := s.unseal("files", f.id, "name_sealed", f.nameS)
	if err != nil {
		return nil, err
	}
	c.Name = name
	c.InlineOK = s.cfg.Download.Inline && slices.Contains(s.cfg.Download.InlineTypes, f.ext)
	body, err := s.backend.Open(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: the stored object is missing", ErrIntegrity)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: storage", ErrUnavailable)
	}
	c.Body = body
	return c, nil
}

// OpenContent opens a version (0 = the current one) for download. The caller must be a viewer or
// stronger, and the access is audited before the content is opened, failing closed: when the
// audit entry cannot be written nothing is returned.
func (s *Store) OpenContent(ctx context.Context, p *Principal, id string, version int) (*Content, error) {
	if !p.Has(PermRead) {
		return nil, ErrForbidden
	}
	if version < 0 || version > 1_000_000 {
		return nil, ErrNotFound
	}
	cctx, cancel := context.WithTimeout(ctx, dbTimeout)
	f, _, err := s.getFile(cctx, s.db, p, id, RoleViewer)
	cancel()
	if err != nil {
		return nil, err
	}
	if f.status != "active" {
		return nil, ErrNotFound
	}
	if err := s.auditRead(ctx, p.Subject, "file.download", "file:"+id); err != nil {
		return nil, err
	}
	c, err := s.openVersion(ctx, f, version)
	if err != nil {
		return nil, err
	}
	c.actor = p.Subject
	return c, nil
}

// ---- changing ------------------------------------------------------------------------------------

// FilePatch changes a file's metadata. Nil fields are left alone. Rev, when not 0, must match the
// file's current rev (optimistic concurrency).
type FilePatch struct {
	Name        *string
	Description *string
	FolderID    *string // "" moves the file to the top level
	Rev         int
}

// UpdateFile renames, describes or moves a file. Editors and managers. The extension cannot
// change: the type was checked against the content when it was uploaded.
func (s *Store) UpdateFile(ctx context.Context, p *Principal, id string, patch FilePatch) (*FileView, error) {
	if !p.Has(PermWrite) {
		return nil, ErrForbidden
	}
	var name, desc string
	if patch.Name != nil {
		name = SafeFilename(*patch.Name)
		if name == "" || len(*patch.Name) > maxNameBytes {
			return nil, invalid("the file needs a name of 1 to %d bytes", maxNameBytes)
		}
	}
	if patch.Description != nil {
		var err error
		if desc, err = cleanText(*patch.Description, maxDescBytes); err != nil {
			return nil, err
		}
	}
	if patch.FolderID != nil && *patch.FolderID != "" && !validID("fld", *patch.FolderID) {
		return nil, invalid("bad folder")
	}
	var view *FileView
	err := s.txFor(ctx, p, "file.update", "file:"+id, func(tx *sql.Tx) error {
		f, role, err := s.getFile(ctx, tx, p, id, RoleEditor)
		if err != nil {
			return err
		}
		if f.status != "active" {
			return conflict("restore the file first")
		}
		if patch.Rev != 0 && patch.Rev != f.rev {
			return conflict("the file changed since it was read")
		}
		var sets []string
		var args []any
		var fields []string
		if patch.Name != nil {
			if extOf(name) != f.ext {
				return invalid("the extension cannot change (.%s)", f.ext)
			}
			sealed, err := s.seal("files", id, "name_sealed", name)
			if err != nil {
				return err
			}
			sets, args, fields = append(sets, "name_sealed = ?"), append(args, sealed.String), append(fields, "name")
		}
		if patch.Description != nil {
			sealed, err := s.seal("files", id, "description_sealed", desc)
			if err != nil {
				return err
			}
			sets, args, fields = append(sets, "description_sealed = ?"), append(args, nullStr(sealed)), append(fields, "description")
		}
		if patch.FolderID != nil {
			var folder any
			if *patch.FolderID != "" {
				var n int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE (id = ?) AND (space_id = ?)`, *patch.FolderID, f.spaceID).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					return invalid("the folder is not in this space")
				}
				folder = *patch.FolderID
			}
			sets, args, fields = append(sets, "folder_id = ?"), append(args, folder), append(fields, "folder")
		}
		if len(sets) == 0 {
			return invalid("nothing to change")
		}
		args = append(args, ts(s.now()), id)
		if _, err := tx.ExecContext(ctx, `UPDATE files SET `+strings.Join(sets, ", ")+`, rev = rev + 1, updated_at = ? WHERE (id = ?)`, args...); err != nil { //nolint:gosec // fixed column list
			return err
		}
		if err := s.audit(ctx, tx, p.Subject, "file.update", "file:"+id, fields, "ok"); err != nil {
			return err
		}
		fr, err := scanFileRow(tx.QueryRowContext(ctx, `SELECT `+fileCols+` `+fileFrom+` WHERE (f.id = ?)`, id))
		if err != nil {
			return err
		}
		view, err = s.view(fr, role)
		return err
	})
	return view, err
}

// DeleteFile moves a file to the trash. Editors and managers. A legal hold or a retention date
// that has not passed blocks it. Its live share links are revoked. Nothing is destroyed here: the retention sweep purges after
// trash_days.
func (s *Store) DeleteFile(ctx context.Context, p *Principal, id string) error {
	if !p.Has(PermWrite) {
		return ErrForbidden
	}
	err := s.txFor(ctx, p, "file.delete", "file:"+id, func(tx *sql.Tx) error {
		f, _, err := s.getFile(ctx, tx, p, id, RoleEditor)
		if err != nil {
			return err
		}
		if f.status == "deleted" {
			return nil
		}
		if err := s.deletable(f); err != nil {
			return err
		}
		now := ts(s.now())
		if _, err := tx.ExecContext(ctx, `UPDATE files SET status = 'deleted', deleted_at = ?, rev = rev + 1, updated_at = ? WHERE (id = ?)`, now, now, id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, p.Subject, "file.delete", "file:"+id, nil, "ok"); err != nil {
			return err
		}
		// Links end with the file, for good: restoring it does not bring them back.
		if err := s.revokeFileLinks(ctx, tx, p.Subject, id, "file_deleted"); err != nil {
			return err
		}
		return s.emit(ctx, tx, EventDeleted, deletedEvent(id, f.spaceID, "user", p.Subject, s.now()))
	})
	if err == nil {
		s.wake()
	}
	return err
}

// deletable enforces the hold and the retention lock.
func (s *Store) deletable(f *fileRow) error {
	if f.hold == 1 {
		return conflict("the file is under a legal hold")
	}
	if f.retain.Valid && f.retain.Int64 > ts(s.now()) {
		return conflict("the file must be kept until %s", fromUnix(f.retain.Int64).Format(time.DateOnly))
	}
	return nil
}

// RestoreFile takes a file out of the trash. Editors and managers.
func (s *Store) RestoreFile(ctx context.Context, p *Principal, id string) error {
	if !p.Has(PermWrite) {
		return ErrForbidden
	}
	return s.txFor(ctx, p, "file.restore", "file:"+id, func(tx *sql.Tx) error {
		f, _, err := s.getFile(ctx, tx, p, id, RoleEditor)
		if err != nil {
			return err
		}
		if f.status != "deleted" {
			return conflict("the file is not in the trash")
		}
		// A file trashed before deletion revoked links may still have live ones.
		if err := s.revokeFileLinks(ctx, tx, p.Subject, id, "file_deleted"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files SET status = 'active', deleted_at = NULL, rev = rev + 1, updated_at = ? WHERE (id = ?)`, ts(s.now()), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "file.restore", "file:"+id, nil, "ok")
	})
}

package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// writeRow loads an issue the caller may change.
func (s *Store) writeRow(ctx context.Context, q dbq, a *Principal, key string) (*issueRow, error) {
	row, err := s.loadIssue(ctx, q, a, key)
	if err != nil {
		return nil, err
	}
	if row.archived {
		return nil, fmt.Errorf("%w: project %s is archived", ErrConflict, row.Project)
	}
	return row, nil
}

// ---- comments -------------------------------------------------------------------------

// AddComment adds a comment; the author starts watching the issue.
func (s *Store) AddComment(ctx context.Context, a *Principal, key, body string) (*Comment, error) {
	if strings.TrimSpace(body) == "" || !validText(body, maxComment) {
		return nil, invalid("body", "required, at most %d characters", maxComment)
	}
	var out *Comment
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		row, err := s.writeRow(ctx, tx, a, key)
		if err != nil {
			return err
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `INSERT INTO comments (issue_id, author, body, created_at) VALUES (?, ?, ?, ?)`, row.id, a.Subject, body, ts(now))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO watchers (issue_id, subject) VALUES (?, ?)`, row.id, a.Subject); err != nil {
			return err
		}
		if err := s.addActivity(ctx, tx, row.id, a.Subject, "commented", map[string]int64{"comment_id": id}); err != nil {
			return err
		}
		out = &Comment{ID: id, Issue: row.Key, Author: a.Subject, Body: body, CreatedAt: now.UTC().Truncate(1e9)}
		return s.emit(ctx, tx, EventCommentCreated, commentCreatedData{IssueKey: row.Key, Project: row.Project, CommentID: id, Author: a.Subject, CreatedAt: out.CreatedAt})
	})
	return out, err
}

// ListComments returns comments oldest first, after the given comment id.
func (s *Store) ListComments(ctx context.Context, a *Principal, key string, after int64, limit int) ([]Comment, int64, error) {
	row, err := s.loadIssue(ctx, s.db, a, key)
	if err != nil {
		return nil, 0, err
	}
	limit = clampLimit(limit)
	rs, err := s.db.QueryContext(ctx, `SELECT id, author, body, created_at FROM comments WHERE issue_id = ? AND id > ? ORDER BY id LIMIT ?`, row.id, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rs.Close() }()
	out := []Comment{}
	for rs.Next() {
		var c Comment
		var at int64
		if err := rs.Scan(&c.ID, &c.Author, &c.Body, &at); err != nil {
			return nil, 0, err
		}
		c.Issue, c.CreatedAt = row.Key, fromUnix(at)
		out = append(out, c)
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, rs.Err()
}

// ---- activity -------------------------------------------------------------------------

// ListActivity returns an issue's history oldest first, after the given entry id.
func (s *Store) ListActivity(ctx context.Context, a *Principal, key string, after int64, limit int) ([]Activity, int64, error) {
	row, err := s.loadIssue(ctx, s.db, a, key)
	if err != nil {
		return nil, 0, err
	}
	limit = clampLimit(limit)
	rs, err := s.db.QueryContext(ctx, `SELECT id, actor, kind, data, at FROM activity WHERE issue_id = ? AND id > ? ORDER BY id LIMIT ?`, row.id, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rs.Close() }()
	out := []Activity{}
	for rs.Next() {
		var e Activity
		var d string
		var at int64
		if err := rs.Scan(&e.ID, &e.Actor, &e.Kind, &d, &at); err != nil {
			return nil, 0, err
		}
		e.Data, e.At = json.RawMessage(d), fromUnix(at)
		if e.Kind == "linked" && !a.Unscoped() {
			e.Data = redactLinkActivity(a, e.Data)
		}
		out = append(out, e)
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, rs.Err()
}

// ---- links ----------------------------------------------------------------------------

func (s *Store) linksFor(ctx context.Context, q dbq, a *Principal, issueID int64) ([]Link, error) {
	rs, err := q.QueryContext(ctx, `
		SELECT k.id, k.type, 'outbound', p.key || '-' || t.number, t.title, t.status
		  FROM issue_links k JOIN issues t ON t.id = k.target_id JOIN projects p ON p.id = t.project_id WHERE k.source_id = ?
		UNION ALL
		SELECT k.id, k.type, 'inbound', p.key || '-' || t.number, t.title, t.status
		  FROM issue_links k JOIN issues t ON t.id = k.source_id JOIN projects p ON p.id = t.project_id WHERE k.target_id = ?
		ORDER BY 1`, issueID, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := []Link{}
	for rs.Next() {
		var l Link
		if err := rs.Scan(&l.ID, &l.Type, &l.Direction, &l.Key, &l.Title, &l.Status); err != nil {
			return nil, err
		}
		// A link into a project the caller cannot see is hidden, not described.
		if project, _, _ := splitKey(l.Key); a.CanProject(project) {
			out = append(out, l)
		}
	}
	return out, rs.Err()
}

// AddLink links issue key to target: "blocks" means key blocks target, "duplicates" means
// key duplicates target. Self links, repeats and cycles are rejected; "relates" is
// symmetric, so relating A to B twice (in either order) is a repeat.
func (s *Store) AddLink(ctx context.Context, a *Principal, key, typ, target string) (*Link, error) {
	if !slices.Contains(LinkTypes, typ) {
		return nil, invalid("type", "must be one of %s", strings.Join(LinkTypes, ", "))
	}
	if !issueKeyRE.MatchString(target) {
		return nil, invalid("target", "must be an issue key like ACME-12")
	}
	var out *Link
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		src, err := s.writeRow(ctx, tx, a, key)
		if err != nil {
			return err
		}
		dst, err := s.loadIssue(ctx, tx, a, target) // out-of-scope targets look missing
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return invalid("target", "%s does not exist", target)
			}
			return err
		}
		if dst.archived {
			return fmt.Errorf("%w: project %s is archived", ErrConflict, dst.Project)
		}
		if src.id == dst.id {
			return invalid("target", "an issue cannot be linked to itself")
		}
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM issue_links WHERE type = ? AND ((source_id = ? AND target_id = ?) OR (? = 'relates' AND source_id = ? AND target_id = ?))`,
			typ, src.id, dst.id, typ, dst.id, src.id).Scan(&one)
		if err == nil {
			return fmt.Errorf("%w: %s already %s %s", ErrConflict, key, typ, target)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if typ != "relates" {
			// Adding src->dst closes a loop when dst already reaches src along the same type.
			err = tx.QueryRowContext(ctx, `WITH RECURSIVE reach(id) AS (
					SELECT target_id FROM issue_links WHERE type = ?1 AND source_id = ?2
					UNION
					SELECT l.target_id FROM issue_links l JOIN reach r ON l.source_id = r.id WHERE l.type = ?1)
				SELECT 1 FROM reach WHERE id = ?3 LIMIT 1`, typ, dst.id, src.id).Scan(&one)
			if err == nil {
				return fmt.Errorf("%w: %s %s %s would create a cycle", ErrConflict, key, typ, target)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO issue_links (type, source_id, target_id, created_by, created_at) VALUES (?, ?, ?, ?, ?)`, typ, src.id, dst.id, a.Subject, ts(s.now()))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		for _, rowID := range []int64{src.id, dst.id} {
			if err := s.addActivity(ctx, tx, rowID, a.Subject, "linked", map[string]string{"type": typ, "from": src.Key, "to": dst.Key}); err != nil {
				return err
			}
		}
		out = &Link{ID: id, Type: typ, Direction: "outbound", Key: dst.Key, Title: dst.Title, Status: dst.Status}
		return nil
	})
	return out, err
}

// RemoveLink deletes a link the issue is part of.
func (s *Store) RemoveLink(ctx context.Context, a *Principal, key string, linkID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		row, err := s.writeRow(ctx, tx, a, key)
		if err != nil {
			return err
		}
		var typ, srcProject, dstProject string
		var src, dst int64
		err = tx.QueryRowContext(ctx, `SELECT k.type, k.source_id, k.target_id, sp.key, dp.key FROM issue_links k
			JOIN issues si ON si.id = k.source_id JOIN projects sp ON sp.id = si.project_id
			JOIN issues di ON di.id = k.target_id JOIN projects dp ON dp.id = di.project_id
			WHERE k.id = ? AND (k.source_id = ? OR k.target_id = ?)`, linkID, row.id, row.id).Scan(&typ, &src, &dst, &srcProject, &dstProject)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// A link into a project the caller cannot see is invisible, so it cannot be removed
		// either (and no activity row is written on the side the caller cannot see).
		if !a.CanProject(srcProject) || !a.CanProject(dstProject) {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_links WHERE id = ?`, linkID); err != nil {
			return err
		}
		for _, id := range []int64{src, dst} {
			if err := s.addActivity(ctx, tx, id, a.Subject, "unlinked", map[string]any{"type": typ, "link_id": linkID}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- watchers -------------------------------------------------------------------------

// SetWatcher adds or removes a watcher.
func (s *Store) SetWatcher(ctx context.Context, a *Principal, key, subject string, watch bool) error {
	if !validSubject(subject) {
		return invalid("subject", "must be a non-empty identifier of at most %d characters", maxSubject)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		row, err := s.writeRow(ctx, tx, a, key) // first, so an out-of-scope key is 404 whatever the subject
		if err != nil {
			return err
		}
		// People add and remove themselves; changing who else watches is an admin's call, so a
		// write token cannot subscribe an arbitrary address to a project's activity.
		if subject != a.Subject && !a.Can(PermAdmin) {
			return fmt.Errorf("%w: only an admin can change who else watches", ErrForbidden)
		}
		if watch {
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO watchers (issue_id, subject) VALUES (?, ?)`, row.id, subject)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM watchers WHERE issue_id = ? AND subject = ?`, row.id, subject)
		}
		return err
	})
}

// ---- attachments (metadata only) ------------------------------------------------------

// NewAttachment is the body of an attachment registration.
type NewAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	URL         string `json:"url"`
}

const maxAttachmentBytes = 1 << 30

func (n *NewAttachment) validate() error {
	var p problems
	if !filenameRE.MatchString(n.Filename) || n.Filename == "." || n.Filename == ".." || !validText(n.Filename, 255) {
		p.add("filename", "1-255 characters without path separators or control characters")
	}
	if len(n.ContentType) > 127 || strings.ContainsAny(n.ContentType, "\r\n\x00") {
		p.add("content_type", "at most 127 characters")
	}
	if n.SizeBytes < 0 || n.SizeBytes > maxAttachmentBytes {
		p.add("size_bytes", "between 0 and %d", maxAttachmentBytes)
	}
	if n.SHA256 != "" && !sha256RE.MatchString(n.SHA256) {
		p.add("sha256", "must be 64 lower-case hex characters")
	}
	if n.URL != "" {
		u, err := url.Parse(n.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(n.URL) > 2000 {
			p.add("url", "must be an http(s) URL of at most 2000 characters")
		}
	}
	return p.err()
}

func (s *Store) attachmentsFor(ctx context.Context, q dbq, issueID int64) ([]Attachment, error) {
	rs, err := q.QueryContext(ctx, `SELECT id, filename, content_type, size_bytes, sha256, url, added_by, created_at FROM attachments WHERE issue_id = ? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := []Attachment{}
	for rs.Next() {
		var at Attachment
		var c int64
		if err := rs.Scan(&at.ID, &at.Filename, &at.ContentType, &at.SizeBytes, &at.SHA256, &at.URL, &at.AddedBy, &c); err != nil {
			return nil, err
		}
		at.CreatedAt = fromUnix(c)
		out = append(out, at)
	}
	return out, rs.Err()
}

const maxAttachmentsPerIssue = 50

// AddAttachment records attachment metadata. No bytes are stored.
func (s *Store) AddAttachment(ctx context.Context, a *Principal, key string, in NewAttachment) (*Attachment, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	var out *Attachment
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		row, err := s.writeRow(ctx, tx, a, key)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE issue_id = ?`, row.id).Scan(&n); err != nil {
			return err
		}
		if n >= maxAttachmentsPerIssue {
			return fmt.Errorf("%w: at most %d attachments per issue", ErrConflict, maxAttachmentsPerIssue)
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `INSERT INTO attachments (issue_id, filename, content_type, size_bytes, sha256, url, added_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			row.id, in.Filename, in.ContentType, in.SizeBytes, in.SHA256, in.URL, a.Subject, ts(now))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		out = &Attachment{ID: id, Filename: in.Filename, ContentType: in.ContentType, SizeBytes: in.SizeBytes, SHA256: in.SHA256, URL: in.URL, AddedBy: a.Subject, CreatedAt: now.UTC().Truncate(1e9)}
		return s.addActivity(ctx, tx, row.id, a.Subject, "attachment_added", map[string]any{"attachment_id": id, "filename": in.Filename})
	})
	return out, err
}

// RemoveAttachment deletes attachment metadata.
func (s *Store) RemoveAttachment(ctx context.Context, a *Principal, key string, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		row, err := s.writeRow(ctx, tx, a, key)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE id = ? AND issue_id = ?`, id, row.id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return s.addActivity(ctx, tx, row.id, a.Subject, "attachment_removed", map[string]any{"attachment_id": id})
	})
}

// redactLinkActivity hides the keys of a "linked" entry when the other end is in a project the
// caller cannot see (the entry itself stays, so the history has no hole).
func redactLinkActivity(a *Principal, data json.RawMessage) json.RawMessage {
	var d struct{ Type, From, To string }
	if json.Unmarshal(data, &d) != nil {
		return json.RawMessage(`{"redacted":true}`)
	}
	for _, k := range []string{d.From, d.To} {
		if p, _, ok := splitKey(k); !ok || !a.CanProject(p) {
			return json.RawMessage(`{"redacted":true}`)
		}
	}
	return data
}

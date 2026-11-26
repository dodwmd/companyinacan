package work

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Erasure of a person (specs/056-erasure-finance-work). The personal data the tracker holds about
// a person is the identifier they appear under: reporter, assignee, comment author, activity actor,
// watcher, who added a link or an attachment, who an API token belongs to or was made by, and the
// copies of those in activity details, cached replies and queued events. Erasing replaces the
// identifier with a random pseudonym. Texts people wrote (titles, descriptions, comments, file
// names) and the admin audit log are not changed.

// ClassPerson is the data class of an erasure: the one that holds the tracker's names of people
// (modules/work/module.yaml). It is the only one purge accepts.
const ClassPerson = "work-issue"

// replayActor names a replayed erasure in the audit log.
const replayActor = "system:erasure-replay"

const pseudonymPrefix = "erased-"

var (
	pseudonymRe      = regexp.MustCompile(`^erased-[0-9a-f]{16}$`)
	erasureSubjectRe = regexp.MustCompile(`^person-[0-9a-f]{40}$`)
)

// errScrub is an erasure that is committed but left traces in the database files.
var errScrub = errors.New("the person is erased and audited, but scrubbing the database files failed (it is retried by itself)")

// erasureSubject names a person in a tombstone: a hash of the identifier, so that the ledger holds
// no identifier and a restore can still find it among those in an older database. It is not
// anonymous: a guessed identifier can be tested against it.
func erasureSubject(identity string) string {
	sum := sha256.Sum256([]byte("can-work-person\x00" + identity))
	return "person-" + hex.EncodeToString(sum[:20])
}

// ErasureResult is what erasing a person did.
type ErasureResult struct {
	// Subject names this erasure in the erasure ledger (data class work-issue).
	Subject string `json:"subject"`
	// Pseudonym replaces the identifier. Empty when nothing was left to replace.
	Pseudonym     string         `json:"pseudonym,omitempty"`
	AlreadyErased bool           `json:"already_erased,omitempty"`
	Changed       map[string]int `json:"changed"`
	// ScrubPending: the person is erased, but traces may remain in the database files until the
	// scrub, which is retried, succeeds.
	ScrubPending bool `json:"scrub_pending,omitempty"`
}

// ErasurePersonRequest is the body of POST /v1/people/erase. The identifier travels in the body,
// never in a URL.
type ErasurePersonRequest struct {
	Subject string `json:"subject"`
}

// ErasePerson replaces a person's identifier throughout the tracker, revokes their API tokens,
// records the tombstone and scrubs the database files. It needs work.admin and a token that is not
// limited to projects. A repeat finds nothing and changes nothing; if the identifier has been used
// again since, it is erased again and the tombstone takes the later time. ErrNotFound for an
// identifier that appears nowhere and was never erased. With an error wrapping errScrub the result
// is set: the person is erased.
func (s *Store) ErasePerson(ctx context.Context, a *Principal, identity string) (*ErasureResult, error) {
	return s.erasePerson(ctx, a, identity, time.Time{})
}

// erasePerson is ErasePerson; erasedAt, when set, is when a replayed erasure was first made, which
// the tombstone keeps.
func (s *Store) erasePerson(ctx context.Context, a *Principal, identity string, erasedAt time.Time) (*ErasureResult, error) {
	if !a.Can(PermAdmin) || !a.Unscoped() {
		return nil, ErrForbidden
	}
	if !validSubject(identity) || strings.EqualFold(identity, bootstrapSubject) || identity == replayActor || pseudonymRe.MatchString(identity) {
		return nil, invalid("subject", "must be a person's identifier, as it appears on issues (not a reserved one or a pseudonym)")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	pseudo := pseudonymPrefix + hex.EncodeToString(b[:])
	out := &ErasureResult{Subject: erasureSubject(identity), Changed: map[string]int{}}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.replacePerson(ctx, tx, identity, pseudo, out.Changed); err != nil {
			return err
		}
		if len(out.Changed) == 0 {
			var one int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM erasures WHERE subject = ? AND class = ?`, out.Subject, ClassPerson).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			out.AlreadyErased = true
			if err != nil {
				return err
			}
		} else {
			out.Pseudonym = pseudo
			stamp := erasedAt
			if stamp.IsZero() {
				stamp = s.now()
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO erasures (subject, class, created_at) VALUES (?, ?, ?)
				ON CONFLICT (subject, class) DO UPDATE SET created_at = MAX(created_at, excluded.created_at), scrubbed = 0`, out.Subject, ClassPerson, ts(stamp)); err != nil {
				return err
			}
		}
		actor := a.Subject
		if actor == identity {
			actor = pseudo
		}
		return s.audit(ctx, tx, actor, "person.erase", out.Subject, out.Changed)
	})
	if err != nil {
		return nil, err
	}
	// The erasure is committed: the scrub goes on even if the caller has gone away.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := s.ResumeScrub(sctx); err != nil {
		out.ScrubPending = true
		return out, fmt.Errorf("%w: %w", errScrub, err)
	}
	return out, nil
}

// replacePerson rewrites every place the identifier is held and counts what it changed.
func (s *Store) replacePerson(ctx context.Context, tx *sql.Tx, identity, pseudo string, changed map[string]int) error {
	now := ts(s.now())
	quoted, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	exec := func(what, q string, args ...any) error {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed[what] += int(n)
		}
		return nil
	}
	for _, q := range []struct {
		what, sql string
		args      []any
	}{
		{"issues", `UPDATE issues SET reporter = CASE WHEN reporter = ?1 THEN ?2 ELSE reporter END,
			assignee = CASE WHEN assignee = ?1 THEN ?2 ELSE assignee END, version = version + 1
			WHERE reporter = ?1 OR assignee = ?1`, []any{identity, pseudo}},
		{"comments", `UPDATE comments SET author = ? WHERE author = ?`, []any{pseudo, identity}},
		{"activity", `UPDATE activity SET actor = ? WHERE actor = ?`, []any{pseudo, identity}},
		{"watchers", `DELETE FROM watchers WHERE subject = ?`, []any{identity}},
		{"attachments", `UPDATE attachments SET added_by = ? WHERE added_by = ?`, []any{pseudo, identity}},
		{"links", `UPDATE issue_links SET created_by = ? WHERE created_by = ?`, []any{pseudo, identity}},
		{"tokens", `UPDATE api_tokens SET subject = ?, name = '', revoked_at = COALESCE(revoked_at, ?) WHERE subject = ?`, []any{pseudo, now, identity}},
		{"tokens_created", `UPDATE api_tokens SET created_by = ? WHERE created_by = ?`, []any{pseudo, identity}},
		{"project_members", `DELETE FROM project_members WHERE subject = ?`, []any{identity}},
		{"project_members_added", `UPDATE project_members SET added_by = ? WHERE added_by = ?`, []any{pseudo, identity}},
		// A private filter is the person's own; a shared one is the team's and stays.
		{"saved_filters_dropped", `DELETE FROM saved_filters WHERE owner = ? AND shared = 0`, []any{identity}},
		{"saved_filters", `UPDATE saved_filters SET owner = ? WHERE owner = ?`, []any{pseudo, identity}},
		// Cached replies are only there for retries: the ones that are, or name, the person go.
		{"cached_replies", `DELETE FROM idempotency WHERE subject = ? OR instr(response, ?) > 0`, []any{identity, string(quoted)}},
		// Delivered events are history nobody reads again; those still to be sent are rewritten below.
		{"events_dropped", `DELETE FROM outbox WHERE delivered_at IS NOT NULL AND instr(payload, ?) > 0`, []any{string(quoted)}},
	} {
		if err := exec(q.what, q.sql, q.args...); err != nil {
			return err
		}
	}
	// A filter that searches for the person by name goes with them.
	filters, err := filtersNaming(ctx, tx, func(v string) bool { return v == identity })
	if err != nil {
		return err
	}
	for _, id := range filters {
		if err := exec("saved_filters_dropped", `DELETE FROM saved_filters WHERE id = ?`, id); err != nil {
			return err
		}
	}
	for _, j := range []struct{ what, sel, upd string }{
		{"activity_details", `SELECT id, data FROM activity WHERE instr(data, ?) > 0`, `UPDATE activity SET data = ? WHERE id = ?`},
		{"events", `SELECT id, payload FROM outbox WHERE instr(payload, ?) > 0`, `UPDATE outbox SET payload = ? WHERE id = ?`},
	} {
		rows, err := jsonRows(ctx, tx, j.sel, string(quoted))
		if err != nil {
			return err
		}
		for _, r := range rows {
			out, hit, err := replaceInJSON(r.doc, identity, pseudo)
			if err != nil {
				return fmt.Errorf("%s %d: %w", j.what, r.id, err)
			}
			if !hit {
				continue
			}
			if err := exec(j.what, j.upd, out, r.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// filtersNaming returns the saved filters whose query names a person match accepts.
func filtersNaming(ctx context.Context, q dbq, match func(string) bool) ([]int64, error) {
	rows, err := jsonRows(ctx, q, `SELECT id, query FROM saved_filters`)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, r := range rows {
		hit := false
		for _, v := range filterPeople(r.doc) {
			hit = match(v) || hit
		}
		if hit {
			out = append(out, r.id)
		}
	}
	return out, nil
}

type jsonRow struct {
	id  int64
	doc string
}

func jsonRows(ctx context.Context, q dbq, query string, args ...any) ([]jsonRow, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	var out []jsonRow
	for rs.Next() {
		var r jsonRow
		if err := rs.Scan(&r.id, &r.doc); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// personKeys are the fields of stored JSON (activity details, events, cached replies) that name a
// person, either as a value or as the from and to of a change. Other fields, such as a title that
// happens to equal an identifier, are left alone.
var personKeys = map[string]bool{"reporter": true, "assignee": true, "actor": true, "author": true}

// walkPeople calls fn for every identifier under personKeys and stores what it returns.
func walkPeople(v any, fn func(string) string) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			walkPeople(e, fn)
		}
	case map[string]any:
		for k, e := range x {
			if !personKeys[k] {
				walkPeople(e, fn)
				continue
			}
			switch p := e.(type) {
			case string:
				x[k] = fn(p)
			case map[string]any:
				for ck, cv := range p {
					if s, ok := cv.(string); ok {
						p[ck] = fn(s)
					}
				}
			}
		}
	}
}

func decodeJSONDoc(doc string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var v any
	return v, dec.Decode(&v)
}

func replaceInJSON(doc, identity, pseudo string) (string, bool, error) {
	v, err := decodeJSONDoc(doc)
	if err != nil {
		return "", false, err
	}
	hit := false
	walkPeople(v, func(s string) string {
		if s != identity {
			return s
		}
		hit = true
		return pseudo
	})
	b, err := json.Marshal(v)
	return string(b), hit, err
}

// identityOf finds the identifier in this database that a tombstone's subject names, or "".
func (s *Store) identityOf(ctx context.Context, subject string) (string, error) {
	found := ""
	match := func(v string) string {
		if v != "" && erasureSubject(v) == subject {
			found = v
		}
		return v
	}
	rs, err := s.db.QueryContext(ctx, `SELECT reporter FROM issues UNION SELECT assignee FROM issues WHERE assignee IS NOT NULL
		UNION SELECT author FROM comments UNION SELECT actor FROM activity UNION SELECT subject FROM watchers
		UNION SELECT added_by FROM attachments UNION SELECT created_by FROM issue_links
		UNION SELECT subject FROM api_tokens UNION SELECT created_by FROM api_tokens UNION SELECT subject FROM idempotency
		UNION SELECT subject FROM project_members UNION SELECT added_by FROM project_members UNION SELECT owner FROM saved_filters`)
	if err != nil {
		return "", err
	}
	defer func() { _ = rs.Close() }()
	for rs.Next() {
		var v string
		if err := rs.Scan(&v); err != nil {
			return "", err
		}
		match(v)
	}
	if err := errors.Join(rs.Err(), rs.Close()); err != nil || found != "" {
		return found, err
	}
	// An identifier may be left only in a saved filter that searches for it.
	if _, err := filtersNaming(ctx, s.db, func(v string) bool { match(v); return false }); err != nil || found != "" {
		return found, err
	}
	// An identifier may be left only in the history: a former assignee.
	docs, err := jsonRows(ctx, s.db, `SELECT id, data FROM activity UNION ALL SELECT id, payload FROM outbox UNION ALL SELECT rowid, response FROM idempotency`)
	if err != nil {
		return "", err
	}
	for _, d := range docs {
		v, err := decodeJSONDoc(d.doc)
		if err != nil {
			return "", err
		}
		walkPeople(v, match)
		if found != "" {
			break
		}
	}
	return found, nil
}

// PurgeSubject replays one erasure on a restored database. A person who does not appear in it is
// not an error and gives a nil result. Repeating it changes nothing more. erasedAt is when the
// erasure was first made (zero: unknown, now is recorded).
func (s *Store) PurgeSubject(ctx context.Context, subject, class string, erasedAt time.Time) (*ErasureResult, error) {
	if class != ClassPerson {
		return nil, fmt.Errorf("purge handles class %s only", ClassPerson)
	}
	if !erasureSubjectRe.MatchString(subject) {
		return nil, errors.New("the subject of a person erasure looks like person-<40 hex characters>")
	}
	identity, err := s.identityOf(ctx, subject)
	if err != nil {
		return nil, err
	}
	if identity == "" {
		return nil, s.ResumeScrub(ctx)
	}
	return s.erasePerson(ctx, &Principal{Subject: replayActor, Permissions: []string{PermAdmin}}, identity, erasedAt)
}

// ResumeScrub scrubs the database files if an erasure was committed and the scrub that should have
// followed it did not finish: VACUUM clears the free pages, the checkpoint the write-ahead log.
func (s *Store) ResumeScrub(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM erasures WHERE scrubbed = 0`).Scan(&n); err != nil || n == 0 {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if err := s.truncateWAL(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE erasures SET scrubbed = 1 WHERE scrubbed = 0`); err != nil {
		return err
	}
	return s.truncateWAL(ctx) // the mark itself went to the log: leave it empty
}

func (s *Store) truncateWAL(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	if busy != 0 {
		return errors.New("wal checkpoint was blocked; erased data may remain in the write-ahead log")
	}
	return nil
}

// ErasureEntry is one tombstone as can-core pulls it into the erasure ledger.
type ErasureEntry struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
	// At is when the data was erased: a restore leaves alone a backup made after it.
	At string `json:"at"`
}

// ListErasures returns tombstones after a cursor, at most 1000.
func (s *Store) ListErasures(ctx context.Context, after int64) ([]ErasureEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, subject, class, created_at FROM erasures WHERE seq > ? ORDER BY seq LIMIT 1000`, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ErasureEntry{}
	for rows.Next() {
		var e ErasureEntry
		var at int64
		if err := rows.Scan(&e.Seq, &e.Subject, &e.Class, &at); err != nil {
			return nil, err
		}
		t := fromUnix(at)
		e.Date, e.At = t.Format(time.DateOnly), t.Format(time.RFC3339)
		out = append(out, e)
	}
	return out, rows.Err()
}

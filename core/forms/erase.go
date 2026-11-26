package forms

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Erasure of a single lead (specs/054-lead-erasure): the way to answer a data subject's request
// without editing the database.

// ClassLead is the data class of a submission (modules/forms/module.yaml) and of its tombstones.
const ClassLead = "lead-submission"

// EventLeadErased tells the consumers of the leads capability to drop their copies. Ids only.
const EventLeadErased = "lead.erased.v1"

// maxMatches bounds the answer of a search by address.
const maxMatches = 1000

var leadIDRe = regexp.MustCompile(`^ld_[0-9a-f]{24}$`)

// errScrub is an erasure that is committed but left traces in the database files.
var errScrub = errors.New("the lead is erased and audited, but scrubbing the database files failed (it is retried by itself; repeating the request retries at once)")

// ErasedLead is the result of an erasure.
type ErasedLead struct {
	LeadID   string    `json:"lead_id"`
	ErasedAt time.Time `json:"erased_at"`
	// AlreadyErased is set when the lead had been erased before.
	AlreadyErased bool `json:"already_erased,omitempty"`
	// Notification is the state of the notification email when the lead was erased: "cancelled"
	// (it was still waiting and will not be sent), "none" (nothing was waiting) or "" for a repeat.
	Notification string `json:"notification,omitempty"`
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

// Match is a lead that names an address. It holds ids only: the lead itself is read by id.
type Match struct {
	ID        string    `json:"id"`
	FormID    string    `json:"form_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// Match is "address" when a field is the address, "mention" when it only appears in a text,
	// which may be about somebody else: read the lead before erasing it.
	Match string `json:"match"`
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) audit(ctx context.Context, q execer, actor, action, target string, count int) error {
	_, err := q.ExecContext(ctx, `INSERT INTO lead_audit (at, actor, action, target, count) VALUES (?, ?, ?, ?, ?)`, s.now().Unix(), actor, action, target, count)
	return err
}

// checkpoint truncates the write-ahead log, where deleted rows survive until the next automatic
// checkpoint (secure_delete only zeroes them in the main file).
func (s *Store) checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	if busy != 0 {
		return errors.New("wal checkpoint was blocked; erased data may remain in the write-ahead log")
	}
	return nil
}

// scrub removes erased data from the free pages (VACUUM) and the write-ahead log.
func (s *Store) scrub(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return s.checkpoint(ctx)
}

func (s *Store) exists(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM leads WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// Erase deletes a lead, its delivery records and its waiting notification email, records the
// tombstone, queues lead.erased.v1 for the channels given and scrubs the database files. It is
// idempotent: a repeat retries the scrub. ErrNotFound for an id that is neither a lead nor erased.
// With an error wrapping errScrub the result is set: the lead is gone.
func (s *Store) Erase(ctx context.Context, actor, id string, channels []string) (*ErasedLead, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := &ErasedLead{LeadID: id, ErasedAt: time.Unix(s.now().Unix(), 0).UTC()}
	var formID, mail string
	err = tx.QueryRowContext(ctx, `SELECT form_id, COALESCE((SELECT status FROM lead_email WHERE lead_id = leads.id), '') FROM leads WHERE id = ?`, id).Scan(&formID, &mail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var at int64
		err = tx.QueryRowContext(ctx, `SELECT created_at FROM erasures WHERE subject = ? AND class = ?`, id, ClassLead).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		out.AlreadyErased, out.ErasedAt = true, time.Unix(at, 0).UTC()
	case err != nil:
		return nil, err
	default:
		out.Notification = "none"
		if mail == "pending" {
			out.Notification = "cancelled"
		}
		// lead_deliveries and lead_email go with the row (ON DELETE CASCADE).
		if _, err := tx.ExecContext(ctx, `DELETE FROM leads WHERE id = ?`, id); err != nil {
			return nil, err
		}
		// A lead brought back by a restore is erased again: the tombstone takes the new time.
		if _, err := tx.ExecContext(ctx, `INSERT INTO erasures (subject, class, form_id, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (subject, class) DO UPDATE SET created_at = excluded.created_at, scrubbed = 0`, id, ClassLead, formID, out.ErasedAt.Unix()); err != nil {
			return nil, err
		}
		for _, ch := range channels {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO erasure_notices (subject, channel, next_attempt_at) VALUES (?, ?, ?)`, id, ch, out.ErasedAt.Unix()); err != nil {
				return nil, err
			}
		}
	}
	if err := s.audit(ctx, tx, actor, "lead.erase", id, 1); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// The erasure is committed: the scrub goes on even if the caller has gone away.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := s.scrub(sctx); err != nil {
		return out, fmt.Errorf("%w: %w", errScrub, err)
	}
	if err := s.markScrubbed(sctx, id); err != nil {
		slog.Warn("marking an erasure as scrubbed", "error", err) // it is only scrubbed once more
	}
	return out, nil
}

func (s *Store) markScrubbed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE erasures SET scrubbed = 1 WHERE subject = ?`, id)
	return err
}

// ResumeScrub scrubs the database files if an erasure was committed and the scrub that should have
// followed it did not finish: the process stopped, or it failed.
func (s *Store) ResumeScrub(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT subject FROM erasures WHERE scrubbed = 0`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || len(ids) == 0 {
		return err
	}
	if err := s.scrub(ctx); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.markScrubbed(ctx, id); err != nil {
			return err
		}
	}
	return nil
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
		t := time.Unix(at, 0).UTC()
		e.Date, e.At = t.Format(time.DateOnly), t.Format(time.RFC3339)
		out = append(out, e)
	}
	return out, rows.Err()
}

// validSearchAddress is deliberately looser than the form's e-mail check: the address of an
// erasure request may also have been typed into a free text.
func validSearchAddress(a string) bool {
	local, domain, ok := strings.Cut(a, "@")
	return ok && local != "" && domain != "" && len(a) <= 254 && utf8.ValidString(a) &&
		strings.IndexFunc(a, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func asciiAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// folded is how texts and the address searched for are compared: lower case, composed.
func folded(s string) string { return norm.NFC.String(strings.ToLower(s)) }

// mentions reports whether text holds addr (both folded). Only an ASCII letter or digit right
// before it (jimbob@example.com), or right after it, also behind a dot (bob@example.com.au), makes
// it another address. Anything else, such as punctuation, a dash, an underscore or a letter of
// another script, is taken as the end of the address: a person reads a mention before erasing it, so
// finding too much is the safer mistake.
func mentions(text, addr string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], addr)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(addr)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, n := utf8.DecodeRuneInString(text[end:])
		next, _ := utf8.DecodeRuneInString(text[end+n:])
		longerBefore := start > 0 && asciiAlnum(before)
		longerAfter := n > 0 && (asciiAlnum(after) || (after == '.' && asciiAlnum(next)))
		if !longerBefore && !longerAfter {
			return true
		}
		from = start + 1
	}
}

// FindByEmail returns the leads that name addr, in any field or in the staff note, ignoring case.
// The comparison is made here and not in SQL, which only folds ASCII. truncated reports that there
// are more than the answer holds: erase these and search again.
func (s *Store) FindByEmail(ctx context.Context, actor, addr string) (out []Match, truncated bool, err error) {
	want := folded(addr)
	rows, err := s.db.QueryContext(ctx, `SELECT id, form_id, status, email, name, fields, note, created_at FROM leads ORDER BY seq DESC`)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out = []Match{}
	for rows.Next() {
		var m Match
		var email, name, fields, note string
		var created int64
		if err := rows.Scan(&m.ID, &m.FormID, &m.Status, &email, &name, &fields, &note, &created); err != nil {
			return nil, false, err
		}
		values := map[string]string{}
		if err := json.Unmarshal([]byte(fields), &values); err != nil {
			values = map[string]string{"": fields} // a corrupt row is still searched
		}
		texts := []string{email, name, note}
		for _, v := range values {
			texts = append(texts, v)
		}
		for _, t := range texts {
			t = folded(t)
			if strings.TrimSpace(t) == want {
				m.Match = "address"
				break
			}
			if mentions(t, want) {
				m.Match = "mention"
			}
		}
		if m.Match == "" {
			continue
		}
		if len(out) == maxMatches {
			truncated = true
			break
		}
		m.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, m)
	}
	// Closed before the audit is written: there is one database connection.
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	return out, truncated, s.audit(ctx, s.db, actor, "lead.find", "-", len(out))
}

type notice struct {
	subject, channel, formID string
	attempts                 int
	erasedAt                 time.Time
}

func (s *Store) dueNotices(ctx context.Context, now time.Time) ([]notice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.subject, n.channel, n.attempts, e.form_id, e.created_at FROM erasure_notices n
		JOIN erasures e ON e.subject = n.subject WHERE n.next_attempt_at <= ? ORDER BY n.next_attempt_at LIMIT ?`, now.Unix(), retryBatch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []notice
	for rows.Next() {
		var n notice
		var at int64
		if err := rows.Scan(&n.subject, &n.channel, &n.attempts, &n.formID, &at); err != nil {
			return nil, err
		}
		n.erasedAt = time.Unix(at, 0).UTC()
		out = append(out, n)
	}
	return out, rows.Err()
}

// erasedEvent is the lead.erased.v1 envelope (schemas/events/lead.erased.v1.json). The id is
// derived from the lead id, so that a repeat is recognised.
func erasedEvent(n notice) (Event, []byte) {
	sum := sha256.Sum256([]byte(EventLeadErased + "\x00" + n.subject))
	ev := Event{ID: "evt_" + hex.EncodeToString(sum[:12]), Type: EventLeadErased, Time: n.erasedAt, Source: "forms"}
	b, _ := json.Marshal(map[string]any{"id": ev.ID, "type": ev.Type, "time": ev.Time, "source": ev.Source,
		"data": map[string]any{"lead_id": n.subject, "form_id": n.formID, "erased_at": n.erasedAt}})
	return ev, b
}

// AnnounceErasures hands the waiting lead.erased.v1 events to can-core and the webhook. Unlike
// lead.created.v1 it is never given up: a consumer that keeps a copy must hear of the erasure.
func (d *Dispatcher) AnnounceErasures(ctx context.Context) error {
	due, err := d.store.dueNotices(ctx, d.now())
	if err != nil {
		return err
	}
	for _, n := range due {
		ev, body := erasedEvent(n)
		var sendErr error
		switch {
		case n.channel == "can" && d.can != nil:
			sendErr = d.can.PublishEnvelope(ctx, body)
		case n.channel == "webhook" && d.webhook != nil:
			_, sendErr = d.webhook.once(ctx, ev, body)
		default:
			// The channel is not configured now: kept, and looked at again later.
			sendErr = errors.New("the channel is not configured")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sendErr == nil {
			_, err = d.store.db.ExecContext(ctx, `DELETE FROM erasure_notices WHERE subject = ? AND channel = ?`, n.subject, n.channel)
		} else {
			d.log.Warn("lead.erased not delivered; it will be retried", "lead", n.subject, "channel", n.channel, "attempts", n.attempts+1, "error", sendErr)
			_, err = d.store.db.ExecContext(ctx, `UPDATE erasure_notices SET attempts = attempts + 1, next_attempt_at = ? WHERE subject = ? AND channel = ?`,
				d.now().Add(retryPause(n.attempts+1)).Unix(), n.subject, n.channel)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- notifications under way ------------------------------------------------------------------

var errErased = errors.New("the lead was erased")

type underWay struct{ cancel context.CancelCauseFunc }

// hold registers a notification of a lead that is about to be sent, so that erasing the lead stops
// it. ok is false when the lead is already gone. done must be called either way.
func (d *Dispatcher) hold(parent context.Context, leadID string) (ctx context.Context, done func(), ok bool) {
	ctx, cancel := context.WithCancelCause(parent)
	h := &underWay{cancel}
	d.mu.Lock()
	if d.inflight == nil {
		d.inflight = map[string]map[*underWay]bool{}
	}
	if d.inflight[leadID] == nil {
		d.inflight[leadID] = map[*underWay]bool{}
	}
	d.inflight[leadID][h] = true
	d.mu.Unlock()
	done = func() {
		d.mu.Lock()
		delete(d.inflight[leadID], h)
		if len(d.inflight[leadID]) == 0 {
			delete(d.inflight, leadID)
		}
		d.mu.Unlock()
		cancel(nil)
	}
	// Looked up after registering: an erasure either finds the registration or has already
	// removed the row.
	found, err := d.store.exists(ctx, leadID)
	return ctx, done, found || (err != nil && ctx.Err() == nil)
}

// stop cancels the notifications of an erased lead and reports whether one was under way: that
// one may already have reached its receiver.
func (d *Dispatcher) stop(leadID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for h := range d.inflight[leadID] {
		h.cancel(errErased)
	}
	return len(d.inflight[leadID]) > 0
}

// kick makes the retry loop run now.
func (d *Dispatcher) kick() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

package jobs

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/dodwmd/companyinacan/core/modclient"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Event types published by the jobs module. Payload schemas are schemas/events/<type>.json.
// Payloads carry opaque ids and non-personal job facts only: no candidate name, contact detail,
// note or score, so an event never outlives an erasure with personal data in it.
const (
	EventJobOpened        = "job.opened.v1"
	EventApplicationRecv  = "application.received.v1"
	EventApplicationStage = "application.stage_changed.v1"
	EventCandidateHired   = "candidate.hired.v1"
	eventSource           = "jobs"
	maxBackoff            = 6 * time.Hour
	deliveryWindow        = 7 * 24 * time.Hour // undelivered after this long: dead-lettered, not dropped
	deadRetention         = 90 * 24 * time.Hour
	outboxBatch           = 20
	deliveredRetention    = 7 * 24 * time.Hour
)

// Event is the platform event envelope (SPEC.md section 4.3).
type Event struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

// Change is one field's before and after in an update event.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

type jobOpenedData struct {
	JobID          string    `json:"job_id"`
	Slug           string    `json:"slug"`
	Title          string    `json:"title"`
	Department     string    `json:"department"`
	EmploymentType string    `json:"employment_type"`
	Remote         string    `json:"remote"`
	Reopened       bool      `json:"reopened"`
	OpenedAt       time.Time `json:"opened_at"`
	Actor          string    `json:"actor"`
}

type applicationReceivedData struct {
	ApplicationID string    `json:"application_id"`
	JobID         string    `json:"job_id"`
	CandidateID   string    `json:"candidate_id"`
	Source        string    `json:"source"`
	NewCandidate  bool      `json:"new_candidate"`
	ReceivedAt    time.Time `json:"received_at"`
}

type applicationStageData struct {
	ApplicationID string    `json:"application_id"`
	JobID         string    `json:"job_id"`
	CandidateID   string    `json:"candidate_id"`
	From          string    `json:"from"`
	To            string    `json:"to"`
	Status        string    `json:"status"`
	Actor         string    `json:"actor"`
	ChangedAt     time.Time `json:"changed_at"`
}

type candidateHiredData struct {
	CandidateID    string    `json:"candidate_id"`
	ApplicationID  string    `json:"application_id"`
	JobID          string    `json:"job_id"`
	JobTitle       string    `json:"job_title"`
	Department     string    `json:"department"`
	EmploymentType string    `json:"employment_type"`
	Actor          string    `json:"actor"`
	HiredAt        time.Time `json:"hired_at"`
}

func newEventID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "evt_" + hex.EncodeToString(b[:])
}

// emit queues an event in the caller's transaction, so a change and its event commit or
// roll back together. It is a no-op when no webhook is configured. Event payloads carry
// identifiers and changed fields, never comment or description bodies.
func (s *Store) emit(ctx context.Context, tx dbq, typ string, data any) error {
	if !s.events {
		return nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	ev := Event{ID: newEventID(), Type: typ, Time: now, Source: eventSource, Data: b}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox (event_id, type, payload, created_at, next_attempt) VALUES (?, ?, ?, ?, ?)`,
		ev.ID, typ, string(payload), ts(now), ts(now))
	return err
}

// Sign returns the webhook signature "sha256=<hex>" of HMAC-SHA256(secret, ts + "." + body).
// It matches the forms gateway so one receiver verifies both.
func Sign(secret []byte, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Dispatcher delivers outbox events to one webhook URL, signed, with exponential backoff.
// Delivery is at least once; receivers de-duplicate on the event id.
type Dispatcher struct {
	store  *Store
	URL    string
	Secret []byte
	Client *http.Client
	Log    *slog.Logger
	Now    func() time.Time
	// Backoff is the delay before the second attempt; it doubles per attempt.
	Backoff time.Duration
	// Can, when set, publishes through can-core (POST /v1/events) instead of posting to URL: the
	// module-to-module path goes through can-core only.
	Can *modclient.Client
}

// Run delivers until ctx is done, waking on new events and on a timer.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		if _, err := d.DeliverDue(ctx); err != nil && ctx.Err() == nil {
			d.Log.Error("webhook delivery", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-d.store.kick:
		case <-t.C:
		}
	}
}

// DeliverDue sends every due event once and returns how many were delivered.
func (d *Dispatcher) DeliverDue(ctx context.Context) (int, error) {
	now := d.Now()
	type row struct {
		id       int64
		payload  string
		eventID  string
		typ      string
		attempts int
		created  int64
	}
	rows, err := d.store.db.QueryContext(ctx, `SELECT id, event_id, type, payload, attempts, created_at FROM outbox WHERE delivered_at IS NULL AND dead_at IS NULL AND next_attempt <= ? ORDER BY id LIMIT ?`,
		ts(now), outboxBatch)
	if err != nil {
		return 0, err
	}
	var due []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.eventID, &r.typ, &r.payload, &r.attempts, &r.created); err != nil {
			_ = rows.Close()
			return 0, err
		}
		due = append(due, r)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	sent := 0
	for _, r := range due {
		if now.Sub(fromUnix(r.created)) > deliveryWindow {
			// Keep it, flag it and say so: an admin lists dead events and replays them.
			if _, err := d.store.db.ExecContext(ctx, `UPDATE outbox SET dead_at = ? WHERE id = ?`, ts(now), r.id); err != nil {
				return sent, err
			}
			d.Log.Error("webhook event dead-lettered: not delivered within the delivery window; list with GET /v1/events/dead and replay", "event_id", r.eventID, "type", r.typ, "attempts", r.attempts)
			continue
		}
		sendErr := d.send(ctx, r.eventID, r.typ, []byte(r.payload), now)
		if sendErr == nil {
			sent++
			_, err = d.store.db.ExecContext(ctx, `UPDATE outbox SET delivered_at = ?, attempts = attempts + 1, last_error = '' WHERE id = ?`, ts(now), r.id)
		} else {
			delay := d.Backoff
			if delay <= 0 {
				delay = 30 * time.Second
			}
			delay = min(delay<<min(r.attempts, 12), maxBackoff)
			_, err = d.store.db.ExecContext(ctx, `UPDATE outbox SET attempts = attempts + 1, next_attempt = ?, last_error = ? WHERE id = ?`, ts(now.Add(delay)), sendErr.Error(), r.id)
		}
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

func (d *Dispatcher) send(ctx context.Context, id, typ string, body []byte, now time.Time) error {
	if d.Can != nil {
		return d.Can.PublishEnvelope(ctx, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	stamp := now.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "can-jobs")
	req.Header.Set("X-Can-Event-Id", id)
	req.Header.Set("X-Can-Event-Type", typ)
	req.Header.Set("X-Can-Timestamp", strconv.FormatInt(stamp, 10))
	req.Header.Set("X-Can-Signature", Sign(d.Secret, stamp, body))
	c := d.Client
	if c == nil {
		// No redirects: one could aim a call that carries a signature at another host.
		c = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: status %d", resp.StatusCode)
	}
	return nil
}

// purgeOutbox removes delivered events after a week and dead ones after 90 days.
func (s *Store) purgeOutbox(ctx context.Context, tx dbq, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE (delivered_at IS NOT NULL AND created_at < ?) OR (dead_at IS NOT NULL AND dead_at < ?)`,
		ts(now.Add(-deliveredRetention)), ts(now.Add(-deadRetention)))
	return err
}

// DeadEvent is an event that was not delivered within the delivery window.
type DeadEvent struct {
	seq       int64
	EventID   string    `json:"event_id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	DeadAt    time.Time `json:"dead_at"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error"`
}

// ListDeadEvents lists undelivered events past the delivery window, oldest first.
// after is the cursor (0 = start); next is the cursor for the following page (0 = last page).
func (s *Store) ListDeadEvents(ctx context.Context, after int64, limit int) (items []DeadEvent, next int64, err error) {
	limit = clampLimit(limit)
	rs, err := s.db.QueryContext(ctx, `SELECT id, event_id, type, created_at, dead_at, attempts, last_error FROM outbox WHERE dead_at IS NOT NULL AND delivered_at IS NULL AND id > ? ORDER BY id LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rs.Close() }()
	out := []DeadEvent{}
	for rs.Next() {
		var d DeadEvent
		var c, dd int64
		if err := rs.Scan(&d.seq, &d.EventID, &d.Type, &c, &dd, &d.Attempts, &d.LastError); err != nil {
			return nil, 0, err
		}
		d.CreatedAt, d.DeadAt = fromUnix(c), fromUnix(dd)
		out = append(out, d)
	}
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].seq
	}
	return out, next, rs.Err()
}

// ReplayEvent puts a dead event back in the queue with a fresh delivery window. The event
// keeps its id, so receivers that de-duplicate on it behave correctly.
func (s *Store) ReplayEvent(ctx context.Context, a *Principal, eventID string) error {
	if !a.Can(ActAdmin) {
		return ErrForbidden
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := ts(s.now())
		res, err := tx.ExecContext(ctx, `UPDATE outbox SET dead_at = NULL, attempts = 0, next_attempt = ?, created_at = ?, last_error = '' WHERE event_id = ? AND dead_at IS NOT NULL AND delivered_at IS NULL`, now, now, eventID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a.Subject, "event.replay", "event:"+eventID, nil, "ok")
	})
}

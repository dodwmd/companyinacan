package work

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Event types published by the work module. Payload schemas are schemas/events/<type>.json.
const (
	EventIssueCreated     = "work.issue.created.v1"
	EventIssueUpdated     = "work.issue.updated.v1"
	EventIssueTransition  = "work.issue.transitioned.v1"
	EventCommentCreated   = "work.comment.created.v1"
	eventSource           = "work"
	maxDeliveryAttempts   = 8
	outboxBatch           = 20
	outboxRetentionPeriod = 7 * 24 * time.Hour
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

type issueCreatedData struct {
	IssueKey  string    `json:"issue_key"`
	Project   string    `json:"project"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Priority  string    `json:"priority"`
	Status    string    `json:"status"`
	Reporter  string    `json:"reporter"`
	Assignee  *string   `json:"assignee"`
	Parent    *string   `json:"parent"`
	CreatedAt time.Time `json:"created_at"`
}

type issueUpdatedData struct {
	IssueKey  string            `json:"issue_key"`
	Project   string            `json:"project"`
	Version   int64             `json:"version"`
	Actor     string            `json:"actor"`
	Changes   map[string]Change `json:"changes"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type issueTransitionedData struct {
	IssueKey       string    `json:"issue_key"`
	Project        string    `json:"project"`
	From           string    `json:"from"`
	To             string    `json:"to"`
	ToCategory     string    `json:"to_category"`
	Actor          string    `json:"actor"`
	TransitionedAt time.Time `json:"transitioned_at"`
}

type commentCreatedData struct {
	IssueKey  string    `json:"issue_key"`
	Project   string    `json:"project"`
	CommentID int64     `json:"comment_id"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
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
	}
	rows, err := d.store.db.QueryContext(ctx, `SELECT id, event_id, type, payload, attempts FROM outbox WHERE delivered_at IS NULL AND attempts < ? AND next_attempt <= ? ORDER BY id LIMIT ?`,
		maxDeliveryAttempts, ts(now), outboxBatch)
	if err != nil {
		return 0, err
	}
	var due []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.eventID, &r.typ, &r.payload, &r.attempts); err != nil {
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
		sendErr := d.send(ctx, r.eventID, r.typ, []byte(r.payload), now)
		if sendErr == nil {
			sent++
			_, err = d.store.db.ExecContext(ctx, `UPDATE outbox SET delivered_at = ?, attempts = attempts + 1, last_error = '' WHERE id = ?`, ts(now), r.id)
		} else {
			delay := d.Backoff
			if delay <= 0 {
				delay = 30 * time.Second
			}
			delay = min(delay<<r.attempts, 6*time.Hour)
			_, err = d.store.db.ExecContext(ctx, `UPDATE outbox SET attempts = attempts + 1, next_attempt = ?, last_error = ? WHERE id = ?`, ts(now.Add(delay)), sendErr.Error(), r.id)
		}
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

func (d *Dispatcher) send(ctx context.Context, id, typ string, body []byte, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	stamp := now.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "can-work")
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

// PurgeDelivered removes delivered and exhausted outbox rows older than the retention.
func (s *Store) purgeOutbox(ctx context.Context, tx dbq, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE created_at < ? AND (delivered_at IS NOT NULL OR attempts >= ?)`,
		ts(now.Add(-outboxRetentionPeriod)), maxDeliveryAttempts)
	return err
}

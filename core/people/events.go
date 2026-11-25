package people

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
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

// Event types. Payload schemas: schemas/events/<type>.json.
const (
	EventPersonCreated        = "person.created.v1"
	EventPersonStatusChanged  = "person.status_changed.v1"
	EventPersonOffboarded     = "person.offboarded.v1"
	EventPersonErased         = "person.erased.v1"
	EventOneOnOneScheduled    = "oneonone.scheduled.v1"
	EventGoalUpdated          = "goal.updated.v1"
	EventChecklistTaskDone    = "checklist.task_completed.v1"
	eventSource               = "people"
	maxDeliveryAttempts       = 8
	outboxDeliveredRetention  = 7 * 24 * time.Hour
	outboxBatch               = 20
	outboxHTTPTimeout         = 10 * time.Second
	outboxMaxResponseDrainLen = 4096
)

// Event is the platform envelope (SPEC.md section 4.3). Payloads carry opaque ids and
// machine values only: no names, contact data or free text, so the outbox is not a copy of
// personal data (SPEC.md section 3.7, derived copies).
type Event struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Time   time.Time      `json:"time"`
	Source string         `json:"source"`
	Data   map[string]any `json:"data"`
}

// enqueue stores an event in the outbox inside the caller's transaction, so the event exists
// if and only if the change committed.
func (s *Service) enqueue(ctx context.Context, q querier, typ string, data map[string]any) error {
	ev := Event{ID: newID("evt"), Type: typ, Time: s.now().UTC(), Source: eventSource, Data: data}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO outbox (id, type, payload, created_at) VALUES (?,?,?,?)`, ev.ID, typ, string(b), s.now().Unix())
	return err
}

// Sign returns "sha256=<hex>" of HMAC-SHA256(secret, ts + "." + body), the same scheme as the
// forms gateway so one verifier serves both.
func Sign(secret []byte, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Dispatcher delivers outbox events to one webhook URL, signed, with exponential backoff.
type Dispatcher struct {
	URL    string
	Secret []byte
	Client *http.Client
	Log    *slog.Logger
	// Can, when set, publishes through can-core (POST /v1/events) instead of posting to URL: the
	// module-to-module path goes through can-core only.
	Can *modclient.Client
}

// NewDispatcher builds a dispatcher whose client never follows redirects.
func NewDispatcher(url string, secret []byte, log *slog.Logger) *Dispatcher {
	return &Dispatcher{URL: url, Secret: secret, Log: log, Client: &http.Client{
		Timeout:       outboxHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// DeliverPending sends due events and prunes old delivered ones. It returns the number sent.
func (s *Service) DeliverPending(ctx context.Context, d *Dispatcher) (int, error) {
	now := s.now().Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT seq, id, type, payload, attempts FROM outbox
		WHERE delivered_at IS NULL AND attempts < ? AND next_attempt <= ? ORDER BY seq LIMIT ?`, maxDeliveryAttempts, now, outboxBatch)
	if err != nil {
		return 0, err
	}
	type item struct {
		seq      int64
		id, typ  string
		payload  string
		attempts int
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.seq, &it.id, &it.typ, &it.payload, &it.attempts); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	_ = rows.Close()
	sent := 0
	for _, it := range items {
		derr := d.post(ctx, it.id, it.typ, []byte(it.payload), s.now())
		if derr == nil {
			sent++
			_, err = s.db.ExecContext(ctx, `UPDATE outbox SET delivered_at = ?, attempts = attempts + 1, last_error = '' WHERE seq = ?`, now, it.seq)
		} else {
			backoff := int64(1) << min(it.attempts+2, 14) // 4s, 8s, ... capped near 4.5h
			_, err = s.db.ExecContext(ctx, `UPDATE outbox SET attempts = attempts + 1, next_attempt = ?, last_error = ? WHERE seq = ?`, now+backoff, truncate(derr.Error(), 200), it.seq)
		}
		if err != nil {
			return sent, err
		}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM outbox WHERE delivered_at IS NOT NULL AND delivered_at < ?`, now-int64(outboxDeliveredRetention.Seconds()))
	return sent, err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (d *Dispatcher) post(ctx context.Context, id, typ string, body []byte, now time.Time) error {
	if d.Can != nil {
		return d.Can.PublishEnvelope(ctx, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := now.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Can-Event-Id", id)
	req.Header.Set("X-Can-Event-Type", typ)
	req.Header.Set("X-Can-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Can-Signature", Sign(d.Secret, ts, body))
	resp, err := d.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, outboxMaxResponseDrainLen))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}

// PendingEvents returns the undelivered outbox events (tests and diagnostics).
func (s *Service) PendingEvents(ctx context.Context) ([]Event, error) {
	return s.queryEvents(ctx, `SELECT payload FROM outbox WHERE delivered_at IS NULL ORDER BY seq`)
}

// AllEvents returns every outbox event in order (tests).
func (s *Service) AllEvents(ctx context.Context) ([]Event, error) {
	return s.queryEvents(ctx, `SELECT payload FROM outbox ORDER BY seq`)
}

func (s *Service) queryEvents(ctx context.Context, q string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		var ev Event
		if err := json.Unmarshal([]byte(p), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

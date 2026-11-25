package equity

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Event types. Payload schemas: schemas/events/<type>.json. Payloads carry opaque ids and dates
// only: never names, quantities, prices or valuations (SPEC.md sections 3.7 and 4.3, derived
// copies), so the outbox is not a copy of equity data.
const (
	EventGrantApproved    = "equity.grant.approved.v1"
	EventGrantVested      = "equity.grant.vested.v1"
	EventExerciseRecorded = "equity.exercise.recorded.v1"

	eventSource               = "equity"
	maxDeliveryAttempts       = 8
	outboxDeliveredRetention  = 7 * 24 * time.Hour
	outboxBatch               = 20
	outboxHTTPTimeout         = 10 * time.Second
	outboxMaxResponseDrainLen = 4096
)

// Event is the platform envelope (SPEC.md section 4.3).
type Event struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Time   time.Time      `json:"time"`
	Source string         `json:"source"`
	Data   map[string]any `json:"data"`
}

// enqueue stores an event in the outbox inside the caller's transaction, so the event exists if
// and only if the change committed.
func (s *Service) enqueue(ctx context.Context, q querier, typ string, data map[string]any) error {
	ev := Event{ID: newID("evt"), Type: typ, Time: s.now().UTC(), Source: eventSource, Data: data}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO outbox (id, type, payload, created_at) VALUES (?,?,?,?)`, ev.ID, typ, string(b), s.now().Unix())
	return err
}

// Sign returns "sha256=<hex>" of HMAC-SHA256(secret, ts + "." + body), the scheme of the forms
// gateway and people service, so one verifier serves all.
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
			backoff := int64(1) << min(it.attempts+2, 14)
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func isUnique(err error) bool { return err != nil && strings.Contains(err.Error(), "UNIQUE") }

func (d *Dispatcher) post(ctx context.Context, id, typ string, body []byte, now time.Time) error {
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

// ApplyDue is the periodic sweep: it expires active grants whose exercise window or expiry has
// passed (releasing their unexercised options to the pool) and announces newly vested
// instalments with equity.grant.vested.v1 (ids and the date vested through; no quantities).
// It returns the number of grants changed. Safe to run any time and as often as wanted.
func (s *Service) ApplyDue(ctx context.Context) (int, error) {
	today := s.today()
	rows, err := s.db.QueryContext(ctx, `SELECT `+grantCols+` FROM grants g WHERE g.status = 'active' LIMIT 100000`)
	if err != nil {
		return 0, err
	}
	var gs []*grantRow
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		gs = append(gs, g)
	}
	_ = rows.Close()
	changed := 0
	for _, g := range gs {
		if end := windowEnd(&g.Grant); today > end {
			terminal := end
			if t, err := time.Parse(dateLayout, end); err == nil {
				terminal = t.AddDate(0, 0, 1).Format(dateLayout)
			}
			err := s.tx(ctx, func(ctx context.Context, tx *sqlTx) error {
				if err := s.cancelPendingExercises(ctx, tx, g.ID, "expired"); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `UPDATE grants SET status = 'expired', terminal_on = ?, terminal_reason = 'exercise window ended', updated_at = ? WHERE id = ? AND status = 'active'`, terminal, s.now().Unix(), g.ID)
				return err
			})
			if err != nil {
				return changed, err
			}
			changed++
			continue
		}
		in, err := s.vestInputFor(ctx, s.db, &g.Grant, today)
		if err != nil {
			return changed, err
		}
		v, err := Vested(in)
		if err != nil {
			return changed, err
		}
		var notified int64
		if err := s.db.QueryRowContext(ctx, `SELECT vested_notified FROM grants WHERE id = ?`, g.ID).Scan(&notified); err != nil {
			return changed, err
		}
		if int64(v) > notified {
			err := s.tx(ctx, func(ctx context.Context, tx *sqlTx) error {
				res, err := tx.ExecContext(ctx, `UPDATE grants SET vested_notified = ? WHERE id = ? AND vested_notified = ?`, int64(v), g.ID, notified)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					return nil // another sweep announced it
				}
				return s.enqueue(ctx, tx, EventGrantVested, map[string]any{"grant_id": g.ID, "stakeholder_id": g.StakeholderID, "entity_id": g.EntityID, "vested_through": today})
			})
			if err != nil {
				return changed, err
			}
			changed++
		}
	}
	return changed, s.pruneIdempotency(ctx)
}

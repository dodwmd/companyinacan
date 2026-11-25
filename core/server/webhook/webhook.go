// Package webhook signs and delivers events to module webhooks (SPEC §3.1, §4.3).
//
// Wire format of a delivery:
//
//	POST <module webhook URL>
//	X-Can-Event-Id: evt_...          idempotency key, stable across retries
//	X-Can-Event-Type: lead.created.v1
//	X-Can-Attempt: 1
//	X-Can-Timestamp: <unix seconds>
//	X-Can-Signature: v1=<hex HMAC-SHA256(secret, "<timestamp>." + body)>
//
// Receivers recompute the MAC and reject timestamps outside a replay window (Verify).
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Header names.
const (
	HeaderEventID   = "X-Can-Event-Id"
	HeaderEventType = "X-Can-Event-Type"
	HeaderAttempt   = "X-Can-Attempt"
	HeaderTimestamp = "X-Can-Timestamp"
	HeaderSignature = "X-Can-Signature"
)

// DefaultReplayWindow is how far a timestamp may be from the receiver's clock.
const DefaultReplayWindow = 5 * time.Minute

// Verification errors.
var (
	ErrBadSignature = errors.New("webhook signature mismatch")
	ErrStale        = errors.New("webhook timestamp outside the replay window")
	ErrMalformed    = errors.New("webhook signature headers missing or malformed")
)

// Sign returns the X-Can-Signature value for body sent at ts.
func Sign(secret string, ts time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is the receiver side: it checks the headers against body and rejects replays.
func Verify(secret string, h http.Header, body []byte, now time.Time, window time.Duration) error {
	tsRaw, sig := h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || !strings.HasPrefix(sig, "v1=") {
		return ErrMalformed
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "v1="))
	if err != nil {
		return ErrMalformed
	}
	want, _ := hex.DecodeString(strings.TrimPrefix(Sign(secret, time.Unix(ts, 0), body), "v1="))
	if !hmac.Equal(got, want) {
		return ErrBadSignature
	}
	d := now.Sub(time.Unix(ts, 0))
	if d < 0 {
		d = -d
	}
	if d > window {
		return ErrStale
	}
	return nil
}

// DefaultBackoff is 5s doubling per attempt, capped at one hour.
func DefaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 12 {
		return time.Hour
	}
	d := 5 * time.Second << (attempt - 1)
	if d > time.Hour {
		return time.Hour
	}
	return d
}

// Doer is the HTTP client interface (http.Client satisfies it).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// NewHTTPClient returns a client that never follows redirects: a redirect would forward the
// signed body (and headers) to a host the manifest never named.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Dispatcher delivers due outbox rows.
type Dispatcher struct {
	db          *store.DB
	client      Doer
	Now         func() time.Time
	Backoff     func(attempt int) time.Duration
	MaxAttempts int
	BatchSize   int
	Log         *slog.Logger
}

// NewDispatcher returns a dispatcher with default policy: 8 attempts over roughly 10 minutes of
// cumulative backoff growing to one hour, then the delivery is marked dead (visible in the log).
func NewDispatcher(db *store.DB, client Doer) *Dispatcher {
	return &Dispatcher{db: db, client: client, Now: time.Now, Backoff: DefaultBackoff, MaxAttempts: 8, BatchSize: 50, Log: slog.Default()}
}

// RunOnce attempts every due delivery once and returns how many were attempted.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	due, err := d.db.DueDeliveries(ctx, d.Now(), d.BatchSize)
	if err != nil {
		return 0, err
	}
	for _, del := range due {
		if err := d.attempt(ctx, del); err != nil {
			return 0, err
		}
	}
	return len(due), nil
}

// send performs one signed POST and returns the HTTP status (0 if none) and an error description
// ("" on a 2xx).
func (d *Dispatcher) send(ctx context.Context, del store.Delivery, n int, start time.Time) (int, string) {
	if del.HMACSecret == "" {
		return 0, "module has no signing secret; refusing to send an unsigned webhook"
	}
	body := events.FromStored(del.Event).JSON()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, del.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "can-core")
	req.Header.Set(HeaderEventID, del.Event.ID)
	req.Header.Set(HeaderEventType, del.Event.Type)
	req.Header.Set(HeaderAttempt, strconv.Itoa(n))
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(start.Unix(), 10))
	req.Header.Set(HeaderSignature, Sign(del.HMACSecret, start, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Sprintf("subscriber answered HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, ""
}

// deliveries counts delivery attempts by outcome (delivered, retry, dead). The label set is fixed;
// event ids, module ids and URLs never reach a metric.
var deliveries = obs.Default.Counter("can_webhook_deliveries_total", "Webhook delivery attempts by result.", "result")

func (d *Dispatcher) attempt(ctx context.Context, del store.Delivery) error {
	start := d.Now()
	n := del.Attempts + 1
	rec := store.Attempt{At: start}
	rec.StatusCode, rec.Error = d.send(ctx, del, n, start)
	rec.Duration = d.Now().Sub(start)
	status, next := store.StatusDelivered, start
	result := "delivered"
	if rec.Error != "" {
		status, next = store.StatusPending, start.Add(d.Backoff(n))
		result = "retry"
		if n >= d.MaxAttempts {
			status = store.StatusDead
			result = "dead"
			d.Log.Warn("webhook delivery gave up", "module", del.ModuleID, "event", del.Event.ID, "attempts", n)
		}
	}
	err := d.db.RecordAttempt(ctx, del.ID, rec, status, next)
	if err == nil {
		deliveries.With(result).Inc()
	}
	return err
}

// Run processes deliveries until ctx ends: on every wake signal and at least every interval (to
// pick up retries that have become due and rows left over from before a restart).
func (d *Dispatcher) Run(ctx context.Context, wake <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := d.RunOnce(ctx); err != nil && ctx.Err() == nil {
			d.Log.Error("webhook dispatch failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-t.C:
		}
	}
}

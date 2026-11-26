package forms

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modclient"
)

// Email is a message handed to the email.send capability. The sender address is can-core's.
type Email = modclient.Email

// Mailer sends email through the email.send capability. Tests use a fake.
type Mailer interface {
	Send(ctx context.Context, e Email) error
}

// CanMailer sends through can-core with the module's projected token (an installed module).
type CanMailer struct{ Client *modclient.Client }

// Send implements Mailer.
func (m *CanMailer) Send(ctx context.Context, e Email) error { return m.Client.SendEmail(ctx, e) }

// HTTPMailer calls an email.send REST endpoint with a static bearer token (email.url in the
// config; the Docker Compose stack uses it).
type HTTPMailer struct {
	URL    string
	Token  string
	Client *http.Client
}

// Send implements Mailer. Errors worth retrying wrap modclient.ErrUnavailable.
func (m *HTTPMailer) Send(ctx context.Context, e Email) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.Token)
	}
	client := m.Client
	if client == nil {
		client = outboundClient(10 * time.Second)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", modclient.ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	return fmt.Errorf("email.send: %w", modclient.NewStatusError(resp.StatusCode, resp.Header, ""))
}

// Limits of one email.send request (can-core refuses more).
const (
	maxEmailRecipients = 10
	maxEmailSubject    = 200
	maxEmailText       = 60000
)

// cut shortens s to at most n bytes without splitting a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// plain keeps line breaks and tabs and blanks every other control character.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
}

// BuildNotification renders the notification email for a stored lead. The subject only
// contains the operator-defined form name (single line); submitter input appears in the
// plain-text body only.
func BuildNotification(f *Form, l *Lead) Email {
	var b strings.Builder
	fmt.Fprintf(&b, "A new submission arrived on the form %q.\n\n", oneLine(f.Name))
	order := make([]string, 0, len(l.Fields))
	seen := map[string]bool{}
	for _, fl := range f.Fields {
		if _, ok := l.Fields[fl.Name]; ok {
			order = append(order, fl.Name)
			seen[fl.Name] = true
		}
	}
	var rest []string
	for k := range l.Fields {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range append(order, rest...) {
		fmt.Fprintf(&b, "%s: %s\n", k, l.Fields[k])
	}
	text := plain(b.String())
	if len(text) > maxEmailText {
		text = cut(text, maxEmailText) + "\n(shortened; the whole submission is in the leads view)\n"
	}
	text += fmt.Sprintf("\nLead id: %s\n", l.ID)
	// The recipients are the operator's notify_email and nothing else. The visitor's address is only
	// ever a Reply-To, and only when it is a plain address.
	reply := ""
	if config.ValidAddress(l.Email) {
		reply = l.Email
	}
	return Email{To: f.NotifyEmail, ReplyTo: reply, Subject: cut(plain(oneLine("New "+f.Name+" submission")), maxEmailSubject), Text: text, SourceID: "lead:" + l.ID}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Sign returns the webhook signature "sha256=<hex>" of HMAC-SHA256(secret, ts + "." + body).
func Sign(secret []byte, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// VerifySignature is the receiver side of Sign: constant-time compare plus a replay window.
func VerifySignature(secret []byte, tsHeader, sigHeader string, body []byte, now time.Time, tolerance time.Duration) error {
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return errors.New("timestamp outside tolerance")
	}
	if !hmac.Equal([]byte(sigHeader), []byte(Sign(secret, ts, body))) {
		return errors.New("signature mismatch")
	}
	return nil
}

// WebhookSender posts events to one URL, signed, with exponential-backoff retries.
// Retries cover network errors, 5xx and 429; other 4xx are permanent. Redirects are not
// followed.
type WebhookSender struct {
	URL         string
	Secret      []byte
	MaxAttempts int
	Backoff     time.Duration // delay before the second attempt; doubles each time
	Client      *http.Client
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
}

const maxBackoff = 5 * time.Minute

// Send delivers ev and reports how many attempts were made.
func (w *WebhookSender) Send(ctx context.Context, ev Event) (int, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	max := max(w.MaxAttempts, 1)
	backoff := w.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	var last error
	for attempt := 1; attempt <= max; attempt++ {
		retry, err := w.once(ctx, ev, body)
		if err == nil {
			return attempt, nil
		}
		last = err
		if !retry || attempt == max {
			return attempt, last
		}
		if err := w.sleep(ctx, backoff); err != nil {
			return attempt, fmt.Errorf("%w (last error: %v)", err, last)
		}
		backoff = min(backoff*2, maxBackoff)
	}
	return max, last
}

func (w *WebhookSender) sleep(ctx context.Context, d time.Duration) error {
	if w.Sleep != nil {
		return w.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (w *WebhookSender) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return outboundClient(10 * time.Second)
}

func (w *WebhookSender) once(ctx context.Context, ev Event, body []byte) (retry bool, err error) {
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	ts := now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "can-forms")
	req.Header.Set("X-Can-Event-Id", ev.ID)
	req.Header.Set("X-Can-Event-Type", ev.Type)
	req.Header.Set("X-Can-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Can-Signature", Sign(w.Secret, ts, body))
	resp, err := w.client().Do(req)
	if err != nil {
		return ctx.Err() == nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("webhook: status %d", resp.StatusCode)
	}
	return false, fmt.Errorf("webhook: status %d", resp.StatusCode)
}

type job struct {
	lead *Lead
	form *Form
}

// Dispatcher runs notifications (email, webhook) off the request path with a bounded queue
// and a small worker pool, and records each outcome. A failed notification never loses
// the lead: it is already stored.
type Dispatcher struct {
	store   *Store
	mailer  Mailer
	webhook *WebhookSender
	// can publishes lead.created.v1 through can-core when the module is installed.
	can   *modclient.Client
	sleep func(context.Context, time.Duration) error
	log   *slog.Logger
	now   func() time.Time

	// forms looks a form definition up for a retry; emailPausedUntil belongs to the retry loop.
	forms            func(string) *Form
	emailPausedUntil time.Time

	queue   chan job
	pending sync.WaitGroup
	workers sync.WaitGroup
	mu      sync.Mutex
	closed  bool
}

// NewDispatcher builds a dispatcher; mailer and webhook may be nil (channel disabled).
func NewDispatcher(store *Store, mailer Mailer, webhook *WebhookSender, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{store: store, mailer: mailer, webhook: webhook, log: log, now: time.Now, queue: make(chan job, 256)}
}

// Start launches n workers.
func (d *Dispatcher) Start(n int) {
	for i := 0; i < n; i++ {
		d.workers.Add(1)
		go func() {
			defer d.workers.Done()
			for j := range d.queue {
				d.run(j)
				d.pending.Done()
			}
		}()
	}
}

// Enqueue schedules notifications for a stored lead. It never blocks: when the queue is
// full (or the dispatcher is shut down) it returns false and the lead stays stored.
func (d *Dispatcher) Enqueue(l *Lead, f *Form) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	d.pending.Add(1)
	select {
	case d.queue <- job{l, f}:
		return true
	default:
		d.pending.Done()
		d.log.Warn("notification queue full; lead stored without notification", "lead", l.ID)
		return false
	}
}

// Flush waits until everything queued so far has been processed.
func (d *Dispatcher) Flush() { d.pending.Wait() }

// Shutdown stops accepting work, drains the queue and waits for the workers, or until ctx ends.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()
	done := make(chan struct{})
	go func() { d.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Dispatcher) run(j job) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if d.mailer != nil && len(j.form.NotifyEmail) > 0 {
		d.deliverEmail(ctx, j.lead, j.form)
	}
	if d.webhook != nil {
		n, err := d.webhook.Send(ctx, NewLeadCreated(j.lead, d.now()))
		d.record(ctx, j.lead.ID, "webhook", n, err)
	}
	if d.can != nil {
		sleep := d.sleep
		if sleep == nil {
			sleep = sleepCtx
		}
		n, err := publishCan(ctx, d.can, NewLeadCreated(j.lead, d.now()), sleep)
		d.record(ctx, j.lead.ID, "webhook", n, err) // the event channel; the delivery log has two kinds
	}
}

// sendEmail hands the notification to email.send, in requests of at most maxEmailRecipients, and
// retries briefly while it is unavailable and did not say to wait; the source id makes a repeat safe.
// Longer outages are the business of the retry loop (emailqueue.go).
func (d *Dispatcher) sendEmail(ctx context.Context, e Email) (int, error) {
	sleep := d.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	to, id, attempts := e.To, e.SourceID, 0
	for part := 0; len(to) > 0; part++ {
		n := min(len(to), maxEmailRecipients)
		e.To, to = to[:n], to[n:]
		if part > 0 && id != "" {
			e.SourceID = id + "." + strconv.Itoa(part)
		}
		backoff := time.Second
		for try := 1; ; try++ {
			attempts++
			err := d.mailer.Send(ctx, e)
			if err == nil {
				break
			}
			if !errors.Is(err, modclient.ErrUnavailable) || modclient.RetryAfter(err) > 0 || try == 3 {
				return attempts, err
			}
			if serr := sleep(ctx, backoff); serr != nil {
				return attempts, serr
			}
			backoff *= 2
		}
	}
	return attempts, nil
}

func (d *Dispatcher) record(ctx context.Context, leadID, kind string, attempts int, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
		d.log.Warn("notification failed", "kind", kind, "lead", leadID, "attempts", attempts, "error", msg)
	}
	if rerr := d.store.RecordDelivery(ctx, leadID, kind, err == nil, attempts, msg); rerr != nil {
		d.log.Error("record delivery", "error", rerr)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

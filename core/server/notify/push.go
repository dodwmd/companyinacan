package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/server/notify/webpush"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// ChannelWebPush is the Web Push channel (specs/025-pwa-webpush).
const ChannelWebPush = "webpush"

// Web Push limits.
const (
	MaxSubscriptionsPerUser = 10
	maxPushFailures         = 10 // consecutive failed deliveries before a device is dropped
	maxPushAttempts         = 3
	maxPushInFlight         = 8
	pushTimeout             = 30 * time.Second
	testPushInterval        = 30 * time.Second
	defaultPushContact      = "https://github.com/dodwmd/companyinacan"
)

// Errors of the push API.
var (
	ErrTooManySubscriptions = errors.New("too many push subscriptions for this user")
	ErrNoSubscriptions      = errors.New("no push subscriptions")
	ErrPushRateLimited      = errors.New("test notifications are rate limited")
	ErrKeyFile              = errors.New("the VAPID key comes from CAN_VAPID_KEY_FILE; rotate the mounted Secret instead")
)

// PushSender sends one encrypted message. webpush.Client satisfies it; tests inject a fake.
type PushSender interface {
	Send(ctx context.Context, sub webpush.Subscription, payload []byte, o webpush.Options) (webpush.Result, error)
}

var _ PushSender = (*webpush.Client)(nil)

// push is the lazily created Web Push state. Nothing here exists, runs or allocates until a user
// subscribes or asks for the public key.
type push struct {
	mu     sync.Mutex
	sender PushSender
	key    *webpush.Key
	wg     sync.WaitGroup
	sem    chan struct{}
	lastT  map[string]time.Time
}

func (s *Service) pushSender(ctx context.Context) (PushSender, error) {
	if s.opt.PushSender != nil {
		return s.opt.PushSender, nil
	}
	k, err := s.vapidKey(ctx)
	if err != nil {
		return nil, err
	}
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if s.push.sender == nil {
		contact := s.opt.PushContact
		if contact == "" {
			contact = defaultPushContact
		}
		c := webpush.NewClient(k, contact)
		if s.opt.PushHTTP != nil {
			c.HTTP = s.opt.PushHTTP
		}
		c.Now = s.opt.Now
		c.AllowHosts, c.AllowInsecure = s.opt.PushEndpointAllow, s.opt.AllowInsecurePush
		s.push.sender = c
	}
	return s.push.sender, nil
}

// vapidKey returns the application server key: the mounted file when configured, otherwise the one
// stored in the state database, generated on first use.
func (s *Service) vapidKey(ctx context.Context) (*webpush.Key, error) {
	s.push.mu.Lock()
	defer s.push.mu.Unlock()
	if s.push.key != nil {
		return s.push.key, nil
	}
	if f := s.opt.VAPIDKeyFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading the VAPID key file: %w", err)
		}
		k, err := webpush.ParseKey(string(b))
		if err != nil {
			return nil, err
		}
		s.push.key = k
		return k, nil
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT private_key FROM push_vapid WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		k, gerr := webpush.GenerateKey()
		if gerr != nil {
			return nil, gerr
		}
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO push_vapid(id, private_key, created_at) VALUES (1,?,?)`, k.Marshal(), s.opt.Now().Unix()); err != nil {
			return nil, err
		}
		err = s.db.QueryRowContext(ctx, `SELECT private_key FROM push_vapid WHERE id=1`).Scan(&raw)
	}
	if err != nil {
		return nil, err
	}
	k, err := webpush.ParseKey(raw)
	if err != nil {
		return nil, err
	}
	s.push.key = k
	return k, nil
}

// VAPIDPublicKey returns the applicationServerKey browsers subscribe with, creating the key pair on
// first use.
func (s *Service) VAPIDPublicKey(ctx context.Context) (string, error) {
	k, err := s.vapidKey(ctx)
	if err != nil {
		return "", err
	}
	return k.PublicKey(), nil
}

// RotateVAPID replaces the stored key pair and drops every subscription: a browser binds a
// subscription to the public key it was made with, so nothing could be delivered to old ones.
// Users enable push again on each device. Refused when the key comes from a mounted file.
func (s *Service) RotateVAPID(ctx context.Context) error {
	if s.opt.VAPIDKeyFile != "" {
		return ErrKeyFile
	}
	k, err := webpush.GenerateKey()
	if err != nil {
		return err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO push_vapid(id, private_key, created_at) VALUES (1,?,?)
			ON CONFLICT(id) DO UPDATE SET private_key=excluded.private_key, created_at=excluded.created_at`, k.Marshal(), s.opt.Now().Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions`)
		return err
	})
	if err != nil {
		return err
	}
	s.push.mu.Lock()
	s.push.key, s.push.sender = nil, nil
	s.push.mu.Unlock()
	return nil
}

// --- subscriptions --------------------------------------------------------------------------------

// PushDevice is one subscribed device as shown to its owner (never the endpoint or keys).
type PushDevice struct {
	ID        int64      `json:"id"`
	Label     string     `json:"label"`
	Service   string     `json:"service"` // host of the push service, e.g. fcm.googleapis.com
	CreatedAt time.Time  `json:"created_at"`
	LastOK    *time.Time `json:"last_ok,omitempty"`
}

// Subscribe stores a browser push subscription for subject. The endpoint is validated (https, port
// 443, a public DNS name; see webpush.Subscription.Validate). A user holds at most
// MaxSubscriptionsPerUser devices; re-subscribing a known endpoint refreshes it. An endpoint that
// belonged to another user moves to the caller (the same browser signed in as someone else).
func (s *Service) Subscribe(ctx context.Context, subject string, sub webpush.Subscription, label string) (int64, error) {
	if !rbac.ValidSubject(subject) {
		return 0, fmt.Errorf("%w: subject", errInvalid)
	}
	if err := sub.ValidateWith(s.opt.AllowInsecurePush, s.opt.PushEndpointAllow); err != nil {
		return 0, fmt.Errorf("%w: %v", errInvalid, err)
	}
	label = oneLine(label, 80)
	var id int64
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT subject FROM push_subscriptions WHERE endpoint=?`, sub.Endpoint).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if owner != subject {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_subscriptions WHERE subject=?`, subject).Scan(&n); err != nil {
				return err
			}
			if n >= MaxSubscriptionsPerUser {
				return ErrTooManySubscriptions
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO push_subscriptions(subject, endpoint, p256dh, auth, label, created_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT(endpoint) DO UPDATE SET subject=excluded.subject, p256dh=excluded.p256dh, auth=excluded.auth, label=excluded.label, fail_count=0`,
			subject, sub.Endpoint, sub.P256dh, sub.Auth, label, s.opt.Now().Unix()); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM push_subscriptions WHERE endpoint=?`, sub.Endpoint).Scan(&id)
	})
	return id, err
}

// Unsubscribe removes the caller's subscription for endpoint; store.ErrNotFound when the caller
// has none (including when the endpoint belongs to someone else).
func (s *Service) Unsubscribe(ctx context.Context, subject, endpoint string) error {
	return s.deleteSub(ctx, `DELETE FROM push_subscriptions WHERE subject=? AND endpoint=?`, subject, endpoint)
}

// RemoveSubscription removes one of the caller's devices by id.
func (s *Service) RemoveSubscription(ctx context.Context, subject string, id int64) error {
	return s.deleteSub(ctx, `DELETE FROM push_subscriptions WHERE subject=? AND id=?`, subject, id)
}

func (s *Service) deleteSub(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Subscriptions lists the caller's devices.
func (s *Service) Subscriptions(ctx context.Context, subject string) ([]PushDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, endpoint, label, created_at, last_ok_at FROM push_subscriptions WHERE subject=? ORDER BY id LIMIT ?`, subject, MaxSubscriptionsPerUser)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []PushDevice{}
	for rows.Next() {
		var d PushDevice
		var ep string
		var created, ok int64
		if err := rows.Scan(&d.ID, &ep, &d.Label, &created, &ok); err != nil {
			return nil, err
		}
		if u, err := url.Parse(ep); err == nil {
			d.Service = u.Hostname()
		}
		d.CreatedAt = time.Unix(created, 0)
		if ok > 0 {
			t := time.Unix(ok, 0)
			d.LastOK = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type subRow struct {
	id  int64
	sub webpush.Subscription
}

func (s *Service) subsOf(ctx context.Context, subject string) ([]subRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, endpoint, p256dh, auth FROM push_subscriptions WHERE subject=? ORDER BY id LIMIT ?`, subject, MaxSubscriptionsPerUser)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []subRow
	for rows.Next() {
		var r subRow
		if err := rows.Scan(&r.id, &r.sub.Endpoint, &r.sub.P256dh, &r.sub.Auth); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var (
	chromeRe = regexp.MustCompile(`Chrome/|CriOS/`)
	osNames  = []struct{ needle, name string }{{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"}, {"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"}}
)

// DeviceLabel turns a User-Agent into a short label from a fixed vocabulary, so no attacker text is
// stored or shown.
func DeviceLabel(ua string) string {
	browser := "Browser"
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "SamsungBrowser/"):
		browser = "Samsung Internet"
	case chromeRe.MatchString(ua):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	for _, o := range osNames {
		if strings.Contains(ua, o.needle) {
			return browser + " on " + o.name
		}
	}
	return browser
}

// --- delivery -------------------------------------------------------------------------------------

var pushTitles = map[string]string{
	"approval.requested": "Approval waiting",
	"approval.escalated": "Approval escalated",
	"approval.decided":   "Approval decided",
	"lead.created":       "New lead",
}

var pushLinkRe = regexp.MustCompile(`^/[A-Za-z0-9_./-]{0,100}$`)

// pushPayload is what leaves the server for the push service (encrypted, but still minimal): a
// generic title by notification type, a fixed body and a site path. Never the notification's data,
// names, amounts or text; the page behind the link shows those after sign-in.
func pushPayload(typ, link string, id int64) []byte {
	title := pushTitles[typ]
	if title == "" {
		title = "New notification"
	}
	if !pushLinkRe.MatchString(link) || strings.Contains(link, "//") || strings.Contains(link, "..") {
		link = "/notifications"
	}
	b, _ := json.Marshal(map[string]any{"t": title, "b": "Open the console to review.", "u": link, "i": id})
	return b
}

func (s *Service) deliverPush(ctx context.Context, id int64, n Notice) {
	subs, err := s.subsOf(ctx, n.Subject)
	if err != nil {
		s.opt.Log.Error("reading push subscriptions failed", "error", err)
		return
	}
	if len(subs) == 0 {
		return // nothing is created, logged or started for a user without devices
	}
	on, err := s.prefEnabled(ctx, n.Subject, n.Type, ChannelWebPush)
	if err != nil {
		s.opt.Log.Error("reading notification preferences failed", "error", err)
		return
	}
	if !on {
		s.logDelivery(ctx, id, ChannelWebPush, "suppressed", "preference")
		return
	}
	set, err := s.Settings(ctx, n.Subject)
	if err != nil {
		s.opt.Log.Error("reading notification settings failed", "error", err)
		return
	}
	// Push is real time, so a digest setting (which batches email) does not hold it. Quiet hours do,
	// except for urgent notices; the notice is in the inbox either way.
	now := s.opt.Now()
	if !n.Urgent && inQuiet(now, set.QuietStart, set.QuietEnd, set.TZ) {
		s.logDelivery(ctx, id, ChannelWebPush, "suppressed", "quiet hours")
		return
	}
	if !n.Urgent {
		many, err := s.burst(ctx, n.Subject, n.Type)
		if err != nil {
			s.opt.Log.Error("burst lookup failed", "error", err)
			return
		}
		if many {
			s.logDelivery(ctx, id, ChannelWebPush, "suppressed", "burst")
			return
		}
	}
	cnt, err := s.externalCount(ctx, n.Subject, ChannelWebPush, n.Module, n.Type, now.Add(-time.Hour))
	if err != nil {
		s.opt.Log.Error("rate limit lookup failed", "error", err)
		return
	}
	if cnt >= s.opt.RateLimitPerHour {
		s.logDelivery(ctx, id, ChannelWebPush, "suppressed", "rate limited")
		return
	}
	snd, err := s.pushSender(ctx)
	if err != nil {
		s.logDelivery(ctx, id, ChannelWebPush, "failed", "push is not configured")
		s.opt.Log.Error("web push unavailable", "error", err)
		return
	}
	select {
	case s.push.sem <- struct{}{}:
	default:
		s.logDelivery(ctx, id, ChannelWebPush, "suppressed", "push queue is full")
		return
	}
	o := webpush.Options{TTL: 3600, Urgency: webpush.UrgencyNormal}
	if n.Urgent {
		o = webpush.Options{TTL: 24 * 3600, Urgency: webpush.UrgencyHigh}
	}
	payload := pushPayload(n.Type, n.Link, id)
	bg := context.WithoutCancel(ctx)
	s.push.wg.Add(1)
	go func() {
		defer s.push.wg.Done()
		defer func() { <-s.push.sem }()
		cctx, cancel := context.WithTimeout(bg, pushTimeout)
		defer cancel()
		ok, detail := s.fanOut(cctx, snd, subs, payload, o)
		if ok == 0 {
			s.logDelivery(bg, id, ChannelWebPush, "failed", detail)
			return
		}
		s.logDelivery(bg, id, ChannelWebPush, "sent", detail)
		s.emitSent(bg, id, n.Subject, n.Type, ChannelWebPush)
	}()
}

// fanOut sends to every device in parallel and returns how many accepted it.
func (s *Service) fanOut(ctx context.Context, snd PushSender, subs []subRow, payload []byte, o webpush.Options) (int, string) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	var last string
	for _, r := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.sendOne(ctx, snd, r, payload, o)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				last = err.Error()
			}
		}()
	}
	wg.Wait()
	if ok == 0 {
		return 0, fmt.Sprintf("0 of %d devices accepted: %s", len(subs), last)
	}
	return ok, fmt.Sprintf("%d of %d devices", ok, len(subs))
}

// sendOne delivers to one device with bounded retries (429 and 5xx, honouring Retry-After) and
// maintains the row: 404/410 delete it, repeated failures drop it, success resets the count.
func (s *Service) sendOne(ctx context.Context, snd PushSender, r subRow, payload []byte, o webpush.Options) error {
	var err error
	for attempt := 0; attempt < maxPushAttempts; attempt++ {
		var res webpush.Result
		res, err = snd.Send(ctx, r.sub, payload, o)
		if err == nil {
			_, _ = s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_ok_at=?, fail_count=0 WHERE id=?`, s.opt.Now().Unix(), r.id)
			return nil
		}
		if errors.Is(err, webpush.ErrGone) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id=?`, r.id)
			return errors.New("subscription expired and was removed")
		}
		if !res.Retryable || attempt == maxPushAttempts-1 {
			break
		}
		wait := res.RetryAfter
		if wait <= 0 {
			wait = time.Duration(1<<attempt) * time.Second
		}
		if serr := s.opt.PushSleep(ctx, wait); serr != nil {
			break
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE push_subscriptions SET fail_count=fail_count+1 WHERE id=?`, r.id)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id=? AND fail_count>=?`, r.id, maxPushFailures)
	return err
}

// WaitPush waits for in-flight push deliveries (tests and shutdown).
func (s *Service) WaitPush() { s.push.wg.Wait() }

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

// SendTest sends a generic test notification to each of the caller's devices once (no retries) and
// returns how many accepted it. At most one test per user per 30 seconds.
func (s *Service) SendTest(ctx context.Context, subject string) (int, error) {
	subs, err := s.subsOf(ctx, subject)
	if err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, ErrNoSubscriptions
	}
	now := s.opt.Now()
	s.push.mu.Lock()
	if last, ok := s.push.lastT[subject]; ok && now.Sub(last) < testPushInterval {
		s.push.mu.Unlock()
		return 0, ErrPushRateLimited
	}
	if len(s.push.lastT) > 1000 {
		clear(s.push.lastT)
	}
	s.push.lastT[subject] = now
	s.push.mu.Unlock()
	snd, err := s.pushSender(ctx)
	if err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"t": "Test notification", "b": "Push is working on this device.", "u": "/notifications", "i": 0})
	ok := 0
	for _, r := range subs {
		if s.sendOneNoRetry(cctx, snd, r, payload) == nil {
			ok++
		}
	}
	if ok == 0 {
		return 0, errors.New("no device accepted the test notification")
	}
	return ok, nil
}

func (s *Service) sendOneNoRetry(ctx context.Context, snd PushSender, r subRow, payload []byte) error {
	_, err := snd.Send(ctx, r.sub, payload, webpush.Options{TTL: 60, Urgency: webpush.UrgencyHigh, Topic: "test"})
	switch {
	case err == nil:
		_, _ = s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_ok_at=?, fail_count=0 WHERE id=?`, s.opt.Now().Unix(), r.id)
	case errors.Is(err, webpush.ErrGone):
		_, _ = s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id=?`, r.id)
	}
	return err
}

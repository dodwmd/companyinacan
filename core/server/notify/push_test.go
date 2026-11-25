package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/notify/webpush"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// fakeSender is a PushSender recording calls; results are scripted per endpoint.
type fakeSender struct {
	mu      sync.Mutex
	calls   []fakeCall
	results map[string][]fakeResult // endpoint -> successive answers; the last repeats
}

type fakeCall struct {
	Endpoint string
	Payload  string
	Opt      webpush.Options
}

type fakeResult struct {
	res webpush.Result
	err error
}

func (f *fakeSender) Send(_ context.Context, sub webpush.Subscription, payload []byte, o webpush.Options) (webpush.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{sub.Endpoint, string(payload), o})
	rs := f.results[sub.Endpoint]
	if len(rs) == 0 {
		return webpush.Result{Status: 201}, nil
	}
	r := rs[0]
	if len(rs) > 1 {
		f.results[sub.Endpoint] = rs[1:]
	}
	return r.res, r.err
}

func (f *fakeSender) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func newSub(t *testing.T, endpoint string) webpush.Subscription {
	t.Helper()
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	return webpush.Subscription{Endpoint: endpoint, P256dh: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}
}

type pushEnv struct {
	*env
	send   *fakeSender
	sleeps []time.Duration
}

func newPushEnv(t *testing.T) *pushEnv {
	t.Helper()
	p := &pushEnv{send: &fakeSender{results: map[string][]fakeResult{}}}
	p.env = newEnv(t)
	p.s = New(p.db, Options{Now: func() time.Time { return p.now }, Mailer: p.mail, Webhook: p.hook, Bus: p.bus, Roles: p.roles,
		PushSender: p.send, PushSleep: func(_ context.Context, d time.Duration) error { p.sleeps = append(p.sleeps, d); return nil }})
	return p
}

func (p *pushEnv) subscribe(subject, endpoint string) int64 {
	p.t.Helper()
	id, err := p.s.Subscribe(context.Background(), subject, newSub(p.t, endpoint), "Chrome on Android")
	if err != nil {
		p.t.Fatal(err)
	}
	return id
}

const ep1 = "https://fcm.googleapis.com/fcm/send/aaa"
const ep2 = "https://updates.push.services.mozilla.com/wpush/v2/bbb"

func TestSubscribeValidatesAndCaps(t *testing.T) {
	p := newPushEnv(t)
	ctx := context.Background()
	if _, err := p.s.Subscribe(ctx, "user:alice", webpush.Subscription{Endpoint: "http://169.254.169.254/x"}, ""); !ErrInvalid(err) {
		t.Fatalf("metadata endpoint accepted: %v", err)
	}
	if _, err := p.s.Subscribe(ctx, "not-a-subject", newSub(t, ep1), ""); !ErrInvalid(err) {
		t.Fatalf("bad subject accepted: %v", err)
	}
	for i := 0; i < MaxSubscriptionsPerUser; i++ {
		p.subscribe("user:alice", "https://push.example.com/s/"+strings.Repeat("a", i+1))
	}
	if _, err := p.s.Subscribe(ctx, "user:alice", newSub(t, "https://push.example.com/s/new"), ""); !errors.Is(err, ErrTooManySubscriptions) {
		t.Fatalf("11th subscription: %v", err)
	}
	// Re-subscribing an endpoint the user already has refreshes it and is not a new device.
	if _, err := p.s.Subscribe(ctx, "user:alice", newSub(t, "https://push.example.com/s/a"), ""); err != nil {
		t.Fatalf("refresh at the cap: %v", err)
	}
	// The cap is per user.
	p.subscribe("user:bob", ep1)
	devs, err := p.s.Subscriptions(ctx, "user:alice")
	if err != nil || len(devs) != MaxSubscriptionsPerUser {
		t.Fatalf("devices = %d, %v", len(devs), err)
	}
}

func TestSubscribeTakesOverEndpointAndUnsubscribeIsOwnerOnly(t *testing.T) {
	p := newPushEnv(t)
	ctx := context.Background()
	p.subscribe("user:alice", ep1)
	p.subscribe("user:bob", ep1) // same browser, other user: the endpoint moves
	if d, _ := p.s.Subscriptions(ctx, "user:alice"); len(d) != 0 {
		t.Fatal("alice still holds a subscription bob re-created")
	}
	if err := p.s.Unsubscribe(ctx, "user:alice", ep1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice removed bob's device: %v", err)
	}
	if err := p.s.Unsubscribe(ctx, "user:bob", ep1); err != nil {
		t.Fatal(err)
	}
	id := p.subscribe("user:bob", ep2)
	if err := p.s.RemoveSubscription(ctx, "user:alice", id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remove by id across users: %v", err)
	}
}

func TestVAPIDKeyIsCreatedOnceAndLazily(t *testing.T) {
	e := newEnv(t) // no PushSender: the real client path
	ctx := context.Background()
	var n int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_vapid`).Scan(&n)
	if n != 0 {
		t.Fatal("key created before any push use")
	}
	// A notification for a user without subscriptions must not create it either.
	e.notify("user:alice", "approval.requested", "")
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_vapid`).Scan(&n)
	if n != 0 {
		t.Fatal("key created by a notice with no subscriptions")
	}
	k1, err := e.s.VAPIDPublicKey(ctx)
	if err != nil || len(k1) != 87 {
		t.Fatalf("%q %v", k1, err)
	}
	k2, _ := e.s.VAPIDPublicKey(ctx)
	e2 := New(e.db, Options{})
	k3, _ := e2.VAPIDPublicKey(ctx)
	if k1 != k2 || k1 != k3 {
		t.Fatal("key changed between calls or instances")
	}
}

func TestVAPIDRotationDropsSubscriptions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k1, _ := e.s.VAPIDPublicKey(ctx)
	if _, err := e.s.Subscribe(ctx, "user:alice", newSub(t, ep1), ""); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RotateVAPID(ctx); err != nil {
		t.Fatal(err)
	}
	k2, _ := e.s.VAPIDPublicKey(ctx)
	if k1 == k2 {
		t.Fatal("key not rotated")
	}
	if d, _ := e.s.Subscriptions(ctx, "user:alice"); len(d) != 0 {
		t.Fatal("subscriptions bound to the old key survived rotation")
	}
}

func TestVAPIDKeyFileOverridesDatabase(t *testing.T) {
	e := newEnv(t)
	k, _ := webpush.GenerateKey()
	f := filepath.Join(t.TempDir(), "vapid")
	if err := os.WriteFile(f, []byte(k.Marshal()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(e.db, Options{VAPIDKeyFile: f})
	got, err := s.VAPIDPublicKey(context.Background())
	if err != nil || got != k.PublicKey() {
		t.Fatalf("%q %v", got, err)
	}
	if err := s.RotateVAPID(context.Background()); err == nil {
		t.Fatal("rotation must be refused for a mounted key (rotate the Secret instead)")
	}
	var n int
	_ = e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM push_vapid`).Scan(&n)
	if n != 0 {
		t.Fatal("a mounted key must not be copied into the database")
	}
	bad := New(e.db, Options{VAPIDKeyFile: filepath.Join(t.TempDir(), "missing")})
	if _, err := bad.VAPIDPublicKey(context.Background()); err == nil {
		t.Fatal("a missing key file must fail loudly, not fall back to a generated key")
	}
}

func TestPushDeliveryPayloadHasNoSensitiveData(t *testing.T) {
	p := newPushEnv(t)
	p.subscribe("user:alice", ep1)
	_, _, err := p.s.Notify(context.Background(), Notice{Subject: "user:alice", Type: "approval.requested", DedupeKey: "k",
		Data: map[string]string{"approval_type": "expense", "request_id": "apr_9", "title": "Salary 250000 for Jane Doe"}, Link: "/approvals/apr_9"})
	if err != nil {
		t.Fatal(err)
	}
	p.s.WaitPush()
	calls := p.send.Calls()
	if len(calls) != 1 || calls[0].Endpoint != ep1 {
		t.Fatalf("calls = %+v", calls)
	}
	pl := calls[0].Payload
	for _, bad := range []string{"expense", "Salary", "Jane", "250000"} {
		if strings.Contains(pl, bad) {
			t.Errorf("payload %s leaks %q", pl, bad)
		}
	}
	if !strings.Contains(pl, "Approval waiting") || !strings.Contains(pl, `"/approvals/apr_9"`) {
		t.Errorf("payload %s", pl)
	}
	if calls[0].Opt.TTL <= 0 || calls[0].Opt.Urgency != webpush.UrgencyNormal {
		t.Errorf("options %+v", calls[0].Opt)
	}
	d, _ := p.s.Deliveries(context.Background(), 10)
	if !hasDelivery(d, ChannelWebPush, "sent") {
		t.Fatalf("deliveries %+v", d)
	}
}

func hasDelivery(d []DeliveryLog, ch, status string) bool {
	for _, x := range d {
		if x.Channel == ch && x.Status == status {
			return true
		}
	}
	return false
}

func TestPushLinkMustBeASafeSiteRelativePath(t *testing.T) {
	p := newPushEnv(t)
	p.subscribe("user:alice", ep1)
	for i, link := range []string{"/leads/lead_1?email=a@b.c", "/a b", "/" + strings.Repeat("x", 150)} {
		_, _, _ = p.s.Notify(context.Background(), Notice{Subject: "user:alice", Type: "custom.thing", DedupeKey: string(rune('a' + i)), Link: link})
	}
	p.s.WaitPush()
	for _, c := range p.send.Calls() {
		if !strings.Contains(c.Payload, `"u":"/notifications"`) {
			t.Errorf("unsafe or odd link was forwarded: %s", c.Payload)
		}
		if strings.Contains(c.Payload, "email") {
			t.Errorf("query string leaked: %s", c.Payload)
		}
	}
}

func TestPushUrgentUsesHighUrgency(t *testing.T) {
	p := newPushEnv(t)
	p.subscribe("user:alice", ep1)
	_, _, _ = p.s.Notify(context.Background(), Notice{Subject: "user:alice", Type: "approval.escalated", Urgent: true, Link: "/approvals/apr_1"})
	p.s.WaitPush()
	c := p.send.Calls()
	if len(c) != 1 || c[0].Opt.Urgency != webpush.UrgencyHigh {
		t.Fatalf("%+v", c)
	}
}

func TestPushPreferencesQuietHoursAndDigest(t *testing.T) {
	p := newPushEnv(t)
	ctx := context.Background()
	p.subscribe("user:alice", ep1)
	// A digest setting batches email only; push stays real time.
	set := Settings{Subject: "user:alice", QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "daily"}
	_ = p.s.SetSettings(ctx, set)
	p.notify("user:alice", "approval.requested", "d1")
	p.s.WaitPush()
	if len(p.send.Calls()) != 1 {
		t.Fatalf("digest must not hold push: %d calls", len(p.send.Calls()))
	}
	// Quiet hours hold push for ordinary notices ...
	set.QuietStart, set.QuietEnd, set.Digest = 11*60, 13*60, "off" // p.now is 12:00 UTC
	_ = p.s.SetSettings(ctx, set)
	p.notify("user:alice", "approval.requested", "q1")
	p.s.WaitPush()
	if len(p.send.Calls()) != 1 {
		t.Fatalf("ordinary notice pushed during quiet hours: %d calls", len(p.send.Calls()))
	}
	d, _ := p.s.Deliveries(ctx, 20)
	if !hasDelivery(d, ChannelWebPush, "suppressed") {
		t.Fatalf("quiet-hours suppression not logged: %+v", d)
	}
	// ... but urgent ones bypass them.
	_, _, _ = p.s.Notify(ctx, Notice{Subject: "user:alice", Type: "approval.escalated", DedupeKey: "u1", Urgent: true, Link: "/approvals/x"})
	p.s.WaitPush()
	if len(p.send.Calls()) != 2 {
		t.Fatalf("urgent notice must bypass quiet hours: %d calls", len(p.send.Calls()))
	}
	// A per-type preference turns the channel off.
	set.QuietStart, set.QuietEnd = -1, -1
	_ = p.s.SetSettings(ctx, set)
	if err := p.s.SetPref(ctx, "user:alice", "approval.requested", ChannelWebPush, false); err != nil {
		t.Fatal(err)
	}
	p.notify("user:alice", "approval.requested", "p1")
	p.s.WaitPush()
	if len(p.send.Calls()) != 2 {
		t.Fatalf("muted type pushed: %d", len(p.send.Calls()))
	}
	// Only the owner's devices are used.
	p.notify("user:bob", "approval.requested", "b1")
	p.s.WaitPush()
	if len(p.send.Calls()) != 2 {
		t.Fatal("bob has no devices; nothing may be sent")
	}
}

func TestPushRateLimit(t *testing.T) {
	p := newPushEnv(t)
	p.s.SetRateLimit(2)
	p.subscribe("user:alice", ep1)
	for i := 0; i < 4; i++ {
		p.notify("user:alice", "approval.requested", string(rune('a'+i)))
		p.s.WaitPush()
	}
	if n := len(p.send.Calls()); n != 2 {
		t.Fatalf("sent %d, want the limit 2", n)
	}
}

func TestPushRetriesTransientFailuresThenGivesUp(t *testing.T) {
	p := newPushEnv(t)
	ctx := context.Background()
	p.subscribe("user:alice", ep1)
	p.send.results[ep1] = []fakeResult{
		{webpush.Result{Status: 503, Retryable: true}, errors.New("push service answered 503")},
		{webpush.Result{Status: 429, Retryable: true, RetryAfter: 7 * time.Second}, errors.New("push service answered 429")},
		{webpush.Result{Status: 201}, nil},
	}
	p.notify("user:alice", "approval.requested", "r1")
	p.s.WaitPush()
	if n := len(p.send.Calls()); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
	if len(p.sleeps) != 2 || p.sleeps[1] != 7*time.Second {
		t.Fatalf("backoff %v (Retry-After must be honoured)", p.sleeps)
	}
	// A permanent failure is not retried, is logged as failed and counts against the device.
	p.send.calls = nil
	p.send.results[ep1] = []fakeResult{{webpush.Result{Status: 400}, errors.New("push service answered 400")}}
	p.notify("user:alice", "approval.requested", "r2")
	p.s.WaitPush()
	if n := len(p.send.Calls()); n != 1 {
		t.Fatalf("400 retried: %d attempts", n)
	}
	d, _ := p.s.Deliveries(ctx, 20)
	if !hasDelivery(d, ChannelWebPush, "failed") {
		t.Fatalf("failure not logged: %+v", d)
	}
	var fails int
	_ = p.db.QueryRowContext(ctx, `SELECT fail_count FROM push_subscriptions`).Scan(&fails)
	if fails != 1 {
		t.Fatalf("fail_count = %d", fails)
	}
}

func TestPushGoneSubscriptionsAreDeleted(t *testing.T) {
	for _, status := range []int{404, 410} {
		p := newPushEnv(t)
		p.subscribe("user:alice", ep1)
		p.subscribe("user:alice", ep2)
		p.send.results[ep1] = []fakeResult{{webpush.Result{Status: status}, webpush.ErrGone}}
		p.notify("user:alice", "approval.requested", "g1")
		p.s.WaitPush()
		devs, _ := p.s.Subscriptions(context.Background(), "user:alice")
		if len(devs) != 1 {
			t.Fatalf("%d: devices after cleanup = %d, want 1", status, len(devs))
		}
		d, _ := p.s.Deliveries(context.Background(), 10)
		if !hasDelivery(d, ChannelWebPush, "sent") {
			t.Fatalf("the live device accepted it, so the delivery is sent: %+v", d)
		}
	}
}

func TestPushDeviceDroppedAfterRepeatedFailures(t *testing.T) {
	p := newPushEnv(t)
	p.subscribe("user:alice", ep1)
	p.send.results[ep1] = []fakeResult{{webpush.Result{Status: 403}, errors.New("push service answered 403")}}
	for i := 0; i < maxPushFailures; i++ {
		p.notify("user:alice", "approval.requested", string(rune('a'+i)))
		p.s.WaitPush()
	}
	if d, _ := p.s.Subscriptions(context.Background(), "user:alice"); len(d) != 0 {
		t.Fatal("a device that never accepts was kept forever")
	}
}

func TestSendTestRateLimitedAndUsesGenericPayload(t *testing.T) {
	p := newPushEnv(t)
	ctx := context.Background()
	if _, err := p.s.SendTest(ctx, "user:alice"); !errors.Is(err, ErrNoSubscriptions) {
		t.Fatalf("no devices: %v", err)
	}
	p.subscribe("user:alice", ep1)
	p.subscribe("user:alice", ep2)
	n, err := p.s.SendTest(ctx, "user:alice")
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := p.s.SendTest(ctx, "user:alice"); !errors.Is(err, ErrPushRateLimited) {
		t.Fatalf("second test within the interval: %v", err)
	}
	if _, err := p.s.SendTest(ctx, "user:bob"); !errors.Is(err, ErrNoSubscriptions) {
		t.Fatalf("bob's limit is separate and he has no device: %v", err)
	}
	p.now = p.now.Add(time.Minute)
	if _, err := p.s.SendTest(ctx, "user:alice"); err != nil {
		t.Fatalf("after the interval: %v", err)
	}
	if c := p.send.Calls()[0]; !strings.Contains(c.Payload, "Test notification") || !strings.Contains(c.Payload, `"/notifications"`) {
		t.Fatalf("%s", c.Payload)
	}
}

// End to end through the real client and an httptest push service: the body is encrypted, the
// request carries VAPID, and the delivery is recorded. No real push service is contacted.
func TestPushEndToEndAgainstFakePushService(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(201)
	}))
	defer srv.Close()
	e := newEnv(t)
	s := New(e.db, Options{Now: func() time.Time { return e.now }, PushHTTP: srv.Client(), AllowInsecurePush: true, PushContact: "mailto:ops@example.com"})
	ctx := context.Background()
	if _, err := s.Subscribe(ctx, "user:alice", newSub(t, srv.URL+"/push/abc"), "Chrome on Android"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Notify(ctx, Notice{Subject: "user:alice", Type: "approval.requested", Link: "/approvals/apr_1"}); err != nil {
		t.Fatal(err)
	}
	s.WaitPush()
	if got == nil || got.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(got.Header.Get("Authorization"), "vapid t=") || got.Header.Get("TTL") == "" {
		t.Fatalf("request: %+v", got)
	}
	pub, _ := s.VAPIDPublicKey(ctx)
	if !strings.HasSuffix(got.Header.Get("Authorization"), "k="+pub) {
		t.Fatal("VAPID k does not match the published public key")
	}
	if len(body) < 86 || strings.Contains(string(body), "Approval") {
		t.Fatalf("body not encrypted (%d bytes)", len(body))
	}
	d, _ := s.Deliveries(ctx, 5)
	if !hasDelivery(d, ChannelWebPush, "sent") {
		t.Fatalf("%+v", d)
	}
}

func TestDeviceLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/126.0 Mobile Safari/537.36": "Chrome on Android",
		"Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0":                        "Firefox on Linux",
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/126 Safari/537.36 Edg/126":             "Edge on Windows",
		"<script>alert(1)</script>": "Browser",
		"":                          "Browser",
	} {
		if got := DeviceLabel(ua); got != want {
			t.Errorf("%q -> %q, want %q", ua, got, want)
		}
	}
}

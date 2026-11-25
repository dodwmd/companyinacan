package notify

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type fakeRoles map[string][]string

func (f fakeRoles) SubjectsWithRole(_ context.Context, role string, limit int) ([]string, error) {
	r := f[role]
	if len(r) > limit {
		r = r[:limit]
	}
	return r, nil
}

type env struct {
	t     *testing.T
	db    *store.DB
	s     *Service
	mail  *FakeMailer
	hook  *FakeWebhook
	bus   *events.Bus
	now   time.Time
	roles fakeRoles
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, db: db, mail: &FakeMailer{}, hook: &FakeWebhook{}, now: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), roles: fakeRoles{}}
	db.Now = func() time.Time { return e.now }
	e.bus = events.NewBus(db)
	e.s = New(db, Options{Now: func() time.Time { return e.now }, Mailer: e.mail, Webhook: e.hook, Bus: e.bus, Roles: e.roles})
	return e
}

func (e *env) setEmail(subject, addr string) {
	e.t.Helper()
	if err := e.s.SetSettings(context.Background(), Settings{Subject: subject, Email: addr, QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "off"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) notify(subject, typ, key string) (int64, bool) {
	e.t.Helper()
	id, created, err := e.s.Notify(context.Background(), Notice{Subject: subject, Type: typ, DedupeKey: key, Data: map[string]string{"request_id": "apr_1"}, Link: "/approvals/apr_1"})
	if err != nil {
		e.t.Fatal(err)
	}
	return id, created
}

func TestInAppNotificationAndUnreadCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.notify("user:alice", "approval.requested", "")
	e.notify("user:alice", "approval.requested", "")
	e.notify("user:bob", "approval.requested", "")
	n, err := e.s.UnreadCount(ctx, "user:alice")
	if err != nil || n != 2 {
		t.Fatalf("unread = %d, %v; want 2", n, err)
	}
	items, err := e.s.List(ctx, "user:alice", ListOptions{Limit: 10})
	if err != nil || len(items) != 2 {
		t.Fatalf("list = %d, %v", len(items), err)
	}
	if err := e.s.MarkRead(ctx, "user:alice", items[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.UnreadCount(ctx, "user:alice"); n != 1 {
		t.Fatalf("unread after mark-read = %d", n)
	}
	// A subject can never mark someone else's notification.
	if err := e.s.MarkRead(ctx, "user:bob", items[1].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-user mark-read err = %v, want ErrNotFound", err)
	}
	if n, _ := e.s.UnreadCount(ctx, "user:alice"); n != 1 {
		t.Fatal("bob must not have been able to mark alice's notification")
	}
	if err := e.s.MarkAllRead(ctx, "user:alice"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.UnreadCount(ctx, "user:alice"); n != 0 {
		t.Fatalf("unread after read-all = %d", n)
	}
}

func TestUnreadCountIsCapped(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < MaxUnreadCount+5; i++ {
		e.notify("user:a", "x.y", "")
	}
	n, _ := e.s.UnreadCount(context.Background(), "user:a")
	if n != MaxUnreadCount {
		t.Fatalf("unread = %d, want the cap %d", n, MaxUnreadCount)
	}
}

func TestDedupe(t *testing.T) {
	e := newEnv(t)
	e.setEmail("user:alice", "alice@example.com")
	id1, c1 := e.notify("user:alice", "approval.requested", "apr_1:step0")
	id2, c2 := e.notify("user:alice", "approval.requested", "apr_1:step0")
	if !c1 || c2 || id1 != id2 {
		t.Fatalf("created %v/%v ids %d/%d: second notify with the same key must be a no-op", c1, c2, id1, id2)
	}
	if len(e.mail.Sent()) != 1 {
		t.Fatalf("emails = %d, want 1 (a duplicate must not be re-delivered)", len(e.mail.Sent()))
	}
	if _, c3 := e.notify("user:bob", "approval.requested", "apr_1:step0"); !c3 {
		t.Fatal("the same key for another recipient is a different notification")
	}
}

func TestEmailUsesTemplateAndEscapesData(t *testing.T) {
	e := newEnv(t)
	e.setEmail("user:alice", "alice@example.com")
	_, _, err := e.s.Notify(context.Background(), Notice{Subject: "user:alice", Type: "custom.reminder",
		Data: map[string]string{"title": "<script>alert(1)</script>\r\nBcc: evil@example.com", "body": "a & b"}})
	if err != nil {
		t.Fatal(err)
	}
	m := e.mail.Sent()
	if len(m) != 1 {
		t.Fatalf("emails = %d", len(m))
	}
	if strings.Contains(m[0].HTML, "<script>") || !strings.Contains(m[0].HTML, "&lt;script&gt;") {
		t.Fatalf("html body not escaped: %q", m[0].HTML)
	}
	if strings.ContainsAny(m[0].Subject, "\r\n") {
		t.Fatalf("subject carries a line break (header injection): %q", m[0].Subject)
	}
	if m[0].To != "alice@example.com" {
		t.Fatalf("to = %q", m[0].To)
	}
}

func TestPreferencesPerTypeAndChannel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setEmail("user:alice", "alice@example.com")
	if err := e.s.SetPref(ctx, "user:alice", "approval.requested", ChannelEmail, false); err != nil {
		t.Fatal(err)
	}
	e.notify("user:alice", "approval.requested", "")
	if len(e.mail.Sent()) != 0 {
		t.Fatal("email disabled for this type must not send")
	}
	e.notify("user:alice", "lead.created", "")
	if len(e.mail.Sent()) != 1 {
		t.Fatal("other types still send")
	}
	if err := e.s.SetPref(ctx, "user:alice", "*", ChannelEmail, false); err != nil {
		t.Fatal(err)
	}
	e.notify("user:alice", "deal.won", "")
	if len(e.mail.Sent()) != 1 {
		t.Fatal("wildcard off must stop email for types without their own preference")
	}
	if err := e.s.SetPref(ctx, "user:alice", "x", "carrier-pigeon", true); err == nil {
		t.Fatal("unknown channel accepted")
	}
	// In-app can be muted for non-urgent types, but urgent ones always arrive.
	_ = e.s.SetPref(ctx, "user:alice", "lead.created", ChannelInApp, false)
	if _, c := e.notify("user:alice", "lead.created", ""); c {
		t.Fatal("muted in-app type must not be stored")
	}
	if _, _, err := e.s.Notify(ctx, Notice{Subject: "user:alice", Type: "lead.created", Urgent: true}); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.UnreadCount(ctx, "user:alice"); n == 0 {
		t.Fatal("urgent notification must bypass the in-app mute")
	}
}

func TestQuietHoursQueueThenDigestSendsOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setEmail("user:alice", "alice@example.com")
	// 22:00-07:00 UTC; now is 12:00, so move to 23:00.
	if err := e.s.SetSettings(ctx, Settings{Subject: "user:alice", Email: "alice@example.com", QuietStart: 22 * 60, QuietEnd: 7 * 60, TZ: "UTC", Digest: "off"}); err != nil {
		t.Fatal(err)
	}
	e.now = time.Date(2026, 3, 2, 23, 0, 0, 0, time.UTC)
	e.notify("user:alice", "approval.requested", "a")
	e.notify("user:alice", "approval.requested", "b")
	if len(e.mail.Sent()) != 0 {
		t.Fatal("no email during quiet hours")
	}
	if n, _ := e.s.UnreadCount(ctx, "user:alice"); n != 2 {
		t.Fatal("in-app is stored during quiet hours")
	}
	// Urgent bypasses quiet hours.
	if _, _, err := e.s.Notify(ctx, Notice{Subject: "user:alice", Type: "p0", Urgent: true}); err != nil {
		t.Fatal(err)
	}
	if len(e.mail.Sent()) != 1 {
		t.Fatal("urgent must be emailed during quiet hours")
	}
	if n, err := e.s.FlushDigests(ctx); err != nil || n != 0 {
		t.Fatalf("flush during quiet hours sent %d, %v", n, err)
	}
	e.now = time.Date(2026, 3, 3, 7, 30, 0, 0, time.UTC)
	if n, err := e.s.FlushDigests(ctx); err != nil || n != 1 {
		t.Fatalf("flush after quiet hours sent %d, %v; want one digest", n, err)
	}
	if len(e.mail.Sent()) != 2 || !strings.Contains(e.mail.Sent()[1].Text, "2") {
		t.Fatalf("digest should be a single email covering both: %+v", e.mail.Sent())
	}
	if n, _ := e.s.FlushDigests(ctx); n != 0 || len(e.mail.Sent()) != 2 {
		t.Fatal("a flushed digest must not be sent again")
	}
}

func TestQuietHoursWrapAround(t *testing.T) {
	for _, c := range []struct {
		h, m int
		want bool
	}{{23, 0, true}, {3, 0, true}, {6, 59, true}, {7, 0, false}, {12, 0, false}, {21, 59, false}, {22, 0, true}} {
		got := inQuiet(time.Date(2026, 1, 1, c.h, c.m, 0, 0, time.UTC), 22*60, 7*60, "UTC")
		if got != c.want {
			t.Errorf("%02d:%02d quiet = %v, want %v", c.h, c.m, got, c.want)
		}
	}
	if inQuiet(time.Now(), -1, -1, "UTC") {
		t.Fatal("no quiet hours configured")
	}
	// A bad time zone never silences someone permanently.
	if inQuiet(time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC), 22*60, 7*60, "Not/AZone") != true {
		t.Fatal("unknown tz falls back to UTC")
	}
}

func TestDigestHourlyBatches(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.s.SetSettings(ctx, Settings{Subject: "user:alice", Email: "alice@example.com", QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "hourly"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		e.notify("user:alice", "lead.created", "")
	}
	if len(e.mail.Sent()) != 0 {
		t.Fatal("digest users get nothing immediately")
	}
	e.now = e.now.Add(30 * time.Minute)
	if n, _ := e.s.FlushDigests(ctx); n != 0 {
		t.Fatal("hourly digest not due yet")
	}
	e.now = e.now.Add(31 * time.Minute)
	if n, _ := e.s.FlushDigests(ctx); n != 1 || len(e.mail.Sent()) != 1 {
		t.Fatalf("hourly digest: sent %d", n)
	}
}

func TestRateLimitSuppressesExternalButNotInApp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.s.opt.RateLimitPerHour = 3
	e.setEmail("user:alice", "alice@example.com")
	for i := 0; i < 6; i++ {
		e.notify("user:alice", "lead.created", "")
	}
	if len(e.mail.Sent()) != 3 {
		t.Fatalf("emails = %d, want 3 (the limit)", len(e.mail.Sent()))
	}
	if n, _ := e.s.UnreadCount(ctx, "user:alice"); n != 6 {
		t.Fatalf("in-app = %d, want all 6", n)
	}
	logs, _ := e.s.Deliveries(ctx, 50)
	suppressed := 0
	for _, d := range logs {
		if d.Status == "suppressed" {
			suppressed++
		}
	}
	if suppressed != 3 {
		t.Fatalf("suppressed log rows = %d, want 3", suppressed)
	}
	e.now = e.now.Add(61 * time.Minute)
	e.notify("user:alice", "lead.created", "")
	if len(e.mail.Sent()) != 4 {
		t.Fatal("the window must slide")
	}
}

func TestMailerFailureIsLoggedNotFatal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setEmail("user:alice", "alice@example.com")
	e.mail.Err = errors.New("smtp down")
	if _, c := e.notify("user:alice", "lead.created", ""); !c {
		t.Fatal("the in-app notification must still be created")
	}
	logs, _ := e.s.Deliveries(ctx, 10)
	var failed bool
	for _, d := range logs {
		failed = failed || (d.Channel == ChannelEmail && d.Status == "failed" && strings.Contains(d.Detail, "smtp down"))
	}
	if !failed {
		t.Fatalf("no failed email delivery in log: %+v", logs)
	}
}

func TestNotificationSentEventCarriesIDsOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.setEmail("user:alice", "alice@example.com")
	e.notify("user:alice", "approval.requested", "")
	ev, err := e.db.QueryContext(ctx, `SELECT type, data_json FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ev.Close() }()
	n := 0
	for ev.Next() {
		var ty, data string
		_ = ev.Scan(&ty, &data)
		if ty != "notification.sent.v1" {
			continue
		}
		n++
		if strings.Contains(data, "alice@example.com") || strings.Contains(data, "Approval") {
			t.Fatalf("event leaks content: %s", data)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(data), &m)
		for _, k := range []string{"notification_id", "channel", "subject", "type"} {
			if _, ok := m[k]; !ok {
				t.Errorf("event data lacks %q: %s", k, data)
			}
		}
	}
	if n != 2 {
		t.Fatalf("notification.sent.v1 events = %d, want 2 (in-app and email)", n)
	}
}

func TestWebhookChannel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.s.SetChannels(ctx, map[string]string{"ops": "https://hooks.example.com/T1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.NotifyChannel(ctx, "ops", Notice{Type: "lead.created", DedupeKey: "evt1", Data: map[string]string{"lead_id": "l1"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.NotifyChannel(ctx, "ops", Notice{Type: "lead.created", DedupeKey: "evt1", Data: map[string]string{"lead_id": "l1"}}); err != nil {
		t.Fatal(err)
	}
	posts := e.hook.Posts()
	if len(posts) != 1 || posts[0].URL != "https://hooks.example.com/T1" || !strings.Contains(posts[0].Text, "lead") {
		t.Fatalf("posts = %+v, want exactly one (deduped)", posts)
	}
	if err := e.s.NotifyChannel(ctx, "nope", Notice{Type: "x.y"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown channel err = %v", err)
	}
	if err := e.s.SetChannels(ctx, map[string]string{"bad": "http://insecure.example.com/"}); err == nil {
		t.Fatal("plain http channel accepted")
	}
}

func TestSubscriptionRulesFromManifests(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.roles["sales"] = []string{"user:sam", "user:sue"}
	m := store.Module{ID: "forms", Version: "1", Namespace: "mod-forms", ServiceAccount: "forms", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "forms", Notifications: []config.NotifyRule{
			{Event: "lead.created.v1", Recipients: []string{"role:sales", "field:owner", "user:user:boss"}},
		}}}
	if err := e.db.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	e.bus.SubscribeAll(e.s.HandleEvent)
	data := `{"lead_id":"l1","email":"x@example.com","owner":"user:olga","name":"Mallory"}`
	if _, err := e.bus.Publish(ctx, "forms", "lead.created.v1", json.RawMessage(data)); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"user:sam", "user:sue", "user:olga", "user:boss"} {
		n, _ := e.s.UnreadCount(ctx, who)
		if n != 1 {
			t.Errorf("%s unread = %d, want 1", who, n)
		}
	}
	items, _ := e.s.List(ctx, "user:sam", ListOptions{Limit: 5})
	if len(items) != 1 {
		t.Fatal("no item")
	}
	if strings.Contains(items[0].DataJSON, "x@example.com") || strings.Contains(items[0].DataJSON, "Mallory") {
		t.Fatalf("notification data carries PII from the event: %s", items[0].DataJSON)
	}
	if !strings.Contains(items[0].DataJSON, "l1") {
		t.Fatalf("notification data should keep ids: %s", items[0].DataJSON)
	}
	// A non-matching event produces nothing; a disabled module's rules are ignored.
	if _, err := e.bus.Publish(ctx, "forms", "deal.won.v1", json.RawMessage(`{"deal_id":"d1"}`)); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.UnreadCount(ctx, "user:sam"); n != 1 {
		t.Fatal("unmatched event notified")
	}
}

func TestRulesDoNotLoopOnNotificationEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := store.Module{ID: "m", Version: "1", Namespace: "mod-m", ServiceAccount: "m", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "m", Notifications: []config.NotifyRule{{Event: "notification.sent.v1", Recipients: []string{"user:a"}}}}}
	_ = e.db.UpsertModule(ctx, m, nil, nil)
	e.bus.SubscribeAll(e.s.HandleEvent)
	e.notify("user:a", "x.y", "")
	if n, _ := e.s.UnreadCount(ctx, "user:a"); n != 1 {
		t.Fatalf("unread = %d: notification.sent.v1 must never trigger rules", n)
	}
}

func TestRecipientsPerRuleAreBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var many []string
	for i := 0; i < 500; i++ {
		many = append(many, "user:u"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('a'+(i/676)%26)))
	}
	e.roles["everyone"] = many
	m := store.Module{ID: "m", Version: "1", Namespace: "mod-m", ServiceAccount: "m", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "m", Notifications: []config.NotifyRule{{Event: "lead.created.v1", Recipients: []string{"role:everyone"}}}}}
	_ = e.db.UpsertModule(ctx, m, nil, nil)
	e.bus.SubscribeAll(e.s.HandleEvent)
	_, _ = e.bus.Publish(ctx, "m", "lead.created.v1", nil)
	var n int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications`).Scan(&n)
	if n > MaxRecipientsPerRule {
		t.Fatalf("%d notifications created, cap is %d", n, MaxRecipientsPerRule)
	}
}

func TestPruneAndDeliveryLog(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.notify("user:a", "x.y", "")
	e.now = e.now.Add(200 * 24 * time.Hour)
	n, err := e.s.Prune(ctx, 180*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	var d int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_deliveries`).Scan(&d)
	if d != 0 {
		t.Fatal("deliveries must go with their notification")
	}
}

func TestInvalidInputs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, n := range []Notice{{Subject: "alice", Type: "x.y"}, {Subject: "user:a", Type: ""}, {Subject: "user:a", Type: "Bad Type"}, {Subject: "user:a", Type: "x.y", Link: "https://evil.example/"}, {Subject: "user:a", Type: "x.y", Link: "//evil.example"}} {
		if _, _, err := e.s.Notify(ctx, n); err == nil {
			t.Errorf("notice %+v accepted", n)
		}
	}
	if err := e.s.SetSettings(ctx, Settings{Subject: "user:a", Email: "not-an-address", QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "off"}); err == nil {
		t.Fatal("bad email accepted")
	}
	if err := e.s.SetSettings(ctx, Settings{Subject: "user:a", Email: "a@example.com\r\nBcc: x@example.com", QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "off"}); err == nil {
		t.Fatal("email with a line break accepted")
	}
}

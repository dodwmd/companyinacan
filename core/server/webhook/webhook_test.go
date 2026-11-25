package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestSignVerify(t *testing.T) {
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1_700_000_000, 0)
	sig := Sign("secret", now, body)
	if !strings.HasPrefix(sig, "v1=") || len(sig) != 3+64 {
		t.Fatalf("signature format: %s", sig)
	}
	h := http.Header{}
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	h.Set(HeaderSignature, sig)
	if err := Verify("secret", h, body, now.Add(time.Minute), DefaultReplayWindow); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if err := Verify("other", h, body, now, DefaultReplayWindow); !errors.Is(err, ErrBadSignature) {
		t.Errorf("wrong secret: %v", err)
	}
	if err := Verify("secret", h, []byte(`{"id":"evt_2"}`), now, DefaultReplayWindow); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered body: %v", err)
	}
	if err := Verify("secret", h, body, now.Add(DefaultReplayWindow+time.Second), DefaultReplayWindow); !errors.Is(err, ErrStale) {
		t.Errorf("replayed outside window: %v", err)
	}
	if err := Verify("secret", h, body, now.Add(-DefaultReplayWindow-time.Second), DefaultReplayWindow); !errors.Is(err, ErrStale) {
		t.Errorf("future-dated outside window: %v", err)
	}
	// a fresh timestamp cannot be paired with an old signature
	h2 := h.Clone()
	h2.Set(HeaderTimestamp, strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	if err := Verify("secret", h2, body, now.Add(time.Hour), DefaultReplayWindow); !errors.Is(err, ErrBadSignature) {
		t.Errorf("timestamp swap: %v", err)
	}
	for _, mut := range []func(http.Header){
		func(h http.Header) { h.Del(HeaderSignature) },
		func(h http.Header) { h.Del(HeaderTimestamp) },
		func(h http.Header) { h.Set(HeaderTimestamp, "abc") },
		func(h http.Header) { h.Set(HeaderSignature, "v2=00") },
		func(h http.Header) { h.Set(HeaderSignature, "v1=zz") },
	} {
		hh := h.Clone()
		mut(hh)
		if err := Verify("secret", hh, body, now, DefaultReplayWindow); err == nil {
			t.Error("malformed headers accepted")
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for a := 1; a <= 6; a++ {
		d := DefaultBackoff(a)
		if d <= prev {
			t.Fatalf("attempt %d: %v not greater than %v", a, d, prev)
		}
		prev = d
	}
	if DefaultBackoff(1) != 5*time.Second || DefaultBackoff(50) != time.Hour {
		t.Fatalf("1=%v 50=%v", DefaultBackoff(1), DefaultBackoff(50))
	}
}

// --- dispatcher ----------------------------------------------------------------------------

type rt func(*http.Request) (*http.Response, error)

func (f rt) Do(r *http.Request) (*http.Response, error) { return f(r) }

func resp(code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("ignored"))}
}

type fixture struct {
	d   *Dispatcher
	db  *store.DB
	bus *events.Bus
	now *time.Time
	got []*http.Request
	bod [][]byte
}

func setup(t *testing.T, handler func(*fixture, *http.Request) (*http.Response, error)) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	f := &fixture{db: db, bus: events.NewBus(db), now: &now}
	err = db.UpsertModule(ctx, store.Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: store.StateEnabled, HMACSecret: "crm-secret"}, nil,
		[]store.Subscription{{ModuleID: "crm", Event: "lead.created.v1", Service: "crm", Port: 8000, Webhook: "/hooks/lead"}})
	if err != nil {
		t.Fatal(err)
	}
	f.d = NewDispatcher(db, rt(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		f.bod = append(f.bod, b)
		f.got = append(f.got, r)
		return handler(f, r)
	}))
	f.d.Now = func() time.Time { return now }
	return f
}

func (f *fixture) publish(t *testing.T) events.Envelope {
	t.Helper()
	e, err := f.bus.Publish(context.Background(), "forms", "lead.created.v1", json.RawMessage(`{"email":"a@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDeliverySignedRequest(t *testing.T) {
	f := setup(t, func(*fixture, *http.Request) (*http.Response, error) { return resp(204), nil })
	env := f.publish(t)
	n, err := f.d.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	r := f.got[0]
	if r.Method != http.MethodPost || r.URL.String() != "http://crm.mod-crm.svc.cluster.local:8000/hooks/lead" {
		t.Fatalf("%s %s", r.Method, r.URL)
	}
	if r.Header.Get(HeaderEventID) != env.ID || r.Header.Get(HeaderEventType) != "lead.created.v1" || r.Header.Get(HeaderAttempt) != "1" || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", r.Header)
	}
	if err := Verify("crm-secret", r.Header, f.bod[0], *f.now, DefaultReplayWindow); err != nil {
		t.Fatalf("adapter-side verification failed: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(f.bod[0], &m)
	if m["id"] != env.ID || m["data"].(map[string]any)["email"] != "a@example.com" {
		t.Fatalf("body %s", f.bod[0])
	}
	ds, _ := f.db.ListDeliveries(context.Background(), 5)
	if ds[0].Status != store.StatusDelivered || ds[0].Attempts != 1 {
		t.Fatalf("%+v", ds[0])
	}
	// nothing left to do
	if n, _ := f.d.RunOnce(context.Background()); n != 0 {
		t.Fatalf("redelivered a delivered event (%d)", n)
	}
}

func TestRetryWithBackoffThenSuccess(t *testing.T) {
	calls := 0
	f := setup(t, func(*fixture, *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return resp(503), nil
		case 2:
			return nil, errors.New("connection refused")
		}
		return resp(200), nil
	})
	f.publish(t)
	ctx := context.Background()

	if n, _ := f.d.RunOnce(ctx); n != 1 {
		t.Fatalf("first run n=%d", n)
	}
	if n, _ := f.d.RunOnce(ctx); n != 0 {
		t.Fatal("retried before the backoff elapsed")
	}
	*f.now = f.now.Add(DefaultBackoff(1))
	if n, _ := f.d.RunOnce(ctx); n != 1 {
		t.Fatal("second attempt not made after backoff")
	}
	*f.now = f.now.Add(DefaultBackoff(2))
	if n, _ := f.d.RunOnce(ctx); n != 1 {
		t.Fatal("third attempt not made")
	}
	ds, _ := f.db.ListDeliveries(ctx, 5)
	if ds[0].Status != store.StatusDelivered || ds[0].Attempts != 3 {
		t.Fatalf("%+v", ds[0])
	}
	as, _ := f.db.Attempts(ctx, ds[0].ID)
	if len(as) != 3 || as[0].StatusCode != 503 || as[1].Error == "" || as[2].StatusCode != 200 {
		t.Fatalf("delivery log %+v", as)
	}
	if f.got[2].Header.Get(HeaderAttempt) != "3" {
		t.Fatal("attempt header not incremented")
	}
	// the event id is stable across retries (idempotency key)
	if f.got[0].Header.Get(HeaderEventID) != f.got[2].Header.Get(HeaderEventID) {
		t.Fatal("event id changed between attempts")
	}
}

func TestDeadAfterMaxAttempts(t *testing.T) {
	f := setup(t, func(*fixture, *http.Request) (*http.Response, error) { return resp(500), nil })
	f.d.MaxAttempts = 3
	f.publish(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if n, _ := f.d.RunOnce(ctx); n != 1 {
			t.Fatalf("run %d: n=%d", i, n)
		}
		*f.now = f.now.Add(2 * time.Hour)
	}
	ds, _ := f.db.ListDeliveries(ctx, 5)
	if ds[0].Status != store.StatusDead || ds[0].Attempts != 3 {
		t.Fatalf("%+v", ds[0])
	}
	if n, _ := f.d.RunOnce(ctx); n != 0 {
		t.Fatal("dead delivery retried")
	}
}

func TestMissingSecretIsNotSentUnsigned(t *testing.T) {
	f := setup(t, func(*fixture, *http.Request) (*http.Response, error) { return resp(200), nil })
	_, _ = f.db.Exec(`UPDATE modules SET hmac_secret=''`)
	f.publish(t)
	_, _ = f.d.RunOnce(context.Background())
	if len(f.got) != 0 {
		t.Fatal("unsigned webhook was sent")
	}
	ds, _ := f.db.ListDeliveries(context.Background(), 5)
	as, _ := f.db.Attempts(context.Background(), ds[0].ID)
	if len(as) != 1 || !strings.Contains(as[0].Error, "secret") {
		t.Fatalf("%+v", as)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	c := NewHTTPClient(time.Second)
	if c.CheckRedirect == nil || !errors.Is(c.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("client must not follow redirects (signed body would leak to the redirect target)")
	}
}

func TestRunLoopProcessesOnWake(t *testing.T) {
	done := make(chan struct{}, 1)
	f := setup(t, func(*fixture, *http.Request) (*http.Response, error) {
		select {
		case done <- struct{}{}:
		default:
		}
		return resp(200), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.d.Run(ctx, f.bus.Wake(), time.Hour)
	f.publish(t)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not run after wake")
	}
}

func TestDeliveryResultsAreCounted(t *testing.T) {
	read := func() string {
		var b strings.Builder
		_, _ = obs.Default.WriteTo(&b)
		return b.String()
	}
	count := func(result string) float64 {
		for _, l := range strings.Split(read(), "\n") {
			if strings.HasPrefix(l, `can_webhook_deliveries_total{result="`+result+`"} `) {
				v, _ := strconv.ParseFloat(strings.Fields(l)[1], 64)
				return v
			}
		}
		return 0
	}
	delivered, retry, dead := count("delivered"), count("retry"), count("dead")
	ok := setup(t, func(*fixture, *http.Request) (*http.Response, error) { return resp(200), nil })
	ok.publish(t)
	_, _ = ok.d.RunOnce(context.Background())
	bad := setup(t, func(*fixture, *http.Request) (*http.Response, error) { return resp(500), nil })
	bad.d.MaxAttempts = 2
	bad.publish(t)
	for i := 0; i < 2; i++ {
		_, _ = bad.d.RunOnce(context.Background())
		*bad.now = bad.now.Add(2 * time.Hour)
	}
	// The counters are process-wide and other tests deliver in parallel, so assert at least our own.
	if count("delivered") < delivered+1 || count("retry") < retry+1 || count("dead") < dead+1 {
		t.Fatalf("delivered %v->%v retry %v->%v dead %v->%v\n%s", delivered, count("delivered"), retry, count("retry"), dead, count("dead"), read())
	}
	if strings.Contains(read(), "evt_") || strings.Contains(read(), "http://") {
		t.Fatal("per-event or URL data leaked into metrics")
	}
}

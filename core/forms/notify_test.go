package forms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	secret := []byte("whsec")
	body := []byte(`{"a":1}`)
	sig := Sign(secret, 1700000000, body)
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != len("sha256=")+64 {
		t.Fatalf("format: %s", sig)
	}
	now := time.Unix(1700000100, 0)
	if err := VerifySignature(secret, "1700000000", sig, body, now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() error{
		"wrong secret":  func() error { return VerifySignature([]byte("x"), "1700000000", sig, body, now, time.Minute*5) },
		"tampered body": func() error { return VerifySignature(secret, "1700000000", sig, []byte(`{"a":2}`), now, time.Minute*5) },
		"tampered ts":   func() error { return VerifySignature(secret, "1700000001", sig, body, now, time.Minute*5) },
		"stale": func() error {
			return VerifySignature(secret, "1700000000", sig, body, now.Add(time.Hour), time.Minute*5)
		},
		"bad ts":         func() error { return VerifySignature(secret, "abc", sig, body, now, time.Minute*5) },
		"missing prefix": func() error { return VerifySignature(secret, "1700000000", sig[7:], body, now, time.Minute*5) },
	} {
		if fn() == nil {
			t.Errorf("%s: expected failure", name)
		}
	}
}

func testEvent() Event {
	l := &Lead{ID: "ld_1", FormID: "contact", Email: "a@example.com", Name: "Ada", Fields: map[string]string{"message": "hi"}, CreatedAt: time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)}
	return NewLeadCreated(l, time.Date(2026, 11, 25, 1, 2, 4, 0, time.UTC))
}

func newSender(url string, attempts int) (*WebhookSender, *[]time.Duration) {
	var sleeps []time.Duration
	return &WebhookSender{
		URL: url, Secret: []byte("whsec"), MaxAttempts: attempts, Backoff: time.Second,
		Now: func() time.Time { return time.Unix(1700000000, 0) },
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	}, &sleeps
}

func TestWebhookDeliversSignedEnvelope(t *testing.T) {
	var gotBody []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s, _ := newSender(srv.URL, 3)
	ev := testEvent()
	n, err := s.Send(context.Background(), ev)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if hdr.Get("Content-Type") != "application/json" || hdr.Get("X-Can-Event-Id") != ev.ID || hdr.Get("X-Can-Event-Type") != "lead.created.v1" {
		t.Fatalf("headers: %v", hdr)
	}
	if err := VerifySignature([]byte("whsec"), hdr.Get("X-Can-Timestamp"), hdr.Get("X-Can-Signature"), gotBody, time.Unix(1700000000, 0), time.Minute); err != nil {
		t.Fatalf("receiver cannot verify: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(gotBody, &env); err != nil || env["type"] != "lead.created.v1" || env["source"] != "forms" {
		t.Fatalf("envelope: %s", gotBody)
	}
}

func TestWebhookRetriesTransientFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	s, sleeps := newSender(srv.URL, 5)
	n, err := s.Send(context.Background(), testEvent())
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != time.Second || (*sleeps)[1] != 2*time.Second {
		t.Fatalf("exponential backoff expected, got %v", *sleeps)
	}
}

func TestWebhookGivesUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	s, _ := newSender(srv.URL, 3)
	n, err := s.Send(context.Background(), testEvent())
	if err == nil || n != 3 || calls.Load() != 3 {
		t.Fatalf("n=%d calls=%d err=%v", n, calls.Load(), err)
	}
}

func TestWebhookNoRetryOnClientError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	s, _ := newSender(srv.URL, 5)
	n, err := s.Send(context.Background(), testEvent())
	if err == nil || n != 1 || calls.Load() != 1 {
		t.Fatalf("4xx is permanent: n=%d calls=%d err=%v", n, calls.Load(), err)
	}
}

func TestWebhookRetriesTooManyRequestsAndNetworkErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	s, _ := newSender(srv.URL, 3)
	if n, err := s.Send(context.Background(), testEvent()); err != nil || n != 2 {
		t.Fatalf("429 should retry: n=%d err=%v", n, err)
	}
	srv.Close()
	if n, err := s.Send(context.Background(), testEvent()); err == nil || n != 3 {
		t.Fatalf("connection errors should retry then fail: n=%d err=%v", n, err)
	}
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var hit atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	s, _ := newSender(srv.URL, 1)
	if _, err := s.Send(context.Background(), testEvent()); err == nil {
		t.Fatal("redirect must count as failure")
	}
	if hit.Load() != 0 {
		t.Fatal("redirect was followed (SSRF risk)")
	}
}

func TestWebhookContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	s := &WebhookSender{URL: srv.URL, Secret: []byte("x"), MaxAttempts: 5, Backoff: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.Send(ctx, testEvent()); err == nil || time.Since(start) > 30*time.Second {
		t.Fatalf("cancel must stop the retry wait promptly: %v after %v", err, time.Since(start))
	}
}

func TestHTTPMailer(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	m := &HTTPMailer{URL: srv.URL, Token: "tok", Client: srv.Client()}
	err := m.Send(context.Background(), Email{To: []string{"o@example.com"}, ReplyTo: "a@example.com", Subject: "S", Text: "T"})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || got["subject"] != "S" || got["text"] != "T" || got["reply_to"] != "a@example.com" || got["from"] != nil {
		t.Fatalf("request: auth=%q body=%v", auth, got)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer failing.Close()
	if err := (&HTTPMailer{URL: failing.URL, Client: failing.Client()}).Send(context.Background(), Email{To: []string{"o@example.com"}}); err == nil {
		t.Fatal("non-2xx must be an error")
	}
}

func TestEmailTextAndSubject(t *testing.T) {
	f := &Form{ID: "c", Name: "Con\r\ntact", Fields: []Field{{Name: "message", Type: "text"}, {Name: "email", Type: "email"}}}
	l := &Lead{ID: "ld_1", Fields: map[string]string{"message": "hi", "email": "a@example.com"}}
	e := BuildNotification(f, l)
	if strings.ContainsAny(e.Subject, "\r\n") {
		t.Fatalf("subject must be single line: %q", e.Subject)
	}
	if !strings.Contains(e.Text, "message: hi") || e.ReplyTo != "" {
		t.Fatalf("text/reply-to: %+v", e)
	}
	l.Email = "a@example.com"
	if BuildNotification(f, l).ReplyTo != "a@example.com" {
		t.Fatal("reply-to should be the submitter")
	}
}

func TestDispatcherShutdownDrains(t *testing.T) {
	st := openTestStore(t)
	m := &fakeMailer{}
	d := NewDispatcher(st, m, nil, nil)
	d.Start(2)
	f := &Form{ID: "c", Name: "C", NotifyEmail: []string{"o@example.com"}}
	for i := 0; i < 10; i++ {
		l := newLead("c", "a@example.com", time.Now())
		if err := st.Insert(context.Background(), l); err != nil {
			t.Fatal(err)
		}
		if !d.Enqueue(l, f) {
			t.Fatal("queue should accept")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.all()) != 10 {
		t.Fatalf("shutdown must drain: %d/10 sent", len(m.all()))
	}
	if d.Enqueue(&Lead{}, f) {
		t.Fatal("enqueue after shutdown must be refused")
	}
}

func TestDispatcherQueueBounded(t *testing.T) {
	st := openTestStore(t)
	d := NewDispatcher(st, &fakeMailer{}, nil, nil)
	d.queue = make(chan job, 2)
	f := &Form{ID: "c", NotifyEmail: []string{"o@example.com"}}
	accepted := 0
	for i := 0; i < 5; i++ {
		if d.Enqueue(&Lead{ID: "x"}, f) {
			accepted++
		}
	}
	if accepted != 2 {
		t.Fatalf("queue must be bounded: accepted %d", accepted)
	}
}

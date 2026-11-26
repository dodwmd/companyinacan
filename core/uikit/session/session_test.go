package session_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// The flows are covered by the conformance suite every module runs; here are the bounds of the table
// and what a module cannot show through its pages.

type fixture struct {
	mu     sync.Mutex
	now    time.Time
	reauth func(token string) (string, error)
	m      *session.Manager[string]
}

func newFixture(maxSessions, perSubject int) *fixture {
	f := &fixture{now: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)}
	f.reauth = func(token string) (string, error) { return token, nil }
	f.m = session.New(session.Config[string]{
		Module: "test", SessionCookie: "s", CSRFCookie: "c",
		Secure:   func(*http.Request) bool { return false },
		Now:      func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		Timeouts: func() (time.Duration, time.Duration) { return time.Hour, 3 * time.Hour },
		Max:      maxSessions, PerSubject: perSubject,
		Recheck: func(string, bool) time.Duration { return 0 },
		Subject: func(p string) string { return p },
		Reauth:  func(_ *http.Request, token string) (string, error) { return f.reauth(token) },
	})
	return f
}

func (f *fixture) advance(d time.Duration) { f.mu.Lock(); f.now = f.now.Add(d); f.mu.Unlock() }

func (f *fixture) start(subject string) *http.Cookie {
	w := httptest.NewRecorder()
	f.m.Start(w, httptest.NewRequest("POST", "/login", nil), subject, subject)
	return w.Result().Cookies()[0]
}

func (f *fixture) request(ck *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(ck)
	return r
}

func (f *fixture) alive(ck *http.Cookie) bool {
	_, _, ok := f.m.Current(httptest.NewRecorder(), f.request(ck))
	return ok
}

func TestTableIsBounded(t *testing.T) {
	f := newFixture(50, 3)
	// the clock does not move: the oldest is still the first one made
	var mine []*http.Cookie
	for range 10 {
		mine = append(mine, f.start("u1"))
	}
	for i, ck := range mine {
		if f.alive(ck) != (i >= 7) {
			t.Errorf("session %d of the subject: alive %v", i, f.alive(ck))
		}
	}
	for i := range 200 {
		f.start("user" + strconv.Itoa(i))
	}
	if f.m.Len() != 50 {
		t.Errorf("table holds %d", f.m.Len())
	}
	f.advance(2 * time.Hour)
	f.start("late")
	if f.m.Len() != 1 {
		t.Errorf("expired sessions are not swept: %d", f.m.Len())
	}
}

func TestLiveDoesNotExtendASession(t *testing.T) {
	f := newFixture(10, 3)
	ck := f.start("u1")
	f.advance(50 * time.Minute)
	if s, _ := f.m.Live(f.request(ck)); s != "u1" {
		t.Fatalf("live %q", s)
	}
	f.advance(20 * time.Minute)
	if s, _ := f.m.Live(f.request(ck)); s != "" || f.alive(ck) {
		t.Error("looking at a session kept it from going idle")
	}
}

func TestARecheckNamingSomebodyElseOrFailingEndsTheSession(t *testing.T) {
	for name, answer := range map[string]func(string) (string, error){
		"another subject": func(string) (string, error) { return "u2", nil },
		"an error":        func(string) (string, error) { return "u1", errors.New("outage") },
	} {
		f := newFixture(10, 3)
		ck := f.start("u1")
		f.reauth = answer
		if f.alive(ck) || f.m.Len() != 0 {
			t.Errorf("%s: the session survived", name)
		}
	}
}

func TestStepUpWithoutAFreshCheckRefuses(t *testing.T) {
	f := newFixture(10, 3)
	ck := f.start("u1")
	s, _, ok := f.m.Current(httptest.NewRecorder(), f.request(ck))
	if !ok {
		t.Fatal("no session")
	}
	if _, err := f.m.StepUp(f.request(ck), s, "", nil); !errors.Is(err, session.ErrStepUpReenter) {
		t.Errorf("no token: %v", err)
	}
	if _, err := f.m.StepUp(f.request(ck), s, "u1", nil); !errors.Is(err, session.ErrStepUpUnconfirmed) {
		t.Errorf("no fresh check: %v", err)
	}
}

func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		site, origin string
		want         bool
	}{
		{"", "", true},
		{"same-origin", "", true},
		{"none", "", true},
		{"cross-site", "", false},
		{"same-site", "", false},
		{"", "http://example.com", true},
		{"", "http://EXAMPLE.com", true},
		{"same-origin", "null", true},
		{"", "null", false},
		{"none", "null", false},
		{"", "http://example.com.evil.test", false},
		{"", "http://example.com:8080", false},
		{"same-origin", "https://evil.example", false},
	} {
		r := httptest.NewRequest("POST", "/", nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := session.SameOrigin(r); got != c.want {
			t.Errorf("site %q origin %q: %v", c.site, c.origin, got)
		}
	}
}

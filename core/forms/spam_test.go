package forms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)} }

func TestTokenTooFastThenOK(t *testing.T) {
	c := newClock()
	s := NewTokenSigner([]byte("k"), 24*time.Hour, c.now)
	tok := s.Issue("contact")
	if err := s.Check("contact", tok, 3*time.Second); err != ErrTooFast {
		t.Fatalf("want ErrTooFast, got %v", err)
	}
	c.advance(4 * time.Second)
	if err := s.Check("contact", tok, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTokenInvalid(t *testing.T) {
	c := newClock()
	s := NewTokenSigner([]byte("k"), time.Hour, c.now)
	other := NewTokenSigner([]byte("other"), time.Hour, c.now)
	tok := s.Issue("contact")
	c.advance(10 * time.Second)
	for name, tk := range map[string]string{
		"empty":    "",
		"garbage":  "abc",
		"no dot":   "YWJj",
		"bad sig":  other.Issue("contact"),
		"other id": s.Issue("newsletter"),
		"tampered": tok[:len(tok)-2] + "AA",
		"non-b64":  "!!!.???",
		"bad ts":   "YWJj.YWJj",
		"dot only": ".",
	} {
		if err := s.Check("contact", tk, time.Second); err != ErrTokenInvalid {
			t.Errorf("%s: want ErrTokenInvalid, got %v", name, err)
		}
	}
}

func TestTokenExpiredAndFuture(t *testing.T) {
	c := newClock()
	s := NewTokenSigner([]byte("k"), time.Hour, c.now)
	tok := s.Issue("contact")
	c.advance(2 * time.Hour)
	if err := s.Check("contact", tok, 0); err != ErrTokenExpired {
		t.Fatalf("want expired, got %v", err)
	}
	c.advance(-5 * time.Hour)
	if err := s.Check("contact", tok, 0); err != ErrTokenInvalid {
		t.Fatalf("token from the future must be invalid, got %v", err)
	}
}

func TestRateLimiter(t *testing.T) {
	c := newClock()
	r := NewRateLimiter(3, 100, c.now)
	for i := 0; i < 3; i++ {
		if ok, _ := r.Allow("1.1.1.1"); !ok {
			t.Fatalf("request %d should pass", i)
		}
	}
	ok, retry := r.Allow("1.1.1.1")
	if ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("4th must be limited with retry-after, got ok=%v retry=%v", ok, retry)
	}
	if ok, _ := r.Allow("2.2.2.2"); !ok {
		t.Fatal("other ip must be independent")
	}
	c.advance(21 * time.Second) // 3/min => 1 token per 20s
	if ok, _ := r.Allow("1.1.1.1"); !ok {
		t.Fatal("token should have refilled")
	}
}

func TestRateLimiterBoundedMemory(t *testing.T) {
	c := newClock()
	r := NewRateLimiter(5, 50, c.now)
	for i := 0; i < 5000; i++ {
		r.Allow("ip-" + strconv.Itoa(i))
		if r.Len() > 50 {
			t.Fatalf("tracked keys exceeded bound: %d", r.Len())
		}
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	r := NewRateLimiter(1000, 10, time.Now)
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func() {
			for i := 0; i < 500; i++ {
				r.Allow(strconv.Itoa(i % 20))
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	if r.Len() > 10 {
		t.Fatal("bound exceeded")
	}
}

func TestSiteVerify(t *testing.T) {
	var gotSecret, gotResp, gotIP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotSecret, gotResp, gotIP = r.PostForm.Get("secret"), r.PostForm.Get("response"), r.PostForm.Get("remoteip")
		if r.PostForm.Get("response") == "good" {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	defer srv.Close()
	v := &SiteVerify{URL: srv.URL, Secret: "s3cret", Client: srv.Client()}
	ok, err := v.Verify(context.Background(), "good", "9.9.9.9")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if gotSecret != "s3cret" || gotResp != "good" || gotIP != "9.9.9.9" {
		t.Fatalf("wrong request: %s %s %s", gotSecret, gotResp, gotIP)
	}
	if ok, err := v.Verify(context.Background(), "bad", ""); ok || err != nil {
		t.Fatalf("bad token must fail cleanly: ok=%v err=%v", ok, err)
	}
	if ok, err := v.Verify(context.Background(), "", ""); ok || err != nil {
		t.Fatal("empty response must fail without calling upstream")
	}
}

func TestSiteVerifyUpstreamErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	v := &SiteVerify{URL: srv.URL, Secret: "s", Client: srv.Client()}
	if ok, err := v.Verify(context.Background(), "x", ""); ok || err == nil {
		t.Fatal("upstream failure must be an error and not a pass")
	}
}

func TestNewCaptchaVerifier(t *testing.T) {
	if v, err := NewCaptchaVerifier("none", "", nil); v != nil || err != nil {
		t.Fatal("none disables captcha")
	}
	if _, err := NewCaptchaVerifier("turnstile", "", nil); err == nil {
		t.Fatal("secret required")
	}
	for _, p := range []string{"turnstile", "hcaptcha"} {
		v, err := NewCaptchaVerifier(p, "s", nil)
		if err != nil || v == nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if CaptchaResponseField("turnstile") != "cf-turnstile-response" || CaptchaResponseField("hcaptcha") != "h-captcha-response" {
		t.Fatal("response field names wrong")
	}
}

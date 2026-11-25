package httpx

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if got := ClientIP(r, false); got != "10.1.2.3" {
		t.Fatalf("untrusted proxy: %s", got)
	}
	if got := ClientIP(r, true); got != "203.0.113.9" {
		t.Fatalf("must use the entry appended by the trusted proxy, not the client-supplied first: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := ClientIP(r, true); got != "10.1.2.3" {
		t.Fatalf("garbage XFF: %s", got)
	}
}

func TestDecodeJSON(t *testing.T) {
	type body struct {
		A string `json:"a"`
	}
	try := func(ct, payload string) (int, body) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(payload))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		var b body
		if DecodeJSON(w, r, &b) {
			return 200, b
		}
		return w.Code, b
	}
	if c, b := try("application/json; charset=utf-8", `{"a":"x"}`); c != 200 || b.A != "x" {
		t.Fatalf("%d %+v", c, b)
	}
	for _, tc := range []struct {
		ct, p string
		want  int
	}{
		{"", `{}`, 415}, {"text/plain", `{}`, 415}, {"application/json", `{"b":1}`, 400},
		{"application/json", `{`, 400}, {"application/json", `{} {}`, 400},
		{"application/json", `{"a":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, 413},
	} {
		if c, _ := try(tc.ct, tc.p); c != tc.want {
			t.Errorf("%q %.20q: %d want %d", tc.ct, tc.p, c, tc.want)
		}
	}
}

func TestMiddlewareRecoversAndNeverLogsSecrets(t *testing.T) {
	var logs bytes.Buffer
	h := Middleware(slog.New(slog.NewJSONHandler(&logs, nil)), "test", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer SECRET-TOKEN")
	r.AddCookie(&http.Cookie{Name: "can_session", Value: "SECRET-COOKIE"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 500 || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	out, _ := io.ReadAll(&logs)
	if strings.Contains(string(out), "SECRET") || !strings.Contains(string(out), "panic in handler") {
		t.Fatalf("%s", out)
	}
}

func TestBearerToken(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer abc")
	if BearerToken(r) != "abc" {
		t.Fatal("bearer")
	}
	r.Header.Set("Authorization", "Basic abc")
	if BearerToken(r) != "" {
		t.Fatal("basic accepted")
	}
}

package people

// The failed-token allowance behind a proxy (specs/045-public-access): per client address as the
// proxy wrote it, never one allowance for everybody and never one the client can choose.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFailedTokensCountPerClientBehindTheIngress(t *testing.T) {
	for _, trust := range []bool{true, false} {
		e := newAPIEnv(t)
		h := (&API{Svc: e.s, Auth: &StaticTokens{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), TrustProxy: trust}).Handler()
		try := func(forwarded ...string) int {
			r := httptest.NewRequest(http.MethodGet, "/v1/people", nil)
			r.RemoteAddr = "10.42.0.5:4000" // the ingress pod
			r.Header.Set("Authorization", "Bearer wrong")
			for _, f := range forwarded {
				r.Header.Add("X-Forwarded-For", f)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Code
		}
		// one client uses up its allowance, sending a different forged address every time ahead of
		// the one the ingress appends
		limited := false
		for i := range 60 {
			forged := "198.51.100." + string(rune('0'+i%10))
			limited = try(forged, "198.51.100.200, 203.0.113.9") == http.StatusTooManyRequests || limited
		}
		if !limited {
			t.Fatalf("trust=%v: forged addresses bought new allowances", trust)
		}
		want := http.StatusTooManyRequests // untrusted: everything behind the proxy shares one
		if trust {
			want = http.StatusUnauthorized
		}
		if got := try("203.0.113.10"); got != want {
			t.Errorf("trust=%v: another client got %d, want %d", trust, got, want)
		}
	}
}

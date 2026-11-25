package obs

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	obslog "github.com/dodwmd/companyinacan/core/obs/log"
)

func testMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/leads/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "boom" {
			http.Error(w, "x", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(obslog.RequestID(r.Context())))
	})
	return mux
}

func TestInstrumentRoutePatternNotRawPath(t *testing.T) {
	reg := NewRegistry()
	h := NewMiddleware(reg).Wrap(testMux())
	for _, p := range []string{"/v1/leads/alice@example.com", "/v1/leads/42", "/v1/leads/boom", "/nope/ssn-123-45-6789"} {
		req := httptest.NewRequest("GET", p+"?email=bob@example.com&token=abc", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	out := expose(t, reg)
	for _, leak := range []string{"alice", "example.com", "leads/42", "bob", "token=abc", "ssn-123", "nope"} {
		if strings.Contains(out, leak) {
			t.Errorf("metrics leak %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{
		`can_http_requests_total{method="GET",route="/v1/leads/{id}",class="2xx"} 2`,
		`can_http_requests_total{method="GET",route="/v1/leads/{id}",class="5xx"} 1`,
		`can_http_requests_total{method="GET",route="unmatched",class="4xx"} 1`,
		`can_http_request_duration_seconds_count{method="GET",route="/v1/leads/{id}"} 3`,
		"can_http_requests_in_flight 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestInstrumentBoundsMethodLabel(t *testing.T) {
	reg := NewRegistry()
	h := NewMiddleware(reg).Wrap(testMux())
	for _, m := range []string{"BREW", "PROPFIND", "get\x00"} {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Method = m
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	out := expose(t, reg)
	if strings.Contains(out, "BREW") || strings.Contains(out, "PROPFIND") || !strings.Contains(out, `method="OTHER"`) {
		t.Fatalf("method label unbounded:\n%s", out)
	}
}

func TestInstrumentFixedRoute(t *testing.T) {
	reg := NewRegistry()
	h := NewMiddleware(reg, FixedRoute("static")).Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/a/b/c", nil))
	if !strings.Contains(expose(t, reg), `route="static"`) {
		t.Fatal("fixed route not used")
	}
}

func TestRequestIDHeaderAndContext(t *testing.T) {
	h := NewMiddleware(NewRegistry()).Wrap(testMux())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/leads/1", nil)
	req.Header.Set("X-Request-Id", "client-id-1")
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "client-id-1" || rec.Body.String() != "client-id-1" {
		t.Fatalf("valid inbound id not propagated: %q %q", rec.Header().Get("X-Request-Id"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/leads/1", nil)
	req.Header.Set("X-Request-Id", strings.Repeat("z", 200))
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); len(got) != 16 {
		t.Fatalf("oversized id trusted: %q", got)
	}
}

func TestMiddlewareServesReadyz(t *testing.T) {
	hl := NewHealth()
	hl.Add("database", true, func(context.Context) error { return nil })
	h := NewMiddleware(NewRegistry(), WithHealth(hl)).Wrap(testMux())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("readyz: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHealthModel(t *testing.T) {
	hl := NewHealth()
	dbErr := error(nil)
	depErr := error(nil)
	hl.Add("database", true, func(context.Context) error { return dbErr })
	hl.Add("can-core", false, func(context.Context) error { return depErr })
	hl.SetTTL(0)
	get := func() (int, string) {
		rec := httptest.NewRecorder()
		hl.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
		return rec.Code, rec.Body.String()
	}
	if c, b := get(); c != 200 || !strings.Contains(b, `"status":"ok"`) {
		t.Fatalf("all ok: %d %s", c, b)
	}
	depErr = errors.New("dial tcp 10.0.0.7:8090: password=hunter2 refused")
	c, b := get()
	if c != 200 || !strings.Contains(b, `"status":"degraded"`) || !strings.Contains(b, `{"name":"can-core","status":"degraded"}`) {
		t.Fatalf("dependency down must degrade, not fail: %d %s", c, b)
	}
	if strings.Contains(b, "hunter2") || strings.Contains(b, "10.0.0.7") || strings.Contains(b, "dial") {
		t.Fatalf("error detail leaked: %s", b)
	}
	dbErr = errors.New("database is locked")
	if c, b := get(); c != 503 || !strings.Contains(b, `"status":"down"`) {
		t.Fatalf("critical down: %d %s", c, b)
	}
}

func TestHealthCheckTimeoutAndPanic(t *testing.T) {
	hl := NewHealth()
	hl.SetTTL(0)
	hl.SetTimeout(30 * time.Millisecond)
	// The check records the deadline it was given: if SetTimeout were ignored it would see the 2 s
	// default. This does not depend on how fast the machine is.
	granted := make(chan time.Duration, 1)
	hl.Add("slow", true, func(ctx context.Context) error {
		if dl, ok := ctx.Deadline(); ok {
			granted <- time.Until(dl)
		} else {
			granted <- time.Hour
		}
		<-ctx.Done()
		return ctx.Err()
	})
	hl.Add("panics", false, func(context.Context) error { panic("boom") })
	rep := hl.Check(context.Background())
	if rep.Status != "down" {
		t.Fatalf("timeout not enforced: %+v", rep)
	}
	if d := <-granted; d > 30*time.Millisecond {
		t.Fatalf("the check was given %v, not the 30ms timeout", d)
	}
}

func TestHealthCachesWithinTTL(t *testing.T) {
	hl := NewHealth()
	n := 0
	hl.Add("db", true, func(context.Context) error { n++; return nil })
	for i := 0; i < 10; i++ {
		hl.Check(context.Background())
	}
	if n != 1 {
		t.Fatalf("checks ran %d times inside the TTL, want 1", n)
	}
}

func scrape(t *testing.T, h http.Handler, auth string, method string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMetricsHandlerToken(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("can_x_total", "x").With().Inc()
	h := MetricsHandler(reg, "s3cret-token")
	if rec := scrape(t, h, "", "GET"); rec.Code != 401 || strings.Contains(rec.Body.String(), "can_x_total") {
		t.Fatalf("no token: %d", rec.Code)
	}
	for _, bad := range []string{"Bearer wrong", "Bearer s3cret-toke", "Bearer s3cret-token-", "s3cret-token", "Basic s3cret-token"} {
		if rec := scrape(t, h, bad, "GET"); rec.Code != 401 {
			t.Errorf("%q accepted: %d", bad, rec.Code)
		}
	}
	rec := scrape(t, h, "Bearer s3cret-token", "GET")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "can_x_total 1") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("valid token: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec := scrape(t, h, "Bearer s3cret-token", "POST"); rec.Code != 405 {
		t.Fatalf("POST: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/other", nil)
	req.Header.Set("Authorization", "Bearer s3cret-token")
	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != 404 {
		t.Fatalf("only /metrics is served: %d", r2.Code)
	}
}

func TestValidTokenNeverThrottled(t *testing.T) {
	h := MetricsHandler(NewRegistry(), "tok")
	for i := 0; i < 500; i++ {
		scrape(t, h, "Bearer bad", "GET")
	}
	if rec := scrape(t, h, "Bearer tok", "GET"); rec.Code != 200 {
		t.Fatalf("valid credentials refused after failures: %d", rec.Code)
	}
}

func TestListenPolicy(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s, err := StartMetrics(context.Background(), env(nil), NewRegistry(), nil); err != nil || s != nil {
		t.Fatalf("must be disabled by default: %v %v", s, err)
	}
	if _, err := StartMetrics(context.Background(), env(map[string]string{"CAN_METRICS_ADDR": ":0"}), NewRegistry(), nil); err == nil {
		t.Fatal("non-loopback listener without a token must be refused")
	}
	if _, err := StartMetrics(context.Background(), env(map[string]string{"CAN_METRICS_ADDR": "0.0.0.0:0"}), NewRegistry(), nil); err == nil {
		t.Fatal("0.0.0.0 without a token must be refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := StartMetrics(ctx, env(map[string]string{"CAN_METRICS_ADDR": "127.0.0.1:0"}), NewRegistry(), nil)
	if err != nil || s == nil {
		t.Fatalf("loopback without token should start: %v", err)
	}
	resp, err := http.Get("http://" + s.Addr().String() + "/metrics")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("scrape: %v", err)
	}
	_ = resp.Body.Close()
	s2, err := StartMetrics(ctx, env(map[string]string{"CAN_METRICS_ADDR": "0.0.0.0:0", "CAN_METRICS_TOKEN": "t"}), NewRegistry(), nil)
	if err != nil || s2 == nil {
		t.Fatalf("non-loopback with token: %v", err)
	}
	if _, ok := s2.Addr().(*net.TCPAddr); !ok {
		t.Fatal("addr type")
	}
	if _, err := StartMetrics(ctx, env(map[string]string{"CAN_METRICS_ADDR": "not an addr"}), NewRegistry(), nil); err == nil {
		t.Fatal("bad address accepted")
	}
}

func BenchmarkMiddleware(b *testing.B) {
	h := NewMiddleware(NewRegistry()).Wrap(testMux())
	req := httptest.NewRequest("GET", "/v1/leads/42", nil)
	w := &nopWriter{h: http.Header{}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(w, req)
	}
}

func BenchmarkBaselineNoMiddleware(b *testing.B) {
	h := testMux()
	req := httptest.NewRequest("GET", "/v1/leads/42", nil)
	w := &nopWriter{h: http.Header{}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(w, req)
	}
}

type nopWriter struct{ h http.Header }

func (n *nopWriter) Header() http.Header         { return n.h }
func (n *nopWriter) Write(p []byte) (int, error) { return len(p), nil }
func (n *nopWriter) WriteHeader(int)             {}

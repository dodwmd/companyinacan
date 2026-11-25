package httpx

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/obs"
)

func obsMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/things/{id}", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]string{"rid": RequestID(r.Context())})
	})
	return mux
}

func TestMiddlewareRequestIDPolicy(t *testing.T) {
	var logs bytes.Buffer
	h := Middleware(slog.New(slog.NewJSONHandler(&logs, nil)), "admin", obsMux())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/things/7?secret=abc", nil)
	req.Header.Set("X-Request-Id", "trace-123")
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "trace-123" || !strings.Contains(rec.Body.String(), "trace-123") {
		t.Fatalf("valid inbound id not propagated: %v %s", rec.Header(), rec.Body.String())
	}
	if !strings.Contains(logs.String(), `"request_id":"trace-123"`) {
		t.Fatalf("log lacks request id: %s", logs.String())
	}
	if strings.Contains(logs.String(), "secret=abc") || strings.Contains(logs.String(), "/things/7") {
		t.Fatalf("query string or raw path logged: %s", logs.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/things/7", nil)
	req.Header.Set("X-Request-Id", "evil\nid with spaces"+strings.Repeat("x", 100))
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); len(got) != 16 || strings.ContainsAny(got, "\n ") {
		t.Fatalf("untrusted id echoed: %q", got)
	}
}

func TestMiddlewareServesReadyzAndCountsRoutes(t *testing.T) {
	obs.DefaultHealth.Add("test-httpx", true, func(context.Context) error { return nil })
	h := Middleware(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), "admin", obsMux())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"components"`) {
		t.Fatalf("readyz: %d %s", rec.Code, rec.Body.String())
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/things/alice@example.com", nil))
	var b strings.Builder
	_, _ = obs.Default.WriteTo(&b)
	if !strings.Contains(b.String(), `route="/api/v1/things/{id}"`) || strings.Contains(b.String(), "alice") {
		t.Fatalf("route label wrong or leaking:\n%s", b.String())
	}
}

package finance

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The access log carries the matched route pattern, so a bearer-like path segment (the signed
// invoice link) can never reach it, and a client-supplied request id is validated.
func TestAccessLogUsesRoutePatternNotThePath(t *testing.T) {
	var logs bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /public/invoices/{token}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := withMiddleware(slog.New(slog.NewJSONHandler(&logs, nil)), mux)
	req := httptest.NewRequest("GET", "/public/invoices/SECRETLINKTOKEN?x=SECRETQUERY", nil)
	req.Header.Set("X-Request-Id", "bad id\nforged")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := logs.String()
	if strings.Contains(out, "SECRET") {
		t.Fatalf("path or query reached the log: %s", out)
	}
	if !strings.Contains(out, `"route":"/public/invoices/{token}"`) && !strings.Contains(out, `"route":"GET /public/invoices/{token}"`) {
		t.Fatalf("route missing: %s", out)
	}
	if strings.Contains(rec.Header().Get("X-Request-Id"), "forged") {
		t.Fatal("untrusted request id echoed")
	}
}

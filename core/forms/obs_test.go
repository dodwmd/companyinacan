package forms

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
)

func metricText() string {
	var b strings.Builder
	_, _ = obs.Default.WriteTo(&b)
	return b.String()
}

func metricValue(name string) string {
	for _, l := range strings.Split(metricText(), "\n") {
		if strings.HasPrefix(l, name+" ") {
			return strings.Fields(l)[1]
		}
	}
	return ""
}

func TestLeadCounterAndReadiness(t *testing.T) {
	s := openTestStore(t)
	before := metricValue("can_forms_leads_created_total")
	if err := s.Insert(context.Background(), newLead("contact", "pii@example.com", time.Now())); err != nil {
		t.Fatal(err)
	}
	after := metricValue("can_forms_leads_created_total")
	if after == before || after == "" {
		t.Fatalf("lead counter did not move: %q -> %q", before, after)
	}
	if strings.Contains(metricText(), "pii@example.com") {
		t.Fatal("lead data in metrics")
	}
	// the store registered its readiness checks
	h := withMiddleware(slog.New(slog.NewJSONHandler(io.Discard, nil)), "api", http.NotFoundHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"database"`) || !strings.Contains(rec.Body.String(), `"migrations"`) {
		t.Fatalf("readyz: %d %s", rec.Code, rec.Body.String())
	}
}

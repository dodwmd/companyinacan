package adminui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/registry"
)

type fakeEraser struct {
	*fakeLeads
	asked  []string
	erased []string
	err    error
}

func (f *fakeEraser) Find(_ context.Context, email string) ([]LeadMatch, bool, error) {
	f.asked = append(f.asked, email)
	return []LeadMatch{{ID: "7", FormID: "contact", Status: "new", CreatedAt: fixedNow, Match: "address"}, {ID: "8", FormID: "contact", Status: "new", CreatedAt: fixedNow, Match: "mention"}}, false, f.err
}

func (f *fakeEraser) Erase(_ context.Context, id string) error {
	if f.err == nil {
		f.erased = append(f.erased, id)
	}
	return f.err
}

func eraserOf(h *harness) *fakeEraser {
	h.leads.leads = []Lead{{ID: "7", FormID: "contact", Status: "new", Email: "ann@example.com", Name: "Ann", CreatedAt: fixedNow, UpdatedAt: fixedNow}}
	f := &fakeEraser{fakeLeads: h.leads}
	h.ui.Leads = f
	return f
}

func auditText(t *testing.T, h *harness) string {
	t.Helper()
	recs, err := h.audit.Query(context.Background(), audit.Filter{ActionPrefix: "lead.", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLeadErasurePages(t *testing.T) {
	h := newHarness(t)
	f := eraserOf(h)
	s := h.login()

	for path, want := range map[string]string{"/leads": `href="/lead-erasure"`, "/leads/7": `href="/leads/7/erase"`, "/lead-erasure": `action="/lead-erasure"`, "/leads/7/erase": `action="/leads/7/erase"`} {
		r := h.do("GET", path, nil, s.with()...)
		if r.StatusCode != 200 || !strings.Contains(r.body, want) {
			t.Errorf("%s: %d without %s\n%s", path, r.StatusCode, want, r.body)
		}
		for _, p := range a11yProblems(r.body) {
			t.Errorf("%s: %s", path, p)
		}
	}

	r := h.do("POST", "/lead-erasure", url.Values{"csrf": {s.csrf}, "email": {"ann@example.com"}}, s.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, `href="/leads/7/erase"`) || !strings.Contains(r.body, "Only mentioned") {
		t.Fatalf("search: %d %s", r.StatusCode, r.body)
	}
	for _, p := range a11yProblems(r.body) {
		t.Errorf("results: %s", p)
	}
	if len(f.asked) != 1 || f.asked[0] != "ann@example.com" {
		t.Errorf("asked %v", f.asked)
	}
	if r := h.do("POST", "/lead-erasure", url.Values{"csrf": {s.csrf}, "email": {"nonsense"}}, s.with()...); r.StatusCode != 400 {
		t.Errorf("bad address: %d", r.StatusCode)
	}
	if r := h.do("POST", "/lead-erasure", url.Values{"email": {"ann@example.com"}}, s.with()...); r.StatusCode != 403 {
		t.Errorf("without csrf: %d", r.StatusCode)
	}

	if r := h.do("POST", "/leads/7/erase", url.Values{"csrf": {s.csrf}, "confirm": {"8"}}, s.with()...); r.StatusCode != 400 || len(f.erased) != 0 {
		t.Fatalf("wrong confirmation: %d %v", r.StatusCode, f.erased)
	}
	r = h.do("POST", "/leads/7/erase", url.Values{"csrf": {s.csrf}, "confirm": {"7"}}, s.with()...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/leads?flash=flash.lead_erased" || len(f.erased) != 1 {
		t.Fatalf("erase: %d %s %v", r.StatusCode, r.Header.Get("Location"), f.erased)
	}
	trail := auditText(t, h)
	if !strings.Contains(trail, "lead.erase") || !strings.Contains(trail, "lead.find") {
		t.Errorf("audit: %s", trail)
	}
	if strings.Contains(trail, "ann@example.com") || strings.Contains(h.logs.String(), "ann@example.com") {
		t.Error("the address is in the audit trail or the log")
	}

	// An old sign-in must be proved again before anything is destroyed.
	*h.now = h.now.Add(time.Hour)
	r = h.do("POST", "/leads/7/erase", url.Values{"csrf": {s.csrf}, "confirm": {"7"}}, s.with()...)
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/reauth?next=") || len(f.erased) != 1 {
		t.Errorf("stale session: %d %s %v", r.StatusCode, r.Header.Get("Location"), f.erased)
	}
}

func TestLeadErasureNeedsThePermission(t *testing.T) {
	p := newPUI(t)
	f := eraserOf(p.harness)
	if _, err := p.db.ExecContext(context.Background(), `INSERT INTO rbac_assignments(subject, role, scope, granted_by, granted_at) VALUES ('user:alice','operator','','test',0)`); err != nil {
		t.Fatal(err)
	}
	p.rb.Invalidate()
	alice := p.loginAs("alice") // may view and change leads, not erase them
	for _, c := range []struct{ method, path string }{
		{"GET", "/lead-erasure"}, {"POST", "/lead-erasure"}, {"GET", "/leads/7/erase"}, {"POST", "/leads/7/erase"},
	} {
		r := p.do(c.method, c.path, url.Values{"csrf": {alice.csrf}, "email": {"ann@example.com"}, "confirm": {"7"}}, alice.with()...)
		if r.StatusCode != 403 {
			t.Errorf("%s %s: %d", c.method, c.path, r.StatusCode)
		}
	}
	if r := p.do("GET", "/leads/7", nil, alice.with()...); r.StatusCode != 200 || strings.Contains(r.body, "/erase") {
		t.Errorf("lead page: %d, offers erasure", r.StatusCode)
	}
	if len(f.asked)+len(f.erased) != 0 {
		t.Error(f.asked, f.erased)
	}
}

type oneProvider struct{ url string }

func (o oneProvider) Resolve(context.Context, string) (registry.Resolution, error) {
	return registry.Resolution{Providers: []registry.Provider{{URL: o.url}}}, nil
}

func TestHTTPLeadsEraseCalls(t *testing.T) {
	var seen []string
	status, answer := 200, `{"items":[{"id":"ld_1","match":"address"}],"truncated":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.String()+" "+string(b))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	c := &HTTPLeads{Resolver: oneProvider{srv.URL}, Token: func() (string, error) { return "t", nil }}
	ctx := context.Background()
	items, truncated, err := c.Find(ctx, "a@b.example")
	if err != nil || !truncated || len(items) != 1 || seen[0] != `POST /v1/leads/find {"email":"a@b.example"}` {
		t.Fatal(items, truncated, err, seen)
	}
	if err := c.Erase(ctx, "ld_1"); err != nil || seen[1] != "DELETE /v1/leads/ld_1 " {
		t.Fatal(err, seen)
	}
	for _, c2 := range []struct {
		status int
		body   string
		want   error
	}{{403, "", ErrLeadsForbidden}, {404, "", ErrLeadNotFound}, {500, `{"code":"scrub_failed"}`, ErrLeadScrub}} {
		status, answer = c2.status, c2.body
		if err := c.Erase(ctx, "ld_1"); err != c2.want { //nolint:errorlint // the sentinel itself
			t.Errorf("%d: %v", c2.status, err)
		}
	}
}

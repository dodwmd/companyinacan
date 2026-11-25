package forms

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (h *harness) seed(n int) []*Lead {
	h.t.Helper()
	var out []*Lead
	for i := 0; i < n; i++ {
		form := "contact"
		if i%2 == 1 {
			form = "quote"
		}
		l := &Lead{FormID: form, Email: "p" + string(rune('a'+i)) + "@example.com", Name: "P", Fields: map[string]string{"message": "m" + string(rune('a'+i))}, CreatedAt: h.clock.now().Add(time.Duration(i) * time.Minute), RetentionUntil: h.clock.now().Add(time.Hour)}
		if err := h.app.store.Insert(context.Background(), l); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func TestAPIRequiresAuth(t *testing.T) {
	h := newHarness(t, harnessCfg)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/leads"}, {"GET", "/v1/leads/ld_x"}, {"PATCH", "/v1/leads/ld_x"}, {"GET", "/v1/leads/export.csv"},
	} {
		if rec := h.apiDo(tc.method, tc.path, "", ""); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s without token: %d", tc.method, tc.path, rec.Code)
		}
		if rec := h.apiDo(tc.method, tc.path, "wrong-token-0123456789", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong token: %d", tc.method, tc.path, rec.Code)
		}
	}
	rec := h.do(h.api, http.MethodGet, "/v1/leads", nil, map[string]string{"Authorization": "Basic abc"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-bearer scheme: %d", rec.Code)
	}
}

func TestAPIPermissions(t *testing.T) {
	h := newHarness(t, harnessCfg)
	ls := h.seed(1)
	if rec := h.apiDo("GET", "/v1/leads", readToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("read token list: %d", rec.Code)
	}
	if rec := h.apiDo("PATCH", "/v1/leads/"+ls[0].ID, readToken, `{"status":"closed"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("read token must not write: %d", rec.Code)
	}
	if rec := h.apiDo("PATCH", "/v1/leads/"+ls[0].ID, adminToken, `{"status":"closed"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin write: %d %s", rec.Code, rec.Body)
	}
}

func TestAPIListFilterPaging(t *testing.T) {
	h := newHarness(t, harnessCfg)
	h.seed(5)
	rec := h.apiDo("GET", "/v1/leads?limit=2", adminToken, "")
	var page struct {
		Items      []Lead `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page 1: %v %s", err, rec.Body)
	}
	seen := map[string]bool{page.Items[0].ID: true, page.Items[1].ID: true}
	for page.NextCursor != "" {
		rec = h.apiDo("GET", "/v1/leads?limit=2&cursor="+page.NextCursor, adminToken, "")
		page.NextCursor = ""
		page.Items = nil
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		for _, l := range page.Items {
			if seen[l.ID] {
				t.Fatalf("duplicate across pages: %s", l.ID)
			}
			seen[l.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d, want 5", len(seen))
	}
	rec = h.apiDo("GET", "/v1/leads?form=quote", adminToken, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Items) != 2 || page.NextCursor != "" {
		t.Fatalf("form filter: %d", len(page.Items))
	}
	for _, bad := range []string{"?limit=abc", "?limit=0", "?status=bogus", "?cursor=x", "?since=yesterday", "?form=Bad%20Id"} {
		if rec := h.apiDo("GET", "/v1/leads"+bad, adminToken, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400 got %d", bad, rec.Code)
		}
	}
}

func TestAPIGetAndPatch(t *testing.T) {
	h := newHarness(t, harnessCfg)
	l := h.seed(1)[0]
	_ = h.app.store.RecordDelivery(context.Background(), l.ID, "webhook", true, 2, "")
	rec := h.apiDo("GET", "/v1/leads/"+l.ID, adminToken, "")
	var got struct {
		Lead
		Deliveries []Delivery `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != l.ID || len(got.Deliveries) != 1 || got.Deliveries[0].Attempts != 2 {
		t.Fatalf("get: %v %s", err, rec.Body)
	}
	if rec := h.apiDo("GET", "/v1/leads/ld_missing", adminToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	rec = h.apiDo("PATCH", "/v1/leads/"+l.ID, adminToken, `{"status":"contacted","note":"called"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"contacted"`) {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	for name, body := range map[string]string{
		"bad status":    `{"status":"bogus"}`,
		"unknown key":   `{"email":"x@y.z"}`,
		"empty":         `{}`,
		"not json":      `nope`,
		"long note":     `{"note":"` + strings.Repeat("a", 3000) + `"}`,
		"note ctl char": `{"note":"a\u0000b"}`,
	} {
		if rec := h.apiDo("PATCH", "/v1/leads/"+l.ID, adminToken, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400 got %d", name, rec.Code)
		}
	}
	if rec := h.apiDo("PATCH", "/v1/leads/ld_missing", adminToken, `{"status":"closed"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing: %d", rec.Code)
	}
	if rec := h.apiDo("DELETE", "/v1/leads/"+l.ID, adminToken, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete: %d", rec.Code)
	}
}

func TestAPIExportCSV(t *testing.T) {
	h := newHarness(t, harnessCfg)
	evil := &Lead{FormID: "contact", Email: "e@example.com", Name: "=cmd|' /C calc'!A0", Fields: map[string]string{"message": "@SUM(1)"}, CreatedAt: h.clock.now(), RetentionUntil: h.clock.now().Add(time.Hour)}
	if err := h.app.store.Insert(context.Background(), evil); err != nil {
		t.Fatal(err)
	}
	rec := h.apiDo("GET", "/v1/leads/export.csv", readToken, "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") || !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers: %d %v", rec.Code, rec.Header())
	}
	rows, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("csv: %v %v", err, rows)
	}
	if rows[1][5] != "'=cmd|' /C calc'!A0" || rows[1][7] != "'@SUM(1)" {
		t.Fatalf("formula injection not neutralised: %q", rows[1])
	}
}

func TestAPINoCORSAndMethods(t *testing.T) {
	h := newHarness(t, harnessCfg)
	rec := h.do(h.api, http.MethodOptions, "/v1/leads", nil, map[string]string{"Origin": "https://www.example.com", "Access-Control-Request-Method": "GET"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("the leads API is server-to-server and must not enable CORS")
	}
	if rec := h.apiDo("GET", "/v1/nothing", adminToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route: %d", rec.Code)
	}
}

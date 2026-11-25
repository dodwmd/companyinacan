package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/registry"
)

type fakeResolver struct {
	url string
	err error
	got string
}

func (f *fakeResolver) Resolve(_ context.Context, c string) (registry.Resolution, error) {
	f.got = c
	if f.err != nil {
		return registry.Resolution{}, f.err
	}
	return registry.Resolution{Capability: c, Providers: []registry.Provider{{ModuleID: "forms", URL: f.url}}}, nil
}

func TestHTTPLeadsSpeaksTheLeadsContract(t *testing.T) {
	var gotAuth, gotMethod, gotPath, gotQuery, gotBody string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath, gotQuery = r.Header.Get("Authorization"), r.Method, r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch {
		case r.URL.Path == "/v1/leads" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"items":[{"id":"7","form_id":"contact","status":"new","email":"a@example.com","fields":{"k":"v","n":1},"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}],"next_cursor":"9"}`))
		case r.URL.Path == "/v1/leads/7" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"id":"7","form_id":"contact","status":"new","fields":{},"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`))
		case r.URL.Path == "/v1/leads/7" && r.Method == "PATCH":
			_, _ = w.Write([]byte(`{"id":"7","status":"closed"}`))
		case r.URL.Path == "/v1/leads/403":
			w.WriteHeader(403)
		case r.URL.Path == "/v1/leads/redirect":
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
		case r.URL.Path == "/v1/leads/big":
			_, _ = w.Write([]byte(`{"id":"` + strings.Repeat("x", maxLeadsResponse+10) + `"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer gw.Close()
	res := &fakeResolver{url: gw.URL}
	token := "tok-1"
	c := &HTTPLeads{Resolver: res, Token: func() (string, error) { return token, nil }}
	ctx := context.Background()

	page, err := c.List(ctx, LeadQuery{Status: "new", Cursor: "3", Limit: 25})
	if err != nil || len(page.Items) != 1 || page.NextCursor != "9" || page.Items[0].Email != "a@example.com" || page.Items[0].Fields["k"] != "v" {
		t.Fatalf("%+v %v", page, err)
	}
	if res.got != "leads" || gotAuth != "Bearer tok-1" || gotQuery != "cursor=3&limit=25&status=new" {
		t.Fatalf("capability=%q auth=%q query=%q", res.got, gotAuth, gotQuery)
	}
	if _, err := c.Get(ctx, "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "nope"); !errors.Is(err, ErrLeadNotFound) {
		t.Fatalf("404: %v", err)
	}
	if _, err := c.Get(ctx, "../etc"); !errors.Is(err, ErrLeadNotFound) {
		t.Fatalf("path escape: %v", err)
	}
	if err := c.SetStatus(ctx, "7", "closed"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PATCH" || gotPath != "/v1/leads/7" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(gotBody), &body)
	if len(body) != 1 || body["status"] != "closed" {
		t.Fatalf("only status may be sent, got %s", gotBody)
	}
	if _, err := c.Get(ctx, "403"); err == nil || errors.Is(err, ErrLeadNotFound) {
		t.Fatalf("403 must be a gateway failure, got %v", err)
	}
	if _, err := c.Get(ctx, "redirect"); err == nil {
		t.Fatal("redirect followed")
	}
	if _, err := c.Get(ctx, "big"); err == nil {
		t.Fatal("oversized response accepted")
	}

	// no provider / no token => unavailable
	res.err = registry.ErrNoProvider
	if _, err := c.List(ctx, LeadQuery{}); !errors.Is(err, ErrLeadsUnavailable) {
		t.Fatalf("%v", err)
	}
	res.err = nil
	token = ""
	if _, err := c.List(ctx, LeadQuery{}); !errors.Is(err, ErrLeadsUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestFileTokenReadsFreshEachCall(t *testing.T) {
	p := t.TempDir() + "/tok"
	f := FileToken(p)
	if _, err := f(); !errors.Is(err, ErrLeadsUnavailable) {
		t.Fatalf("missing file: %v", err)
	}
	writeFile(t, p, "abc\n")
	if v, err := f(); err != nil || v != "abc" {
		t.Fatalf("%q %v", v, err)
	}
	writeFile(t, p, "def")
	if v, _ := f(); v != "def" {
		t.Fatalf("%q", v)
	}
	if v, err := FileToken("")(); v != "" || !errors.Is(err, ErrLeadsUnavailable) {
		t.Fatalf("%q %v", v, err)
	}
}

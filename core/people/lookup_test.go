package people

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The credential can-core copies to equity answers two questions and nothing else.
func TestLookupTokenIsReadOnlyAndMinimal(t *testing.T) {
	ctx := context.Background()
	kr, err := ParseKeyring("k1 " + strings.Repeat("ab", 32) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := Open(filepath.Join(t.TempDir(), "p.db"), Options{Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	hr := Actor{Subject: "svc-can-core", Roles: []string{RoleHR, RoleAdmin}}
	ann, err := svc.CreatePerson(ctx, hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Ann", FamilyName: "Secretname", WorkEmail: "ann@example.com", SSOSubject: "user:ann"})
	if err != nil {
		t.Fatal(err)
	}
	st := &StaticTokens{}
	tok := strings.Repeat("l00kup", 6)
	AddLookupToken(st, tok)
	h := (&API{Svc: svc, Auth: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	do := func(method, path, body string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	for path, want := range map[string]string{
		"/v1/people?limit=2&sso_subject=user%3Aann":    `{"items":[{"id":"` + ann.ID + `","sso_subject":"user:ann"}]}`,
		"/v1/people?limit=2&sso_subject=user%3Anobody": `{"items":[]}`,
		"/v1/people/" + ann.ID:                         `{"id":"` + ann.ID + `"}`,
	} {
		if code, got := do(http.MethodGet, path, ""); code != http.StatusOK || got != want {
			t.Errorf("GET %s = %d %s, want %s", path, code, got, want)
		}
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/people"},
		{"GET", "/v1/people?sso_subject="},
		{"GET", "/v1/people?sso_subject=user%3Aann&status=active"},
		{"GET", "/v1/people/" + ann.ID + "/sensitive"},
		{"GET", "/v1/people/" + ann.ID + "/history"},
		{"GET", "/v1/erasures"},
		{"GET", "/v1/tasks"},
		{"POST", "/v1/people"},
		{"PATCH", "/v1/people/" + ann.ID},
		{"POST", "/v1/people/" + ann.ID + "/erasure"},
		{"POST", "/v1/import/people"},
	} {
		if code, got := do(c.method, c.path, `{}`); code != http.StatusForbidden {
			t.Errorf("%s %s = %d %s, want 403", c.method, c.path, code, got)
		}
	}
	// Nothing for the web UI either.
	u := &UI{Svc: svc, Auth: st}
	if _, err := u.authenticateToken(httptest.NewRequest(http.MethodGet, "/", nil), tok); err == nil {
		t.Error("the lookup token signs in to the web UI")
	}
}

package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

const apiCompany = "schema: 1\nname: Acme\ndomain: acme.example\nprovider:\n  type: local\n  options: {server_ram_mb: 2048}\nmodules:\n  web: {enabled: true}\n"

func apiModules(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	w := func(id, body string) {
		d := filepath.Join(dir, id)
		if err := os.MkdirAll(filepath.Join(d, "chart"), 0o755); err != nil {
			t.Fatal(err)
		}
		head := "schema: 1\nid: " + id + "\nname: \"" + id + " <b>\"\ndescription: d\nversion: 1.0.0\nprovider: {type: inhouse, name: can}\nruntime: {chart: ./chart}\nrequires: {database: sqlite}\n"
		if err := os.WriteFile(filepath.Join(d, "module.yaml"), []byte(head+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("web", "resources: {ram_mb: 20}\nprovides:\n  - {capability: leads, schema: leads.v1, service: api, port: 8081}\n")
	w("plan", "status: experimental\nresources: {ram_mb: 10}\nprovides:\n  - {capability: invoices, schema: invoices.v1, service: api, port: 8081, status: planned}\n")
	return dir
}

func TestCatalogueAndStatusEndpointsAreGatedAndReadOnly(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	pw := "correct horse battery staple"
	hash, _ := auth.HashPassword(pw)
	secrets := map[string]string{}
	for _, u := range []string{"root", "nobody"} {
		secrets[u] = auth.NewTOTPSecret()
		if _, err := db.CreateAdmin(ctx, u, hash, secrets[u]); err != nil {
			t.Fatal(err)
		}
	}
	al := audit.New(db)
	cfile := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(cfile, []byte(apiCompany), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &modmgr.Service{DB: db, Audit: al, ModulesDir: apiModules(t), Company: &modmgr.FileStore{Path: cfile}, Now: func() time.Time { return now }}
	s := &Server{
		Auth: auth.NewService(db, al, auth.Options{Now: func() time.Time { return now }, MinLoginTime: time.Nanosecond, Sleep: func(context.Context, time.Duration) error { return nil },
			Policy: auth.Policy{HardLock: true, MaxUserFailures: 5, MaxIPFailures: 20, Window: 15 * time.Minute, LockFor: 15 * time.Minute}}),
		DB: db, Audit: al, Registry: registry.New(db, "http://can-core"), RBAC: rbac.New(db, al, rbac.Options{Now: func() time.Time { return now }}),
		Mod: svc, Log: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), Secure: true, Now: func() time.Time { return now },
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	login := func(user string) *http.Cookie {
		now = now.Add(31 * time.Second)
		k, _ := auth.DecodeTOTPSecret(secrets[user])
		b, _ := json.Marshal(map[string]string{"username": user, "password": pw, "totp": auth.Code(k, auth.SHA1, 6, 30, now)})
		resp, err := http.Post(srv.URL+"/api/v1/login", "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == auth.CookieName {
				return c
			}
		}
		t.Fatalf("login %s: %d", user, resp.StatusCode)
		return nil
	}
	get := func(path string, c *http.Cookie) (int, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if c != nil {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	for _, p := range []string{"/api/v1/catalogue", "/api/v1/status"} {
		if code, _ := get(p, nil); code != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d", p, code)
		}
		if code, _ := get(p, login("nobody")); code != http.StatusForbidden {
			t.Errorf("%s without platform.view = %d", p, code)
		}
	}
	root := login("root")
	code, body := get("/api/v1/catalogue", root)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var cat struct {
		Modules []struct {
			ID, Status string
			Enabled    bool
			Deployable bool
			Provides   []provideEntry
		}
		Footprint footprintView
		Conflicts []string
	}
	if err := json.Unmarshal(body, &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Modules) != 2 || cat.Modules[0].ID != "plan" || cat.Modules[0].Status != "experimental" || cat.Modules[0].Provides[0].Served {
		t.Fatalf("%s", body)
	}
	if !cat.Modules[1].Enabled || cat.Footprint.ServerMB != 2048 || cat.Footprint.TotalMB != modmgr.PlatformBaselineMB+20 || cat.Footprint.Verdict != "fits" {
		t.Fatalf("%s", body)
	}
	code, body = get("/api/v1/status", root)
	if code != 200 || !strings.Contains(string(body), `"certificates_available":false`) || !strings.Contains(string(body), `"disk_available":false`) {
		t.Fatalf("%d %s", code, body)
	}
	// no module management: 503, not a panic
	s.Mod = nil
	if code, _ := get("/api/v1/catalogue", root); code != http.StatusServiceUnavailable {
		t.Fatalf("%d", code)
	}
}

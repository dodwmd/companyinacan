package runtime

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnvDefaultsAndOverrides(t *testing.T) {
	c, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminAddr != ":8080" || c.ModuleAddr != ":8090" || !c.SecureCookies || c.TrustProxy || c.TokenReview != "kubernetes" || c.DBPath == "" {
		t.Fatalf("%+v", c)
	}
	env := map[string]string{"CAN_ADMIN_ADDR": "127.0.0.1:9", "CAN_COOKIE_SECURE": "false", "CAN_TRUST_PROXY": "true", "CAN_DB_PATH": "/x/y.db", "CAN_TOKEN_REVIEW": "fake", "CAN_MEMLIMIT_MB": "32"}
	c, err = FromEnv(func(k string) string { return env[k] })
	if err != nil || c.AdminAddr != "127.0.0.1:9" || c.SecureCookies || !c.TrustProxy || c.DBPath != "/x/y.db" || c.TokenReview != "fake" || c.MemLimitMB != 32 {
		t.Fatalf("%+v %v", c, err)
	}
	env["CAN_COMPANY_NAME"], env["CAN_COMPANY_DOMAIN"], env["CAN_LEADS_TOKEN_FILE"] = "Acme", "acme.example", "/run/leads/token"
	if c, _ = FromEnv(func(k string) string { return env[k] }); c.CompanyName != "Acme" || c.CompanyDomain != "acme.example" || c.LeadsTokenFile != "/run/leads/token" {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []map[string]string{{"CAN_COOKIE_SECURE": "maybe"}, {"CAN_TOKEN_REVIEW": "magic"}, {"CAN_MEMLIMIT_MB": "-1"}, {"CAN_ADMIN_ADDR": "x", "CAN_MODULE_ADDR": "x"}} {
		if _, err := FromEnv(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := noKeepAlive.Get(url) //nolint:gosec,noctx // test against a local listener
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestRunServesTwoSeparateListenersAndShutsDownGracefully(t *testing.T) {
	cfg := Config{
		AdminAddr: "127.0.0.1:0", ModuleAddr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "state.db"),
		SecureCookies: false, TokenReview: "fake", PlatformURL: "http://can-core", ShutdownTimeout: 20 * time.Second,
	}
	addrs := make(chan [2]net.Addr, 1)
	cfg.OnListen = func(a, m net.Addr) { addrs <- [2]net.Addr{a, m} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))) }()

	var a [2]net.Addr
	select {
	case a = <-addrs:
	case err := <-done:
		t.Fatalf("exited early: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("no listeners")
	}
	admin, mod := "http://"+a[0].String(), "http://"+a[1].String()
	if get(t, admin+"/healthz") != 200 || get(t, mod+"/healthz") != 200 {
		t.Fatal("healthz")
	}
	// the listeners are separate: each API is absent from the other port
	if get(t, admin+"/v1/self") != 404 {
		t.Fatal("module API reachable on the admin listener")
	}
	if get(t, mod+"/api/v1/session") != 404 {
		t.Fatal("admin API reachable on the module listener")
	}
	if get(t, admin+"/api/v1/session") != 401 || get(t, mod+"/v1/self") != 401 {
		t.Fatal("protected routes must demand credentials")
	}
	// the admin UI is served on the admin listener only
	if get(t, admin+"/login") != 200 || get(t, admin+"/static/admin.css") != 200 || get(t, mod+"/login") != 404 {
		t.Fatal("admin UI mounting")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(45 * time.Second): // a loaded CI runner under -race; the code's own bound is ShutdownTimeout
		t.Fatal("no graceful shutdown")
	}
	if _, err := http.Get(admin + "/healthz"); err == nil { //nolint:gosec,noctx // listener must be closed
		t.Fatal("still serving after shutdown")
	}
}

func TestRunFailsFastOnBadDatabasePath(t *testing.T) {
	cfg := Config{AdminAddr: "127.0.0.1:0", ModuleAddr: "127.0.0.1:0", DBPath: "/nonexistent-dir/x/state.db", TokenReview: "fake"}
	if err := Run(context.Background(), cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		t.Fatal("expected an error")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func body(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := noKeepAlive.Do(req) //nolint:gosec // local test listener
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRunObservability(t *testing.T) {
	metrics := freeAddr(t)
	env := map[string]string{"CAN_METRICS_ADDR": metrics, "CAN_METRICS_TOKEN": "tok-123", "CAN_RAM_DECLARED_MB": "48"}
	cfg := Config{
		AdminAddr: "127.0.0.1:0", ModuleAddr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "state.db"),
		TokenReview: "fake", PlatformURL: "http://can-core", ShutdownTimeout: 20 * time.Second, Version: "v9.9.9", Commit: "c0ffee",
		Env: func(k string) string { return env[k] },
	}
	addrs := make(chan [2]net.Addr, 1)
	cfg.OnListen = func(a, m net.Addr) { addrs <- [2]net.Addr{a, m} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))) }()
	var a [2]net.Addr
	select {
	case a = <-addrs:
	case err := <-done:
		t.Fatalf("exited early: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("no listeners")
	}
	admin, mod := "http://"+a[0].String(), "http://"+a[1].String()
	for _, base := range []string{admin, mod} {
		req, _ := http.NewRequest("GET", base+"/readyz", nil)
		code, b := body(t, req)
		if code != 200 || !strings.Contains(b, `"database"`) || !strings.Contains(b, `"migrations"`) {
			t.Fatalf("%s/readyz: %d %s", base, code, b)
		}
	}
	// /metrics must never be on the public/ingress-facing listeners
	for _, base := range []string{admin, mod} {
		req, _ := http.NewRequest("GET", base+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer tok-123")
		if code, b := body(t, req); code == 200 || strings.Contains(b, "can_ram_rss_bytes") {
			t.Fatalf("%s serves metrics", base)
		}
	}
	req, _ := http.NewRequest("GET", "http://"+metrics+"/metrics", nil)
	if code, _ := body(t, req); code != 401 {
		t.Fatalf("unauthenticated scrape: %d", code)
	}
	req.Header.Set("Authorization", "Bearer tok-123")
	code, b := body(t, req)
	if code != 200 {
		t.Fatalf("scrape: %d", code)
	}
	for _, want := range []string{
		`can_build_info{version="v9.9.9",commit="c0ffee"`, "can_ram_declared_bytes 50331648", "can_ram_rss_bytes ",
		"can_http_requests_total{", "can_db_connections_open", "can_approvals_pending", "can_audit_verify_ok",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("no graceful shutdown")
	}
}

func TestRunRefusesOpenMetricsListenerWithoutToken(t *testing.T) {
	env := map[string]string{"CAN_METRICS_ADDR": "0.0.0.0:0"}
	cfg := Config{AdminAddr: "127.0.0.1:0", ModuleAddr: "127.0.0.1:0", DBPath: filepath.Join(t.TempDir(), "state.db"), TokenReview: "fake", Env: func(k string) string { return env[k] }}
	if err := Run(context.Background(), cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		t.Fatal("started with an unauthenticated non-loopback metrics listener")
	}
}

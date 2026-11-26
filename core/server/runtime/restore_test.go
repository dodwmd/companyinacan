package runtime

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/backup/backuptest"
)

// In restore mode the listeners answer only the probes and the restore hooks' dump download.
func TestRestoringHandlers(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	code := func(h http.Handler, method, path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w.Code
	}
	admin, module := restoringHandler(ok, adminAllowed), restoringHandler(ok, moduleAllowed)
	for _, c := range []struct {
		h            http.Handler
		method, path string
		want         int
	}{
		{admin, "GET", "/healthz", 200}, {admin, "GET", "/readyz", 200}, {admin, "GET", "/", 503},
		{admin, "POST", "/api/v1/login", 503}, {admin, "GET", "/v1/dumps/a/b", 503},
		{module, "GET", "/v1/dumps/wiki/x", 200}, {module, "PUT", "/v1/dumps/wiki/x", 503},
		{module, "POST", "/v1/email", 503}, {module, "POST", "/v1/events", 503}, {module, "GET", "/healthz", 200},
	} {
		if got := code(c.h, c.method, c.path); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

// A can-core started with CAN_RESTORE imports the state dump, serves only the probes and the
// dump download, and starts no worker.
func TestRunInRestoreMode(t *testing.T) {
	dir := t.TempDir()
	dumps := filepath.Join(dir, "dumps")
	if err := os.MkdirAll(filepath.Join(dumps, "can-core"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dumps, backup.StateDump), backuptest.StateDB(t), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"CAN_ADMIN_ADDR": "127.0.0.1:0", "CAN_MODULE_ADDR": "localhost:0", "CAN_DB_PATH": filepath.Join(dir, "state.db"),
		"CAN_TOKEN_REVIEW": "fake", "CAN_DUMPS_DIR": dumps, "CAN_RESTORE": "r1", "CAN_DUMP_START_DELAY_SECONDS": "0",
	}
	cfg, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	addr := make(chan [2]string, 1)
	cfg.OnListen = func(a, m net.Addr) { addr <- [2]string{a.String(), m.String()} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	var a [2]string
	select {
	case a = <-addr:
	case err := <-done:
		t.Fatal(err)
	}
	get := func(u string) int {
		var resp *http.Response
		var err error
		for i := 0; i < 100; i++ {
			if resp, err = http.Get(u); err == nil { //nolint:gosec,noctx // loopback
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("http://" + a[0] + "/healthz"); c != 200 {
		t.Error("healthz", c)
	}
	if c := get("http://" + a[0] + "/login"); c != 503 {
		t.Error("login page during a restore:", c)
	}
	if c := get("http://" + a[1] + "/v1/self"); c != 503 {
		t.Error("module API during a restore:", c)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dumps, backup.MarkerName)); err == nil {
		t.Error("a dump run happened in restore mode")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

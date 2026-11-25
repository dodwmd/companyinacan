package moduleapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

type dumpHarness struct {
	srv *httptest.Server
	d   *dumps.Dumper
}

func newDumpHarness(t *testing.T, wired bool) *dumpHarness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tr := tokenreview.NewFake()
	reg := registry.New(db, "http://can-core")
	for _, id := range []string{"wiki", "support"} {
		m := config.Module{ID: id, Name: id, Version: "1"}
		if err := reg.Register(ctx, store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s", Manifest: m}); err != nil {
			t.Fatal(err)
		}
		tr.Add("tok-"+id, "mod-"+id, id)
	}
	h := &dumpHarness{d: &dumps.Dumper{Store: &dumps.Store{Dir: filepath.Join(t.TempDir(), "dumps")}, Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}}
	s := &Server{Auth: modauth.New(tr, db), Registry: reg, Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	if wired {
		s.Dumps = h.d
	}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *dumpHarness) do(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// publish puts a file where a finished run would have.
func publish(t *testing.T, h *dumpHarness, module, name, body string) {
	t.Helper()
	dir := filepath.Join(h.d.Store.Dir, "modules", module)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustBegin(t *testing.T, h *dumpHarness, module string) {
	t.Helper()
	if err := h.d.BeginForTest(module); err != nil {
		t.Fatal(err)
	}
}

func TestDumpAPIPutAndGetOwnDump(t *testing.T) {
	h := newDumpHarness(t, true)
	mustBegin(t, h, "wiki")
	if st, b := h.do(t, "PUT", "/v1/dumps/wiki/storage.tar.gz", "tok-wiki", "archive"); st != http.StatusNoContent {
		t.Fatalf("PUT: %d %s", st, b)
	}
	publish(t, h, "wiki", "storage.tar.gz", "archive")
	// QA 11: outside restore mode a module cannot read dumps back at all.
	if st, _ := h.do(t, "GET", "/v1/dumps/wiki/storage.tar.gz", "tok-wiki", ""); st != http.StatusConflict {
		t.Fatalf("GET outside restore mode: %d, want 409", st)
	}
	h.d.RestoreMode = true
	if st, b := h.do(t, "GET", "/v1/dumps/wiki/storage.tar.gz", "tok-wiki", ""); st != http.StatusOK || b != "archive" {
		t.Fatalf("GET: %d %q", st, b)
	}
	if st, _ := h.do(t, "GET", "/v1/dumps/wiki/missing", "tok-wiki", ""); st != http.StatusNotFound {
		t.Fatalf("missing dump: %d", st)
	}
}

func TestDumpAPIOnlyTheModuleWhoseHookRunsMayUpload(t *testing.T) {
	h := newDumpHarness(t, true)
	mustBegin(t, h, "wiki")
	if st, _ := h.do(t, "PUT", "/v1/dumps/support/x", "tok-support", "a"); st != http.StatusConflict {
		t.Fatalf("got %d, want 409", st)
	}
}

func TestDumpAPIModuleCannotTouchAnotherModulesDumps(t *testing.T) {
	h := newDumpHarness(t, true)
	mustBegin(t, h, "wiki")
	if st, _ := h.do(t, "PUT", "/v1/dumps/wiki/x", "tok-wiki", "mine"); st != http.StatusNoContent {
		t.Fatal(st)
	}
	for _, m := range []string{"PUT", "GET"} {
		if st, _ := h.do(t, m, "/v1/dumps/wiki/x", "tok-support", "theirs"); st != http.StatusForbidden {
			t.Errorf("%s of another module's dump: %d, want 403", m, st)
		}
	}
}

func TestDumpAPIRequiresAToken(t *testing.T) {
	h := newDumpHarness(t, true)
	if st, _ := h.do(t, "PUT", "/v1/dumps/wiki/x", "", "a"); st != http.StatusUnauthorized {
		t.Fatalf("got %d", st)
	}
}

func TestDumpAPIUploadOutsideARunIsConflict(t *testing.T) {
	h := newDumpHarness(t, true)
	if st, _ := h.do(t, "PUT", "/v1/dumps/wiki/x", "tok-wiki", "a"); st != http.StatusConflict {
		t.Fatalf("got %d, want 409", st)
	}
}

func TestDumpAPIRejectsBadNames(t *testing.T) {
	h := newDumpHarness(t, true)
	mustBegin(t, h, "wiki")
	if st, _ := h.do(t, "PUT", "/v1/dumps/wiki/COMPLETE", "tok-wiki", "a"); st != http.StatusBadRequest {
		t.Fatalf("overwriting the marker name must be refused, got %d", st)
	}
}

func TestDumpAPIUnwiredIs503(t *testing.T) {
	h := newDumpHarness(t, false)
	if st, _ := h.do(t, "GET", "/v1/dumps/wiki/x", "tok-wiki", ""); st != http.StatusServiceUnavailable {
		t.Fatalf("got %d", st)
	}
}

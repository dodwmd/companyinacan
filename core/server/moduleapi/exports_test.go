package moduleapi

// `can export` end to end without a cluster (specs/071-company-export): can-core's export, the
// module API, and the real `can-forms export` in place of its Job.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/companyexport"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

type jobFunc func() error

func (f jobFunc) RunExport(context.Context, string, bool) error { return f() }

func TestExportThroughTheModuleAPI(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	state := filepath.Join(dir, "state.db")
	db, err := store.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	reg := registry.New(db, "http://can-core")
	tr := tokenreview.NewFake()
	for _, id := range []string{"forms", "work"} {
		if err := reg.Register(ctx, store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s",
			Manifest: config.Module{ID: id, Name: id, Version: "1"}}); err != nil {
			t.Fatal(err)
		}
		tr.Add("tok-"+id, "mod-"+id, id)
	}

	// The forms module's own database, with one submission.
	formsDB := filepath.Join(dir, "forms.db")
	fs, err := forms.OpenStore(formsDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fs.Close() }()
	if err := fs.Insert(ctx, &forms.Lead{FormID: "contact", Email: "ann@example.com", RetentionUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "forms.yaml")
	token := filepath.Join(dir, "token")
	if err := errors.Join(os.WriteFile(cfg, []byte("db_path: "+formsDB+"\n"), 0o600), os.WriteFile(token, []byte("tok-forms"), 0o600)); err != nil {
		t.Fatal(err)
	}

	svc := &companyexport.Service{DBPath: state, Company: "Acme", Audit: audit.New(db),
		Modules: func(ctx context.Context) ([]store.Module, error) { return db.ListModules(ctx) }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer((&Server{Auth: modauth.New(tr, db), Registry: reg, Exports: svc, Log: svc.Log}).Handler())
	defer srv.Close()

	h := &dumpHarness{srv: srv}
	if code, _ := h.do(t, "PUT", "/v1/exports/forms", "tok-forms", "x"); code != 409 {
		t.Fatal("accepted outside an export", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/exports/forms", "", "x"); code != 401 {
		t.Fatal("no token", code)
	}
	if code, _ := h.do(t, "PUT", "/v1/exports/forms", "tok-work", "x"); code != 403 {
		t.Fatal("another module's export", code)
	}

	// work has no export in this test; forms runs its real subcommand against the API.
	svc.Hooks = jobFunc(func() error {
		var log bytes.Buffer
		env := map[string]string{"CAN_API_URL": srv.URL, "CAN_MODULE": "forms", "CAN_TOKEN_FILE": token}
		if code := modbackup.Main("can-forms", forms.Migrations(), []string{"export", "-config", cfg}, func(k string) string { return env[k] }, &log, &log); code != 0 {
			return errors.New(log.String())
		}
		return nil
	})
	req, err := svc.Prepare(ctx, companyexport.Options{Only: []string{"forms"}, Actor: "root"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	m, err := req.Write(ctx, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Modules) != 1 || m.Modules[0].Export == nil || m.Modules[0].Export.Tables["leads"] != 1 {
		t.Fatalf("%+v", m)
	}
	zr, err := gzip.NewReader(&out)
	if err != nil {
		t.Fatal(err)
	}
	tarr := tar.NewReader(zr)
	leads := ""
	for {
		hd, err := tarr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hd.Name == "modules/forms/records/leads.jsonl" {
			b, err := io.ReadAll(tarr)
			if err != nil {
				t.Fatal(err)
			}
			leads = string(b)
		}
	}
	if !strings.Contains(leads, `"email":"ann@example.com"`) {
		t.Fatalf("leads: %q", leads)
	}
}

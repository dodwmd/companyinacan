package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/restoreops"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// restoringHeader marks the answers of a can-core process that is in restore mode.
const restoringHeader = "X-Can-Restoring"

// Serving asks the can-core running in this pod how it behaves: "restoring" or "normal". The
// latch on disk says what the next start will do; only the process says what it does now.
func (c Config) Serving(ctx context.Context) (string, error) {
	_, port, err := net.SplitHostPort(c.AdminAddr)
	if err != nil {
		return "", fmt.Errorf("CAN_ADMIN_ADDR: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("can-core does not answer: %w", err)
	}
	_ = resp.Body.Close()
	switch {
	case resp.Header.Get(restoringHeader) != "":
		return "restoring", nil
	case resp.StatusCode >= http.StatusInternalServerError:
		return "", fmt.Errorf("can-core answers %d", resp.StatusCode)
	}
	return "normal", nil
}

// restoringHandler answers 503 to everything but what allowed admits.
func restoringHandler(next http.Handler, allowed func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowed(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "60")
		w.Header().Set(restoringHeader, "1")
		http.Error(w, "this server is being restored from a backup", http.StatusServiceUnavailable)
	})
}

func probe(r *http.Request) bool { return r.URL.Path == "/healthz" || r.URL.Path == "/readyz" }

// adminAllowed: probes only. No logins, no changes while the restore runs.
func adminAllowed(r *http.Request) bool { return r.Method == http.MethodGet && probe(r) }

// moduleAllowed: probes and the dump download of the restore hooks.
func moduleAllowed(r *http.Request) bool {
	return r.Method == http.MethodGet && (probe(r) || strings.HasPrefix(r.URL.Path, "/v1/dumps/"))
}

// serveRestoring is can-core in restore mode: the two listeners, guarded, and nothing else.
func serveRestoring(ctx context.Context, timeout time.Duration, log *slog.Logger, admin *http.Server, adminLn net.Listener, module *http.Server, moduleLn net.Listener) error {
	log.Warn("RESTORE MODE: no email, webhooks, dump runs, module reconcile or admin UI until `can restore` completes")
	errc := make(chan error, 2)
	for _, s := range []struct {
		srv *http.Server
		ln  net.Listener
	}{{admin, adminLn}, {module, moduleLn}} {
		go func() {
			if err := s.srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return errors.Join(runErr, admin.Shutdown(sctx), module.Shutdown(sctx))
}

// RestoreOps builds the operations `can restore` runs in the can-core pod (`can-core restore ...`).
// The returned function closes the database.
func (c Config) RestoreOps(ctx context.Context, log *slog.Logger) (*restoreops.Ops, func(), error) {
	if c.Installer != "kubernetes" {
		return nil, nil, errors.New("restore needs CAN_INSTALLER=kubernetes")
	}
	if c.DumpsDir == "" {
		return nil, nil, errors.New("restore needs CAN_DUMPS_DIR")
	}
	api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(ctx, c.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening state database: %w", err)
	}
	o := &restoreops.Ops{
		DB: db, Audit: audit.New(db), DBPath: c.DBPath, DumpsDir: c.DumpsDir, ModulesDir: c.ModulesDir,
		API: api, Hooks: c.kubeHooks(api, db), PostgresURL: c.PostgresAdminURL,
		Installer: func(seed func(context.Context, reconcile.Spec) error, pg string) reconcile.ModuleInstaller {
			// As newInstaller: the operator's server when configured (or exported), otherwise the
			// shared Postgres can-core runs, which starts empty and is filled by the restore hooks.
			var prov dbprov.Provisioner = &dbprov.Managed{API: api}
			if pg != "" {
				prov = &dbprov.External{API: api, Inner: &dbprov.Postgres{AdminURL: pg, Host: c.PostgresHost, Port: 5432}}
			}
			return installer.New(installer.Config{API: api, ModulesDir: c.ModulesDir, PlatformURL: c.PlatformURL, NodeIP: c.NodeIP,
				Domain: c.CompanyDomain, RequireAdmission: true, DB: prov, Log: log, Quiesce: true, Seed: seed})
		},
	}
	return o, func() { _ = db.Close() }, nil
}

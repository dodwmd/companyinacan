package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// newInstaller builds the module installer selected by CAN_INSTALLER.
func (c Config) newInstaller(log *slog.Logger) (reconcile.ModuleInstaller, error) {
	if c.ModuleInstaller != nil {
		return c.ModuleInstaller, nil
	}
	if c.DevStack {
		return recordOnly{log}, nil
	}
	if c.Installer != "kubernetes" {
		return nil, nil
	}
	api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
	if err != nil {
		return nil, err
	}
	// An external Postgres when the operator configured one; otherwise can-core runs its own in can-data,
	// from the first module that needs it (specs/041-wrapped-modules-live).
	var prov dbprov.Provisioner = &dbprov.Managed{API: api}
	if c.PostgresAdminURL != "" {
		prov = &dbprov.External{API: api, Inner: &dbprov.Postgres{AdminURL: c.PostgresAdminURL, Host: c.PostgresHost, Port: 5432}}
		// Recorded at start too, not only at the next module apply, so that dropping the setting later
		// is refused even if nothing was applied in between.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := dbprov.MarkExternal(ctx, api); err != nil {
			log.Warn("postgres: could not record the external server; it is recorded at the next module apply", "error", err)
		}
	}
	return installer.New(installer.Config{API: api, ModulesDir: c.ModulesDir, PlatformURL: c.PlatformURL, NodeIP: c.NodeIP, Domain: c.CompanyDomain, RequireAdmission: true, DB: prov, Relay: c.appRelay, IMAP: c.appIMAP, Log: log}), nil
}

// reconcileOnce applies the company.yaml at cfg.CompanyFile (in a cluster: the mounted ConfigMap
// maintained by `can apply`). A file that is not there yet is not an error; an invalid one is
// refused whole, and nothing is applied.
func reconcileOnce(ctx context.Context, cfg Config, r *reconcile.Reconciler, log *slog.Logger) error {
	if _, err := os.Stat(cfg.CompanyFile); errors.Is(err, os.ErrNotExist) {
		log.Debug("no company.yaml to reconcile yet", "path", cfg.CompanyFile)
		return nil
	}
	desired, ps := reconcile.LoadDesired(cfg.CompanyFile, cfg.ModulesDir)
	if problem.HasErrors(ps) {
		for _, p := range ps {
			if p.Severity == problem.Error {
				log.Error("company.yaml is invalid; nothing applied", "rule", p.Rule, "path", p.Path, "message", p.Message)
			}
		}
		return fmt.Errorf("company.yaml is invalid (%d problems)", len(ps))
	}
	res, err := r.Apply(ctx, desired, reconcile.Options{})
	for _, a := range res.Actions {
		log.Info("reconciled", "action", a.Kind, "module", a.Module, "reason", a.Reason, "failed", contains(res.Failed, a.Module))
	}
	return err
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// reconcileLoop reconciles at start and then every cfg.ReconcileInterval until ctx ends. The
// kubelet refreshes a mounted ConfigMap within about a minute, so the loop, not a file watcher,
// picks up `can apply` changes; failed modules are retried on the same cadence.
func reconcileLoop(ctx context.Context, cfg Config, r *reconcile.Reconciler, log *slog.Logger) {
	loopEvery(ctx, cfg, log, func(ctx context.Context) error { return reconcileOnce(ctx, cfg, r, log) })
}

// loopEvery runs tick at start and then every cfg.ReconcileInterval until ctx ends.
func loopEvery(ctx context.Context, cfg Config, log *slog.Logger, tick func(context.Context) error) {
	interval := cfg.ReconcileInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for {
		if err := tick(ctx); err != nil && ctx.Err() == nil {
			log.Error("reconcile failed; will retry", "error", err, "in", interval.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// reconcileStored applies the company.yaml held by the company store (the in-cluster ConfigMap).
// It reads the store, not the mounted file: the kubelet refreshes a mounted ConfigMap only after up to
// a minute, so a change just made in the admin UI would otherwise be reverted by the next tick. The
// runner serialises it with applies started from the UI, and the outcome is recorded for the status page.
func reconcileStored(ctx context.Context, cfg Config, cs modmgr.CompanyStore, run *modmgr.Runner, svc *modmgr.Service, log *slog.Logger) error {
	snap, err := cs.Get(ctx)
	if errors.Is(err, modmgr.ErrNoCompany) {
		log.Debug("no company.yaml to reconcile yet")
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the company store: %w", err)
	}
	desired, ps := reconcile.LoadDesiredBytes("company.yaml", snap.Data, cfg.ModulesDir)
	if problem.HasErrors(ps) {
		for _, p := range ps {
			if p.Severity == problem.Error {
				log.Error("company.yaml is invalid; nothing applied", "rule", p.Rule, "path", p.Path, "message", p.Message)
			}
		}
		return fmt.Errorf("company.yaml is invalid (%d problems)", len(ps))
	}
	res, err := run.Apply(ctx, desired)
	for _, a := range res.Actions {
		log.Info("reconciled", "action", a.Kind, "module", a.Module, "reason", a.Reason, "failed", contains(res.Failed, a.Module))
	}
	svc.RecordReconcile(ctx, res, err)
	return err
}

func newReconciler(db *store.DB, reg *registry.Registry, al *audit.Log, inst reconcile.ModuleInstaller) *reconcile.Reconciler {
	r := reconcile.New(db, reg, al, inst)
	r.Actor = "can-core"
	// Nobody confirms what the loop applies after a can-core upgrade: an upgrade that widens what a
	// module may do waits for approval, and one that failed is not retried at once.
	r.RequireApproval, r.Backoff = true, true
	return r
}

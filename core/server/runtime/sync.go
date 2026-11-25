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
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// newInstaller builds the module installer selected by CAN_INSTALLER.
func (c Config) newInstaller(log *slog.Logger) (reconcile.ModuleInstaller, error) {
	if c.ModuleInstaller != nil {
		return c.ModuleInstaller, nil
	}
	if c.Installer != "kubernetes" {
		return nil, nil
	}
	api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
	if err != nil {
		return nil, err
	}
	var prov dbprov.Provisioner = dbprov.Unconfigured{}
	if c.PostgresAdminURL != "" {
		prov = &dbprov.Postgres{AdminURL: c.PostgresAdminURL, Host: c.PostgresHost, Port: 5432}
	}
	return installer.New(installer.Config{API: api, ModulesDir: c.ModulesDir, PlatformURL: c.PlatformURL, NodeIP: c.NodeIP, Domain: c.CompanyDomain, RequireAdmission: true, DB: prov, Log: log}), nil
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
	interval := cfg.ReconcileInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for {
		if err := reconcileOnce(ctx, cfg, r, log); err != nil && ctx.Err() == nil {
			log.Error("reconcile failed; will retry", "error", err, "in", interval.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func newReconciler(db *store.DB, reg *registry.Registry, al *audit.Log, inst reconcile.ModuleInstaller) *reconcile.Reconciler {
	r := reconcile.New(db, reg, al, inst)
	r.Actor = "can-core"
	return r
}

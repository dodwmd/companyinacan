package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// PlanModules is what the can-core configured by getenv would do with company (company.yaml
// content), held steps included. It changes nothing.
func PlanModules(ctx context.Context, getenv func(string) string, db *store.DB, company []byte) ([]reconcile.Action, error) {
	cfg, err := FromEnv(getenv)
	if err != nil {
		return nil, err
	}
	cfg.PostgresAdminURL = "" // building the installer would otherwise record the external server
	inst, err := cfg.newInstaller(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, errors.New("this can-core installs no modules (CAN_INSTALLER)")
	}
	desired, ps := reconcile.LoadDesiredBytes("company.yaml", company, cfg.ModulesDir)
	if problem.HasErrors(ps) {
		for _, p := range ps {
			if p.Severity == problem.Error {
				return nil, fmt.Errorf("company.yaml: %s", p.Message)
			}
		}
	}
	r := newReconciler(db, registry.New(db, cfg.PlatformURL), audit.New(db), inst)
	return r.Plan(ctx, desired)
}

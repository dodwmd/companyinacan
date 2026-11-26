package modmgr

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// PendingUpgrade is an upgrade of a running module that waits for approval.
type PendingUpgrade struct {
	Module, Approval string
	Widens           []string
}

// UpgradeAck is the confirmation Apply takes as the approval of a held upgrade.
func UpgradeAck(module, approval string) string { return "upgrade:" + module + ":" + approval }

// PendingUpgrades lists the upgrades the reconciler holds.
func (s *Service) PendingUpgrades(ctx context.Context) ([]PendingUpgrade, error) {
	mods, err := s.DB.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	var out []PendingUpgrade
	for _, m := range mods {
		p, ok := reconcile.ParsePending(m.Pending)
		if !ok || m.State != store.StateEnabled {
			continue
		}
		u := PendingUpgrade{Module: m.ID, Approval: p.Approval}
		for _, w := range p.Widens {
			u.Widens = append(u.Widens, Sanitise(w))
		}
		out = append(out, u)
	}
	return out, nil
}

// ApproveUpgrade approves the held upgrade of a module; approval must be the one that was shown,
// so that a release that widens more in the meantime is not approved unseen. The periodic
// reconcile then runs it.
func (s *Service) ApproveUpgrade(ctx context.Context, actor, module, approval string) error {
	m, err := s.DB.GetModule(ctx, module)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	p, ok := reconcile.ParsePending(m.Pending)
	if !ok || p.Approval != approval {
		return fmt.Errorf("%w: the waiting upgrade of %s has changed or is gone", ErrStale, module)
	}
	return s.recordApproval(ctx, actor, module, approval, p.Widens)
}

func (s *Service) recordApproval(ctx context.Context, actor, module, approval string, widens []string) error {
	if _, err := s.audit(ctx, actor, "module.upgrade-approve", module, map[string]any{"approval": approval, "widens": widens}); err != nil {
		return err
	}
	return s.DB.ApproveUpgrade(ctx, module, approval)
}

// approveConfirmed records the approvals given with an apply for upgrades its preview holds.
func (s *Service) approveConfirmed(ctx context.Context, actor string, held []reconcile.Action, confirmed []string) error {
	for _, a := range held {
		if a.Held == reconcile.HeldApproval && slices.Contains(confirmed, UpgradeAck(a.Module, a.Approval)) {
			if err := s.recordApproval(ctx, actor, a.Module, a.Approval, a.Widens); err != nil {
				return err
			}
		}
	}
	return nil
}

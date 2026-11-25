package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/notify"
)

// platformTimers drives the approval timers (expiry, escalation) and email digests every minute,
// and prunes old notifications hourly. Each pass does bounded work.
func platformTimers(ctx context.Context, eng *approvals.Engine, ntf *notify.Service, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := eng.Tick(ctx); err != nil {
				log.Error("approval timers failed", "error", err)
			}
			if _, err := ntf.FlushDigests(ctx); err != nil {
				log.Error("notification digests failed", "error", err)
			}
			if time.Since(last) >= time.Hour {
				last = time.Now()
				if _, err := ntf.Prune(ctx, 180*24*time.Hour); err != nil {
					log.Error("notification pruning failed", "error", err)
				}
			}
		}
	}
}

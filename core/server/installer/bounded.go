package installer

import (
	"context"
	"fmt"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// renderBounded renders one module's chart in the child process, at most one render per module at a
// time. The child is killed on timeout or cancellation, so nothing is left running after a failure.
func (in *Installer) renderBounded(ctx context.Context, s reconcile.Spec) ([]kube.Object, error) {
	in.mu.Lock()
	if in.rendering[s.ID] {
		in.mu.Unlock()
		return nil, fmt.Errorf("a previous render of %s is still running; not starting another", s.ID)
	}
	in.rendering[s.ID] = true
	in.mu.Unlock()
	defer func() {
		in.mu.Lock()
		delete(in.rendering, s.ID)
		in.mu.Unlock()
	}()
	return in.render(ctx, s)
}

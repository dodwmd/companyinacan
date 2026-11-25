package installer

import (
	"context"
	"fmt"
	"time"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// overallRenderTimeout bounds one module's whole render (values mapping plus chart) as a hard
// backstop to the renderer's own iteration budget and deadline.
const overallRenderTimeout = 30 * time.Second

// renderBounded renders in a goroutine under a timeout so a stuck template can never hold up the
// reconcile loop. The iteration budget (ticks.go) also stops the abandoned goroutine.
func (in *Installer) renderBounded(ctx context.Context, s reconcile.Spec) ([]kube.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, overallRenderTimeout)
	defer cancel()
	type result struct {
		objs []kube.Object
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{nil, fmt.Errorf("rendering panicked: %v", r)}
			}
		}()
		objs, err := in.render(ctx, s)
		ch <- result{objs, err}
	}()
	select {
	case r := <-ch:
		return r.objs, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("rendering %s: %w", s.ID, ctx.Err())
	}
}

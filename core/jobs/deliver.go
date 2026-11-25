package jobs

import "context"

// DeliverEvents delivers the due outbox events once (the dispatcher otherwise runs inside Serve).
// Tests and tools use it to drive delivery by hand.
func (a *App) DeliverEvents(ctx context.Context) (int, error) {
	if a.dispatcher == nil {
		return 0, nil
	}
	return a.dispatcher.DeliverDue(ctx)
}

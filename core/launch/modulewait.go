package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// can-core installs the enabled modules after company.yaml reaches it, which is after the phases of
// `can up` are done (the kubelet delivers a changed ConfigMap in its own time). WaitModules lets
// `can up` say when they are usable instead of returning before they are.

// stillEvery is how often a wait that is making no progress says so.
const stillEvery = 30 * time.Second

// ModuleWait configures WaitModules.
type ModuleWait struct {
	Timeout time.Duration
	// Sleep and Now are seams for tests; nil means the real ones.
	Sleep func(context.Context, time.Duration)
	Now   func() time.Time
	// Say receives the progress lines.
	Say func(format string, a ...any)
}

// WaitModules waits, at most w.Timeout, for the Deployments of each module (namespace mod-<id>) to
// be rolled out. It returns the modules that are not, sorted, and the last error asking about
// them. A cancelled ctx ends the wait. It only reads.
func WaitModules(ctx context.Context, kube Kube, ids []string, w ModuleWait) (notReady []string, lastErr error) {
	if w.Sleep == nil {
		w.Sleep = func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	pending := slices.Clone(ids)
	slices.Sort(pending)
	start := w.Now()
	// The number of rounds is bounded as well as the time, so a fake clock cannot loop forever.
	rounds := int(w.Timeout/pollEvery) + 1
	said := time.Duration(0)
	for i := 0; i < rounds && len(pending) > 0; i++ {
		var left []string
		for _, id := range pending {
			// A hanging API call must not outlast the budget.
			qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := moduleRolledOut(qctx, kube, id)
			cancel()
			if err != nil {
				left, lastErr = append(left, id), fmt.Errorf("module %s: %w", id, err)
			} else if i > 0 {
				w.Say("  %s is ready after %s", id, w.Now().Sub(start).Round(time.Second))
			}
		}
		pending = left
		elapsed := w.Now().Sub(start)
		if len(pending) == 0 || ctx.Err() != nil || elapsed >= w.Timeout || i == rounds-1 {
			break
		}
		switch {
		case i == 0:
			w.Say("waiting for can-core to install: %s (at most %s; Ctrl-C skips the wait)", strings.Join(pending, ", "), w.Timeout)
		case elapsed-said >= stillEvery:
			said = elapsed
			w.Say("  still waiting for %s (%s)", strings.Join(pending, ", "), elapsed.Round(time.Second))
		}
		w.Sleep(ctx, pollEvery)
		if ctx.Err() != nil {
			break
		}
	}
	if len(pending) == 0 {
		return nil, nil
	}
	return pending, lastErr
}

// moduleRolledOut is nil when the module's own Deployment exists and it and its companions (an
// adapter) have every wanted replica updated and available, as `kubectl rollout status` has it.
func moduleRolledOut(ctx context.Context, kube Kube, id string) error {
	out, err := kube.Run(ctx, nil, "-n", "mod-"+id, "get", "deploy", "-o", "json")
	if err != nil {
		return err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name       string `json:"name"`
				Generation int64  `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				Replicas           int   `json:"replicas"`
				UpdatedReplicas    int   `json:"updatedReplicas"`
				AvailableReplicas  int   `json:"availableReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return errors.New("the list of deployments is not readable")
	}
	found := false
	for _, d := range list.Items {
		want := 1
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if d.Metadata.Name == id {
			found = want > 0
		}
		s := d.Status
		if s.ObservedGeneration < d.Metadata.Generation || s.UpdatedReplicas < want || s.AvailableReplicas < want || s.Replicas > s.UpdatedReplicas {
			return fmt.Errorf("deployment %s is not rolled out", d.Metadata.Name)
		}
	}
	if !found {
		return errors.New("not installed yet")
	}
	return nil
}

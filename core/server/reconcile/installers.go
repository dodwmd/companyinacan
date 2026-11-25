package reconcile

import (
	"context"
	"sync"
)

// RecordOnly is an installer that touches nothing: `can apply --record-only` uses it to update
// can-core's recorded state without a cluster. It exists for local development and tests of the
// reconciler wiring, not as a deployment path.
type RecordOnly struct{}

// Enable implements ModuleInstaller.
func (RecordOnly) Enable(context.Context, Spec) error { return nil }

// Disable implements ModuleInstaller.
func (RecordOnly) Disable(context.Context, string) error { return nil }

// FakeInstaller is an in-memory ModuleInstaller that records what it was asked to do.
type FakeInstaller struct {
	mu          sync.Mutex
	Enabled     map[string]Spec // currently running modules, with the config they were given
	Calls       []string        // "enable:<id>" / "disable:<id>" in order
	FailEnable  map[string]error
	FailDisable map[string]error
}

// NewFakeInstaller returns an empty fake.
func NewFakeInstaller() *FakeInstaller {
	return &FakeInstaller{Enabled: map[string]Spec{}, FailEnable: map[string]error{}, FailDisable: map[string]error{}}
}

// Enable implements ModuleInstaller.
func (f *FakeInstaller) Enable(_ context.Context, s Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "enable:"+s.ID)
	if err := f.FailEnable[s.ID]; err != nil {
		return err
	}
	f.Enabled[s.ID] = s
	return nil
}

// Disable implements ModuleInstaller.
func (f *FakeInstaller) Disable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "disable:"+id)
	if err := f.FailDisable[id]; err != nil {
		return err
	}
	delete(f.Enabled, id)
	return nil
}

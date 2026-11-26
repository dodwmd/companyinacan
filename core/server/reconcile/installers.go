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

// Restrict implements Restricter.
func (RecordOnly) Restrict(context.Context, Spec) error { return nil }

// FakeInstaller is an in-memory ModuleInstaller that records what it was asked to do.
type FakeInstaller struct {
	mu          sync.Mutex
	Enabled     map[string]Spec // currently running modules, with the config they were given
	Calls       []string        // "enable:<id>" / "disable:<id>" in order
	FailEnable  map[string]error
	FailDisable map[string]error
	Restricted  map[string]Spec // what Restrict was last given
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

// Restrict implements Restricter.
func (f *FakeInstaller) Restrict(_ context.Context, s Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "restrict:"+s.ID)
	if err := f.FailEnable[s.ID]; err != nil {
		return err
	}
	if f.Restricted == nil {
		f.Restricted = map[string]Spec{}
	}
	f.Restricted[s.ID] = s
	return nil
}

// EnabledCount returns how many modules are currently enabled (safe to call while a reconcile runs).
func (f *FakeInstaller) EnabledCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Enabled)
}

// EnabledSpec returns the Spec an enabled module was given.
func (f *FakeInstaller) EnabledSpec(id string) (Spec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.Enabled[id]
	return s, ok
}

// CallLog returns a copy of the recorded calls.
func (f *FakeInstaller) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// fakeRunner is a Runner that never touches the network. Run matches the
// script against rules (first substring match wins).
type fakeRunner struct {
	mu      sync.Mutex
	rules   []fakeRule
	Ran     []string
	Copied  map[string][]byte
	Modes   map[string]string
	CopyErr error
}

type fakeRule struct {
	contains string
	out      string
	err      error
}

func newFakeRunner(rules ...fakeRule) *fakeRunner {
	return &fakeRunner{rules: rules, Copied: map[string][]byte{}, Modes: map[string]string{}}
}

func (f *fakeRunner) Run(_ context.Context, script string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Ran = append(f.Ran, script)
	for _, r := range f.rules {
		if strings.Contains(script, r.contains) {
			return r.out, r.err
		}
	}
	return "", errors.New("fakeRunner: no rule for script: " + firstLine(script))
}

func (f *fakeRunner) Copy(_ context.Context, path string, data []byte, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CopyErr != nil {
		return f.CopyErr
	}
	f.Copied[path] = data
	f.Modes[path] = mode
	return nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

const probeGood = "uid=0\nbootstrapped=\nos_id=ubuntu\nos_version=24.04\narch=x86_64\nmem_mb=1950\ndisk_free_mb=30000\nsystemd=yes\nport_80=free\nport_443=free\nport_6443=free\n"

const probeBootstrapped = "uid=0\nbootstrapped=" + DefaultK3sVersion + "\nos_id=ubuntu\nos_version=24.04\narch=x86_64\nmem_mb=1950\ndisk_free_mb=30000\nsystemd=yes\nport_80=busy\nport_443=busy\nport_6443=busy\n"

const statusReady = "bootstrapped=" + DefaultK3sVersion + "\nk3s_active=active\nnodes_ready=1\n"

func noSleep(context.Context, time.Duration) {}

func testDeps(r Runner) Deps {
	d := Deps{Runner: func(Target) Runner { return r }, Sleep: noSleep, Getenv: func(string) string { return "" }}
	d.defaults()
	return d
}

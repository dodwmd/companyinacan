package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RestoreCall records one FakeRestic.Restore invocation.
type RestoreCall struct {
	Snapshot, Target string
	Includes         []string
}

// FakeRestic is an in-memory ResticAPI for tests (here and in package cli).
// Files maps absolute host paths to content; Restore materialises the ones
// under the requested includes below the target, like `restic restore`.
type FakeRestic struct {
	Snaps       []Snapshot
	Files       map[string][]byte
	Corrupt     map[string][]byte // returned instead of Files content
	CheckErr    error
	RestoreErr  error
	CheckCalled bool
	Restores    []RestoreCall
}

var _ ResticAPI = (*FakeRestic)(nil)

// Snapshots implements ResticAPI.
func (f *FakeRestic) Snapshots(context.Context) ([]Snapshot, error) {
	s := append([]Snapshot(nil), f.Snaps...)
	sort.Slice(s, func(i, j int) bool { return s[i].Time.After(s[j].Time) })
	return s, nil
}

// Check implements ResticAPI.
func (f *FakeRestic) Check(context.Context, bool) error {
	f.CheckCalled = true
	return f.CheckErr
}

// Restore implements ResticAPI.
func (f *FakeRestic) Restore(_ context.Context, snapshot, target string, includes []string) error {
	f.Restores = append(f.Restores, RestoreCall{snapshot, target, includes})
	if f.RestoreErr != nil {
		return f.RestoreErr
	}
	if snapshot != "latest" {
		found := false
		for _, s := range f.Snaps {
			found = found || s.ID == snapshot
		}
		if !found {
			return fmt.Errorf("fake restic: no snapshot %q", snapshot)
		}
	}
	for p, c := range f.Files {
		if len(includes) > 0 && !underAny(p, includes) {
			continue
		}
		if cc, ok := f.Corrupt[p]; ok {
			c = cc
		}
		dst := filepath.Join(target, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, c, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, strings.TrimRight(r, "/")+"/") {
			return true
		}
	}
	return false
}

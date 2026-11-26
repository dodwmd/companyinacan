package dumps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// E08b QA3 2: when even the rollback fails, the staging directory holding the previous set
// survives, and the next run puts it back.
func TestFailedRollbackKeepsThePreviousSet(t *testing.T) {
	d, _ := newDumper(t, []string{"wiki"}, &fakeHooks{})
	d.Hooks = hookFunc(func(_ context.Context, m string) error { return upload(d, m, "a", "v1") })
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.rename = func(from, to string) error {
		if filepath.Base(to) == "modules" { // neither the new set nor the old one can be moved in
			return errors.New("disk error")
		}
		return os.Rename(from, to)
	}
	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	olds, _ := filepath.Glob(filepath.Join(d.Store.Dir, ".run-*", "old", "wiki", "a"))
	if len(olds) != 1 {
		t.Fatalf("the previous set was deleted: %v", olds)
	}
	// still broken: the next run must fail without cleaning the only copy away
	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if olds, _ = filepath.Glob(filepath.Join(d.Store.Dir, ".run-*", "old", "wiki", "a")); len(olds) != 1 {
		t.Fatalf("the previous set was deleted by the next run: %v", olds)
	}
	d.rename = nil
	d.Hooks = hookFunc(func(context.Context, string) error { return errors.New("boom") })
	_, _ = d.Run(context.Background())
	if got := read(t, filepath.Join(d.Store.Dir, "modules", "wiki", "a")); got != "v1" {
		t.Fatalf("%q", got)
	}
}

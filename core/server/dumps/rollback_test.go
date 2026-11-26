package dumps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
)

// E08b QA3 3: the documented way to clear a "no data" module: remove its carried dump.
func TestRemovingACarriedDumpClearsTheReport(t *testing.T) {
	gone := false
	d, _ := newDumper(t, []string{"wiki"}, &fakeHooks{})
	d.Hooks = hookFunc(func(_ context.Context, m string) error {
		if gone {
			return ErrNoData
		}
		return upload(d, m, "a", "v")
	})
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	gone = true
	if err := os.RemoveAll(filepath.Join(d.Store.Dir, "modules", "wiki")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := backup.ReadDumpRun(d.Store.Dir)
	if err != nil || len(r.Skipped) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

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

// E08b QA3 4: skips that lose data are listed for the host script to announce.
func TestSkippedDatabaseIsListedForTheHostScript(t *testing.T) {
	d, _ := newDumper(t, []string{"crm", "wiki"}, &fakeHooks{})
	d.Disabled = func(context.Context, string) bool { return true }
	d.Unrunnable = func(context.Context, string) string { return "no image" }
	d.Database = func(_ context.Context, m string) string {
		return map[string]string{"crm": "mariadb", "wiki": "none"}[m]
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(d.Store.Dir, backup.AlertsName))
	if !strings.Contains(got, `"crm"`) || strings.Contains(got, `"wiki"`) || strings.Count(got, "\n") != 1 {
		t.Fatalf("%q", got)
	}
	d.Disabled = func(context.Context, string) bool { return false }
	_, _ = d.Run(context.Background())
	if exists(filepath.Join(d.Store.Dir, backup.AlertsName)) {
		t.Fatal("stale alerts")
	}
}

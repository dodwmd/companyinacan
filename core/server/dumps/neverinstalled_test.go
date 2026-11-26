package dumps

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
)

// E06d: a first install that failed (its image cannot be pulled) leaves a volume whose hook Job
// cannot start. It must not stop every other module's backup.
func TestModuleWhoseFirstInstallFailedDoesNotStopBackups(t *testing.T) {
	for name, unrunnable := range map[string]string{"hook fails": "", "known unrunnable": "it needs the can-hooks image"} {
		t.Run(name, func(t *testing.T) {
			d, _ := newDumper(t, []string{"forms", "work"}, &fakeHooks{})
			d.Hooks = hookFunc(func(_ context.Context, m string) error {
				if m == "work" {
					return errors.New("hook Job cannot start: ImagePullBackOff")
				}
				return upload(d, m, "a", "v")
			})
			d.NeverInstalled = func(_ context.Context, m string) bool { return m == "work" }
			d.Unrunnable = func(_ context.Context, m string) string {
				if m == "work" {
					return unrunnable
				}
				return ""
			}
			d.Database = func(context.Context, string) string { return "sqlite" }
			if _, err := d.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := backup.ReadMarker(filepath.Join(d.Store.Dir, COMPLETE)); err != nil {
				t.Fatalf("no completion marker: %v", err)
			}
			if got := read(t, filepath.Join(d.Store.Dir, DumpedList)); got != "forms sqlite\n" {
				t.Errorf("DUMPED = %q: a skipped module must keep its raw files", got)
			}
			r, err := backup.ReadDumpRun(d.Store.Dir)
			if err != nil || len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Message(), "first install never succeeded") {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

// A module that once had a dump keeps being mandatory, whatever its record says now.
func TestFailedModuleThatHadADumpStillFailsTheRun(t *testing.T) {
	d, _ := newDumper(t, []string{"work"}, &fakeHooks{})
	d.Hooks = hookFunc(func(_ context.Context, m string) error { return upload(d, m, "a", "v") })
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.Hooks = hookFunc(func(context.Context, string) error { return errors.New("boom") })
	d.NeverInstalled = func(context.Context, string) bool { return true }
	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("the run succeeded without the dump of a module that had one")
	}
}

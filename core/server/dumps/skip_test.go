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

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// QA2 1: disabling a module whose hook cannot run (or fails) gives backups back. It is skipped,
// not listed as dumped (so its raw files stay in the backup), and reported.
func TestDisabledModuleWithAHookThatCannotRunIsSkippedAndReported(t *testing.T) {
	for name, unrunnable := range map[string]string{"known unrunnable": "it needs the can-hooks image", "fails at run time": ""} {
		t.Run(name, func(t *testing.T) {
			d, _ := newDumper(t, []string{"crm", "forms", "wiki"}, &fakeHooks{})
			var ran []string
			d.Hooks = hookFunc(func(_ context.Context, m string) error {
				ran = append(ran, m)
				if m != "forms" {
					_ = upload(d, m, "partial", "half")
					return errors.New("ImagePullBackOff")
				}
				return upload(d, m, "a", "v")
			})
			d.Disabled = func(_ context.Context, m string) bool { return m != "forms" }
			d.Unrunnable = func(_ context.Context, m string) string {
				if m == "forms" {
					return ""
				}
				return unrunnable
			}
			d.Database = func(_ context.Context, m string) string {
				return map[string]string{"crm": "mariadb", "forms": "sqlite", "wiki": "none"}[m]
			}
			if _, err := d.Run(context.Background()); err != nil {
				t.Fatalf("a disabled module must not stop backups: %v", err)
			}
			if unrunnable != "" && strings.Join(ran, ",") != "forms" {
				t.Errorf("a hook known not to run was started: %v", ran)
			}
			if got := read(t, filepath.Join(d.Store.Dir, DumpedList)); got != "forms sqlite\n" {
				t.Errorf("DUMPED = %q: a skipped module must keep its raw files", got)
			}
			if exists(filepath.Join(d.Store.Dir, "modules", "wiki", "partial")) {
				t.Error("a failed hook's partial upload was published")
			}
			r, err := backup.ReadDumpRun(d.Store.Dir)
			if err != nil || !r.OK || len(r.Skipped) != 2 {
				t.Fatalf("%+v %v", r, err)
			}
			msgs := r.Skipped[0].Message() + "\n" + r.Skipped[1].Message()
			for _, want := range []string{`"crm"`, "DATABASE IS NOT BACKED UP", `"wiki"`, "raw volume files"} {
				if !strings.Contains(msgs, want) {
					t.Errorf("missing %q in %s", want, msgs)
				}
			}
		})
	}
}

// QA2 1: an enabled (or failed) module still fails the run.
func TestEnabledModuleWithAFailingHookStillFailsTheRun(t *testing.T) {
	d, _ := newDumper(t, []string{"wiki"}, &fakeHooks{})
	d.Hooks = hookFunc(func(context.Context, string) error { return errors.New("boom") })
	d.Disabled = func(context.Context, string) bool { return false }
	d.Unrunnable = func(context.Context, string) string { return "it needs the can-hooks image" }
	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
}

// QA2 5: a module that had a dump and now reports no data keeps its dump and is reported.
func TestDumpIsCarriedForwardWhenAModuleReportsNoData(t *testing.T) {
	gone := false
	d, _ := newDumper(t, []string{"wiki"}, &fakeHooks{})
	d.Hooks = hookFunc(func(_ context.Context, m string) error {
		if gone {
			return ErrNoData
		}
		return upload(d, m, "a", "precious")
	})
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	gone = true
	for range 2 {
		if _, err := d.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := read(t, filepath.Join(d.Store.Dir, "modules", "wiki", "a")); got != "precious" {
			t.Fatalf("the dump was lost: %q", got)
		}
		r, _ := backup.ReadDumpRun(d.Store.Dir)
		if len(r.Skipped) != 1 || !r.Skipped[0].Kept || !strings.Contains(r.Skipped[0].Message(), "earlier dump is kept") {
			t.Fatalf("not reported: %+v", r.Skipped)
		}
	}
}

// QA2 4: a crash between the two renames of the swap must not lose the previous set.
func TestInterruptedSwapIsRecovered(t *testing.T) {
	d, _ := newDumper(t, []string{"wiki"}, &fakeHooks{})
	old := filepath.Join(d.Store.Dir, ".run-crashed", "old", "wiki")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "a"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Hooks = hookFunc(func(context.Context, string) error { return errors.New("boom") })
	_, _ = d.Run(context.Background())
	if got := read(t, filepath.Join(d.Store.Dir, "modules", "wiki", "a")); got != "previous" {
		t.Fatalf("previous set lost: %q", got)
	}
}

// QA2 4: if the new set cannot be moved in, the previous one is put back.
func TestFailedSwapIsRolledBack(t *testing.T) {
	d, _ := newDumper(t, []string{"wiki"}, &fakeHooks{})
	d.Hooks = hookFunc(func(_ context.Context, m string) error { return upload(d, m, "a", "v1") })
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.rename = func(from, to string) error {
		if strings.HasSuffix(from, filepath.Join("modules")) && strings.Contains(from, ".run-") {
			return errors.New("disk error")
		}
		return os.Rename(from, to)
	}
	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if got := read(t, filepath.Join(d.Store.Dir, "modules", "wiki", "a")); got != "v1" {
		t.Fatalf("%q", got)
	}
	if exists(filepath.Join(d.Store.Dir, COMPLETE)) {
		t.Fatal("marker after a failed swap")
	}
}

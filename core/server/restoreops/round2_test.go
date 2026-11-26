package restoreops

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
)

// A script hook's dump says nothing about its own age. Carried over from an earlier run it is not
// as new as this run, so an erasure is not taken to be reflected in it.
func TestACarriedOverDumpWithoutAManifestIsOfUnknownAge(t *testing.T) {
	o, h, _ := ops(t, module("wiki", "disabled", func(m *config.Module) { classes("wiki-page")(m); m.Hooks.Purge = "./hooks/purge.sh" }))
	run := backup.DumpRun{RunID: "20260610T023000Z-abcd", OK: true, Skipped: []backup.SkippedModule{{ID: "wiki", Reason: "x", Kept: true}}}
	if err := backup.WriteDumpRun(o.DumpsDir, run); err != nil {
		t.Fatal(err)
	}
	es := []backup.Entry{erasedAt("", "p1", "wiki-page", "2026-06-01T10:00:00Z")}
	rep, err := o.Erase(context.Background(), es, nil)
	if err == nil || rep.Reflected != 0 || strings.Join(rep.Unapplied, ",") != "wiki:wiki-page" {
		t.Fatalf("the erasure was silently skipped: %v %+v %v", err, rep, h.calls)
	}
	// a dump of this run, not carried over, is as new as the run
	run.Skipped = nil
	if err := backup.WriteDumpRun(o.DumpsDir, run); err != nil {
		t.Fatal(err)
	}
	if rep, err = o.Erase(context.Background(), es, nil); err != nil || rep.Reflected != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
	if len(rep.SkippedByHand) != 1 || !strings.Contains(rep.SkippedByHand[0], "wiki-page p1") {
		t.Errorf("a skipped entry made by hand is not listed: %+v", rep.SkippedByHand)
	}
}

// An erasure's time is taken before its transaction commits: one stamped just before the copy may
// be missing from it, so it is replayed.
func TestAnErasureStampedJustBeforeTheDumpIsReplayed(t *testing.T) {
	o, h, _ := ops(t, module("people", "enabled", classes("directory-basic")))
	dumpedAt(t, o, "2026-06-10T02:30:00Z", "people")
	if _, err := o.Erase(context.Background(), []backup.Entry{
		erasedAt("people", "pers_close", "directory-basic", "2026-06-10T02:29:30Z"),
		erasedAt("people", "pers_old", "directory-basic", "2026-06-10T02:28:00Z"),
	}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.calls, ",") != "cli people pers_close directory-basic" {
		t.Fatal(h.calls)
	}
}

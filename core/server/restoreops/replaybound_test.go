package restoreops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
)

// dumpedAt records a module's dump as taken at the given time, and the run as begun then.
func dumpedAt(t *testing.T, o *Ops, taken string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		dir := filepath.Join(o.DumpsDir, "modules", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"taken_at":"`+taken+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func erasedAt(module, subject, class, at string) backup.Entry {
	return backup.Entry{Module: module, Subject: subject, Class: class, Date: at[:10], At: at}
}

// A backup made after an erasure already reflects it, and may hold data created since for the same
// subject: nothing is purged from it. Whatever the module, an erasure made after the backup is
// replayed.
func TestReplayIsBoundedByTheTimeOfTheDump(t *testing.T) {
	for _, m := range []struct{ id, class string }{
		{"people", "directory-basic"}, {"equity", "equity-stakeholder"}, {"board", "board-private"}, {"entity", "entity-person"},
	} {
		t.Run(m.id, func(t *testing.T) {
			o, h, _ := ops(t, module(m.id, "enabled", classes(m.class)))
			dumpedAt(t, o, "2026-06-10T02:30:00Z", m.id)
			rep, err := o.Erase(context.Background(), []backup.Entry{erasedAt(m.id, "s_before", m.class, "2026-06-01T10:00:00Z")}, nil)
			if err != nil || len(h.calls) != 0 || rep.Reflected != 1 {
				t.Fatalf("data created after the erasure would be destroyed: %v %v %+v", err, h.calls, rep)
			}
			// erased before and again after the dump: the later erasure is replayed, once
			rep, err = o.Erase(context.Background(), []backup.Entry{
				erasedAt(m.id, "s_1", m.class, "2026-06-01T10:00:00Z"), erasedAt(m.id, "s_1", m.class, "2026-06-11T10:00:00Z"),
			}, nil)
			if err != nil || strings.Join(h.calls, ",") != "cli "+m.id+" s_1 "+m.class || len(rep.Purged) != 1 {
				t.Fatalf("%v %v %+v", err, h.calls, rep)
			}
		})
	}
}

// Without a dump of its own the start of the dump run bounds the replay; when the set says neither,
// nothing is guessed.
func TestReplayWithoutAModuleDumpTime(t *testing.T) {
	o, h, _ := ops(t, module("people", "enabled", classes("directory-basic")))
	es := []backup.Entry{erasedAt("people", "pers_1", "directory-basic", "2026-06-01T10:00:00Z")}
	rep, err := o.Erase(context.Background(), es, nil)
	if err == nil || len(h.calls) != 0 || strings.Join(rep.Unapplied, ",") != "people:directory-basic" {
		t.Fatalf("%v %v %+v", err, h.calls, rep)
	}
	if err := backup.WriteDumpRun(o.DumpsDir, backup.DumpRun{RunID: "20260610T023000Z-abcd", OK: true}); err != nil {
		t.Fatal(err)
	}
	if rep, err = o.Erase(context.Background(), es, nil); err != nil || len(h.calls) != 0 || rep.Reflected != 1 {
		t.Fatalf("%v %v %+v", err, h.calls, rep)
	}
}

// What one module reported is applied to that module and to no other, whatever it claims.
func TestReplayGoesToTheReportingModuleOnly(t *testing.T) {
	o, h, _ := ops(t,
		module("people", "enabled", classes("directory-basic")),
		module("board", "enabled", classes("board-private")),
		module("entity", "enabled", classes("entity-person")),
		module("jobs", "enabled", classes("candidate-pii")))
	dumpedAt(t, o, "2026-06-10T02:30:00Z", "people", "board", "entity", "jobs")
	forged := erasedAt("jobs", "pers_1", "directory-basic", "2026-06-11T10:00:00Z")
	rep, err := o.Erase(context.Background(), []backup.Entry{forged}, nil)
	if err == nil || len(h.calls) != 0 || strings.Join(rep.Unapplied, ",") != "jobs:directory-basic" {
		t.Fatalf("jobs ordered a people purge: %v %v %+v", err, h.calls, rep)
	}
	// the shared class from the board instance stays in the board instance
	if _, err := o.Erase(context.Background(), []backup.Entry{erasedAt("board", "pers_1", "entity-person", "2026-06-11T10:00:00Z")}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.calls, ",") != "cli board pers_1 entity-person" {
		t.Fatal(h.calls)
	}
}

// can-core's derived copies are only dropped for what its own dump does not reflect, and a module's
// report reaches only what that module sent.
func TestCoreCleanupIsBounded(t *testing.T) {
	ctx := context.Background()
	o, _, _ := ops(t, module("people", "enabled", classes("directory-basic")), module("equity", "enabled", classes("equity-grants")))
	if err := backup.WriteDumpRun(o.DumpsDir, backup.DumpRun{RunID: "20260610T023000Z-abcd", OK: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.DB.ExecContext(ctx, `INSERT INTO events(id,type,time,source,data_json) VALUES
		('e1','x.v1',1,'work','{"party_id":"pers_old"}'), ('e2','x.v1',1,'work','{"party_id":"stk_1"}'), ('e3','x.v1',1,'equity','{"id":"stk_1"}')`); err != nil {
		t.Fatal(err)
	}
	rep, err := o.Erase(ctx, []backup.Entry{
		erasedAt("people", "pers_old", "directory-basic", "2026-06-01T10:00:00Z"),
		erasedAt("equity", "stk_1", "equity-grants", "2026-06-11T10:00:00Z"),
	}, nil)
	if err != nil || rep.CoreRows != 1 {
		t.Fatalf("%v %+v", err, rep)
	}
}

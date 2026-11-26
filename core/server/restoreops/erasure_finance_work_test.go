package restoreops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
)

// finance and work are purged by their own `purge` command, only into data copied before the
// erasure (specs/056-erasure-finance-work).
func TestEraseFinanceAndWorkRespectTheDumpTime(t *testing.T) {
	o, h, _ := ops(t,
		module("finance", "enabled", classes("finance-party-contact")),
		module("work", "enabled", classes("work-issue")))
	for _, m := range []string{"finance", "work"} {
		dir := filepath.Join(o.DumpsDir, "modules", m)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"taken_at":"2026-11-25T02:00:00Z"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	at := func(module, subject, class, when string) backup.Entry {
		return backup.Entry{Module: module, Subject: subject, Class: class, Date: when[:10], At: when}
	}
	rep, err := o.Erase(context.Background(), []backup.Entry{
		at("finance", "party-1-10", "finance-party-contact", "2026-11-24T09:00:00Z"), // the dump reflects it
		at("finance", "party-2-10", "finance-party-contact", "2026-11-25T09:00:00Z"),
		at("work", "person-old", "work-issue", "2026-11-24T09:00:00Z"),
		at("work", "person-new", "work-issue", "2026-11-25T09:00:00Z"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.calls, ","); got != "cli finance party-2-10 finance-party-contact,cli work person-new work-issue" {
		t.Error(got)
	}
	if rep.Reflected != 2 || len(rep.Purged) != 2 {
		t.Errorf("%+v", rep)
	}
}

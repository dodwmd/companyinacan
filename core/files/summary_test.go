package files

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSummaryStorageAndRetentionFollowTheCallersRole(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	build := summaryBuilder(e.s, nil)
	figures := func(subject string, perms ...string) map[string]int64 {
		t.Helper()
		got, err := build(ctx, subject, perms)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, f := range got {
			out[f.Key] = f.Count
		}
		return out
	}
	want := func(got, exp map[string]int64) {
		t.Helper()
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("figures %v, want %v", got, exp)
		}
	}
	mine := e.space(e.alice, "Alice")
	theirs := e.space(e.carol, "Carol")
	a := e.upload(e.alice, mine.ID, "a.pdf", pdf("small"))
	c := e.upload(e.carol, theirs.ID, "c.pdf", pdf("small too"))
	e.member(e.alice, mine.ID, "user", "user:bob", "editor")

	want(figures("user:alice", PermWrite), map[string]int64{"storage.managed": 1, "files.retention_ending": 0})
	// An editor manages nothing: no figure rather than a zero. No permission, no figure.
	want(figures("user:bob", PermWrite), map[string]int64{})
	want(figures("user:alice"), map[string]int64{})

	soon, late := e.now().Add(10*24*time.Hour), e.now().Add(60*24*time.Hour)
	if err := e.s.SetHold(ctx, e.root, a.ID, false, &soon); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetHold(ctx, e.root, c.ID, false, &late); err != nil {
		t.Fatal(err)
	}
	want(figures("user:alice", PermRead), map[string]int64{"storage.managed": 1, "files.retention_ending": 1})
	want(figures("user:carol", PermRead), map[string]int64{"storage.managed": 1, "files.retention_ending": 0})
	want(figures("user:root", PermAdmin), map[string]int64{"storage.company": 1, "files.retention_ending": 1})
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'summary.read'`) == 0 {
		t.Error("the read is not audited")
	}
}

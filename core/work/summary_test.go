package work

import (
	"context"
	"testing"
	"time"
)

func TestSummaryCountsOpenAndOverdueUrgentIssues(t *testing.T) {
	h := newHarness(t, nil)
	for _, p := range []string{"WEB", "OPS"} {
		h.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": p, "name": p})
	}
	yesterday := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	later := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	for _, i := range []struct{ project, priority, due string }{
		{"WEB", "P0", yesterday}, {"WEB", "P1", later}, {"WEB", "P1", ""}, {"WEB", "P2", yesterday}, {"OPS", "P1", yesterday},
	} {
		body := map[string]any{"type": "task", "title": "t", "priority": i.priority}
		if i.due != "" {
			body["due_date"] = i.due
		}
		h.mustDo(201, "POST", "/v1/projects/"+i.project+"/issues", bootstrap, body)
	}
	store := h.app.Store()
	count := func(projects ...string) [2]int64 {
		t.Helper()
		open, overdue, err := store.Summary(context.Background(), &Principal{Permissions: []string{PermRead}, Projects: projects})
		if err != nil {
			t.Fatal(err)
		}
		return [2]int64{open, overdue}
	}
	if got := count(); got != [2]int64{4, 2} {
		t.Errorf("all projects: %v", got)
	}
	if got := count("OPS"); got != [2]int64{1, 1} {
		t.Errorf("one project: %v", got)
	}
	build := summaryBuilder(store)
	for _, perms := range [][]string{nil, {"people.read"}} {
		if f, err := build(context.Background(), "user:alice", perms); err != nil || len(f) != 0 {
			t.Errorf("%v: %v %v", perms, f, err)
		}
	}
	f, err := build(context.Background(), "user:alice", []string{PermWrite})
	if err != nil || len(f) != 2 || f[0].Count != 2 || f[1].Count != 4 {
		t.Errorf("%+v %v", f, err)
	}
}

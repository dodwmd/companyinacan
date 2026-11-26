package report

import (
	"strings"
	"testing"
	"time"
)

func TestBudgetsAndFailuresAreViolations(t *testing.T) {
	var r Report
	if got := r.Add("fast", time.Second, time.Minute, false, ""); got != "ok" {
		t.Fatal(got)
	}
	if got := r.Add("unbudgeted", time.Hour, 0, false, ""); got != "ok" {
		t.Fatal(got)
	}
	if got := r.Add("slow", 2*time.Minute, time.Minute, false, ""); got != "over budget" {
		t.Fatal(got)
	}
	if got := r.Add("broken", time.Second, time.Minute, true, "a | b\nc"); got != "failed" {
		t.Fatal(got)
	}
	r.Memory = []Memory{{Label: "idle", K3s: 600, Pods: 500, Budget: 1000}, {Label: "small", K3s: 1, Pods: 1, Budget: 10}}
	r.Note("seen %d", 1)
	r.Note("seen %d", 1)
	v := r.Violations()
	if len(v) != 3 || len(r.Notes) != 1 {
		t.Fatalf("violations %v notes %v", v, r.Notes)
	}
	md := r.Markdown()
	for _, want := range []string{"Result: **failed**", "| broken | 1.0 | 60 | failed | a \\| b c |", "| idle | 600 | 500 | 1100 | 1000 |", "- seen 1"} {
		if !strings.Contains(md, want) {
			t.Errorf("no %q in\n%s", want, md)
		}
	}
}

func TestAPassingReportSaysSo(t *testing.T) {
	var r Report
	r.Add("init", 100*time.Millisecond, time.Minute, false, "")
	r.Runs = []Run{{Label: "first", Phases: []Phase{{Name: "base", Seconds: 2}, {Name: "images", Skipped: true}}}}
	md := r.Markdown()
	for _, want := range []string{"Result: **passed**", "| images (skipped) | 0.0 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("no %q in\n%s", want, md)
		}
	}
	if b, err := r.JSON(); err != nil || !strings.Contains(string(b), `"can_up_runs"`) {
		t.Fatal(err, string(b))
	}
	// A report with no steps proved nothing.
	if strings.Contains((&Report{}).Markdown(), "passed") {
		t.Error("an empty report says passed")
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("password hunter2hunter2 and ab", []string{"hunter2hunter2", "ab", ""}); got != "password [redacted] and ab" {
		t.Fatal(got)
	}
}

package work

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/obs"
)

func TestIssueCounter(t *testing.T) {
	read := func() string {
		var b strings.Builder
		_, _ = obs.Default.WriteTo(&b)
		for _, l := range strings.Split(b.String(), "\n") {
			if strings.HasPrefix(l, "can_work_issues_created_total ") {
				return strings.Fields(l)[1]
			}
		}
		return ""
	}
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	before := read()
	mustIssue(t, s, alice, "ACME", CreateIssue{Title: "Secret roadmap for Bob"})
	after := read()
	if after == "" || after == before {
		t.Fatalf("issue counter did not move: %q -> %q", before, after)
	}
	var b strings.Builder
	_, _ = obs.Default.WriteTo(&b)
	if strings.Contains(b.String(), "Bob") || strings.Contains(b.String(), "ACME") {
		t.Fatal("issue data in metrics")
	}
}

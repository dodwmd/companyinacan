package modapproval

import (
	"regexp"
	"testing"
)

// Whatever a module calls a person, can-core gets a subject it accepts, and only a real account can
// look like one.
func TestSubject(t *testing.T) {
	valid := regexp.MustCompile(`^(user|party):[A-Za-z0-9_.@-]{1,64}$`) // core/server/rbac
	for in, want := range map[string]string{
		"user:alice": "user:alice", "party:per_1": "party:per_1", "emp-1": "party:emp-1",
		"alice@example.com": "", "svc:e2e": "", "user:a b": "", "Éva": "",
	} {
		got := Subject(in)
		if !valid.MatchString(got) || (want != "" && got != want) || (got != in && IsAccount(got)) {
			t.Errorf("Subject(%q) = %q", in, got)
		}
	}
	if Subject("svc:a") == Subject("svc:b") {
		t.Error("two subjects share a name")
	}
}

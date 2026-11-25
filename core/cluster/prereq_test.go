package cluster

import (
	"os"
	"strings"
	"testing"
)

func TestPrerequisites(t *testing.T) {
	all := func(n string) (string, error) { return "/bin/" + n, nil }
	if miss := Prerequisites(all); len(miss) != 0 {
		t.Fatalf("unexpected: %v", miss)
	}
	onlyGo := func(n string) (string, error) {
		if n == "go" || n == "git" || n == "make" {
			return "/bin/" + n, nil
		}
		return "", os.ErrNotExist
	}
	var names []string
	for _, m := range Prerequisites(onlyGo) {
		names = append(names, m.Tool)
		if m.Remedy == "" {
			t.Errorf("%s has no remedy", m.Tool)
		}
	}
	got := strings.Join(names, ",")
	for _, want := range []string{"docker or podman", "k3d", "kubectl"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "helm") {
		t.Error("helm is optional for dev")
	}
}

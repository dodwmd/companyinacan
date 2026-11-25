package doctor

import (
	"errors"
	"testing"
)

func lookup(found ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, f := range found {
		set[f] = true
	}
	return func(n string) (string, error) {
		if set[n] {
			return "/usr/bin/" + n, nil
		}
		return "", errors.New("not found")
	}
}

func TestAllRequiredPresent(t *testing.T) {
	res := Check(lookup("go", "git", "make"))
	if HasMissingRequired(res) {
		t.Fatalf("unexpected missing required: %+v", res)
	}
	var optMissing int
	for _, r := range res {
		if r.Status == StatusOptionalMissing {
			optMissing++
			if r.Remedy == "" {
				t.Errorf("%s: optional-missing tool needs a remedy", r.Tool)
			}
		}
	}
	if optMissing == 0 {
		t.Fatal("expected some optional tools reported missing")
	}
}

func TestMissingRequired(t *testing.T) {
	res := Check(lookup("go", "make")) // git missing
	if !HasMissingRequired(res) {
		t.Fatal("expected missing required")
	}
	for _, r := range res {
		if r.Tool == "git" && (r.Status != StatusMissing || r.Remedy == "") {
			t.Fatalf("git: %+v", r)
		}
	}
}

func TestContainerRuntimeAcceptsPodman(t *testing.T) {
	for _, r := range Check(lookup("go", "git", "make", "podman")) {
		if r.Tool == "docker or podman" && r.Status != StatusOK {
			t.Fatalf("podman should satisfy the container runtime check: %+v", r)
		}
	}
}

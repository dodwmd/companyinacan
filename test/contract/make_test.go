package contract

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// The up target is a stub until epic E06: they must fail (make reports status 2;
// the underlying `can up` exits 3) with a clear message and no side effects.
func TestMakeStubTargets(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	for target, epic := range map[string]string{"up": "E06"} {
		cmd := exec.Command("make", "-s", target)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() == 0 {
			t.Fatalf("make %s: want a non-zero exit, got %v (%s)", target, err, out)
		}
		if !strings.Contains(string(out), "not implemented") || !strings.Contains(string(out), epic) {
			t.Fatalf("make %s: unclear message %q", target, out)
		}
	}
}

// make dev|down|doctor wrap `can dev up|down|doctor` (epic E02); checked with -n so no cluster is needed.
func TestMakeDevTargets(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	for target, want := range map[string]string{"dev": "dev up", "down": "dev down", "doctor": "dev doctor"} {
		cmd := exec.Command("make", "-n", target)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("make -n %s: err=%v out=%q, want it to run `can %s`", target, err, out, want)
		}
	}
}

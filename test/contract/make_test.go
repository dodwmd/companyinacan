package contract

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// The dev and up targets are stubs until epics E02 and E06: they must fail (make reports status 2;
// the underlying `can dev|up` exits 3) with a clear message and no side effects.
func TestMakeStubTargets(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	for target, epic := range map[string]string{"dev": "E02", "up": "E06"} {
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

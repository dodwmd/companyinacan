package contract

import (
	"os/exec"
	"strings"
	"testing"
)

// The make targets are thin wrappers over `can`; checked with -n so nothing runs
// (no cluster, no server). dev and doctor are epic E02, up/down/status/server-down epic E06.
// `make down` runs `can down`, which dispatches on provider.type (local -> `can dev down`).
func TestMakeTargetsWrapCan(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	want := map[string]string{
		"dev":         "can dev up",
		"doctor":      "can dev doctor",
		"down":        "can down",
		"up":          "can up",
		"status":      "can status",
		"server-down": "can down",
	}
	for target, run := range want {
		cmd := exec.Command("make", "-n", target)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), run) {
			t.Fatalf("make -n %s: err=%v out=%q, want it to run `%s`", target, err, out, run)
		}
	}
}

// Restored from E02: make dev|doctor map to `can dev up|doctor`.
func TestMakeDevTargets(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	for target, want := range map[string]string{"dev": "dev up", "doctor": "dev doctor"} {
		cmd := exec.Command("make", "-n", target)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("make -n %s: err=%v out=%q, want it to run `can %s`", target, err, out, want)
		}
	}
}

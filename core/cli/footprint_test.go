package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fpPods = `{"items":[{"metadata":{"namespace":"mod-forms","name":"forms-1","uid":"11111111-2222-3333-4444-555555555555"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`

func fpSample(mb int) string {
	return fmt.Sprintf("NODE 419430400 0\nCG /kubepods/burstable/pod11111111-2222-3333-4444-555555555555 %d 0\nPROC k3s-server 100000\n", mb*1024*1024)
}

func TestFootprintReportAndCheck(t *testing.T) {
	dir := t.TempDir()
	samples := filepath.Join(dir, "s")
	if err := os.Mkdir(samples, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, mb := range []int{16, 17, 17} {
		if err := os.WriteFile(filepath.Join(samples, "00"+fmt.Sprint(i+1)+".txt"), []byte(fpSample(mb)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pods := filepath.Join(dir, "pods.json")
	if err := os.WriteFile(pods, []byte(fpPods), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "footprint", "report", "--pods", pods, "--samples", samples, "--label", "t")
	if code != ExitOK {
		t.Fatalf("report: %d %s", code, errs)
	}
	report := filepath.Join(dir, "r.json")
	if err := os.WriteFile(report, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, _ = run(t, "footprint", "check", "--report", report, "--namespace", "mod-forms", "--ram-mb", "15")
	if code != ExitOK || !strings.Contains(out, "within") {
		t.Fatalf("within: %d %s", code, out)
	}
	code, out, _ = run(t, "footprint", "check", "--report", report, "--namespace", "mod-forms", "--ram-mb", "10")
	if code != ExitFailure || !strings.Contains(out, "OVER") {
		t.Fatalf("over: %d %s", code, out)
	}
	// declared figure from a module manifest: modules/forms declares 15
	code, _, errs = run(t, "footprint", "check", "--report", report, "--namespace", "mod-forms", "--module", "../../modules/forms/module.yaml")
	if code != ExitOK {
		t.Fatalf("module: %d %s", code, errs)
	}
	code, _, _ = run(t, "footprint", "check", "--report", report, "--namespace", "nope", "--ram-mb", "15")
	if code != ExitFailure {
		t.Fatalf("empty selection must fail, got %d", code)
	}
	code, _, _ = run(t, "footprint", "check", "--report", report, "--namespace", "mod-forms")
	if code != ExitUsage {
		t.Fatalf("missing declared figure: %d", code)
	}
	code, _, _ = run(t, "footprint", "report")
	if code != ExitUsage {
		t.Fatalf("report without input: %d", code)
	}
}

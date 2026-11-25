package corecli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installer re-executes can-core as `render-chart`; this is the can-core side of that contract.
func TestRenderChartSubcommand(t *testing.T) {
	mod, err := filepath.Abs("../../../test/integration/testdata/modules/stub")
	if err != nil {
		t.Fatal(err)
	}
	job := map[string]any{
		"module_dir": mod, "chart": filepath.Join(mod, "chart"), "settings": map[string]any{"greeting": "hi"},
		"can": map[string]any{}, "input": map[string]any{}, "release": map[string]any{"Name": "stub", "Namespace": "mod-stub"}, "timeout_s": 5,
	}
	in, _ := json.Marshal(job)
	var out, errb bytes.Buffer
	code := Execute([]string{"render-chart", "--heap-mb=64"}, bytes.NewReader(in), &out, &errb, func(string) string { return "" })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var res struct {
		Objects []map[string]any
		Error   string
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Error != "" || len(res.Objects) != 2 {
		t.Fatalf("%v %q %s", err, res.Error, out.String())
	}
	if !strings.Contains(out.String(), `"hi"`) {
		t.Fatalf("settings not mapped: %s", out.String())
	}
}

func TestRenderChartSubcommandReportsAChartErrorInTheResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chart", "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chart", "templates", "a.yaml"), []byte(`{{ fail "nope" }}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"module_dir": dir, "chart": filepath.Join(dir, "chart"), "release": map[string]any{"Name": "x", "Namespace": "n"}})
	var out bytes.Buffer
	if code := Execute([]string{"render-chart"}, bytes.NewReader(in), &out, &bytes.Buffer{}, func(string) string { return "" }); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "nope") {
		t.Fatalf("%s", out.String())
	}
}

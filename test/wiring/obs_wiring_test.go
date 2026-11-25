package wiring

import (
	"os"
	"strings"
	"testing"
)

// Every in-house service must opt into the shared observability layer (epic E22). A forgotten
// service would silently have no metrics, no /readyz and unredacted logs, so this fails the build.
func TestEveryServiceIsObservable(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile("../../" + p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, m := range []string{"forms", "work", "jobs", "finance", "people", "equity", "files"} {
		cmd := read("core/" + m + "/cmd.go")
		for _, want := range []string{"obs.Setup(", "obslog.New("} {
			if !strings.Contains(cmd, want) {
				t.Errorf("core/%s/cmd.go: missing %s", m, want)
			}
		}
		if strings.Contains(cmd, "slog.NewJSONHandler(stderr") {
			t.Errorf("core/%s/cmd.go: builds its own logger without redaction", m)
		}
		storeFile := "store.go"
		if m == "people" || m == "equity" {
			storeFile = "service.go"
		}
		store := read("core/" + m + "/" + storeFile)
		if !strings.Contains(store, "obssql.WatchDefault(") {
			t.Errorf("core/%s: store does not register readiness and DB metrics", m)
		}
		// the handler chain must be instrumented somewhere in the package
		found := false
		entries, _ := os.ReadDir("../../core/" + m)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") && strings.Contains(read("core/"+m+"/"+e.Name()), "obs.Instrument(") {
				found = true
			}
		}
		if !found {
			t.Errorf("core/%s: no obs.Instrument on the HTTP handler", m)
		}
	}
	if !strings.Contains(read("cmd/can-static/main.go"), "obs.Setup(") || !strings.Contains(read("cmd/can-static/main.go"), "obs.Instrument(") {
		t.Error("can-static is not instrumented")
	}
}

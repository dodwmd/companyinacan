package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// An upgrade that renders differently each time would restart pods on every apply: the same module
// renders to the same bytes.
func TestShippedChartsRenderTheSameEveryTime(t *testing.T) {
	dirs, err := filepath.Glob("../../../modules/*/module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	in := New(Config{PlatformURL: "http://can-core:8090", Domain: "acme.example"})
	rendered := 0
	for _, path := range dirs {
		dir := filepath.Dir(path)
		id := filepath.Base(dir)
		m := loadManifest(t, path)
		chart := filepath.Join(dir, filepath.Clean("/"+m.Runtime.Chart))
		if _, err := os.Stat(filepath.Join(chart, "templates")); err != nil {
			continue
		}
		spec := reconcile.Spec{ID: id, Version: m.Version, Namespace: "mod-" + id, ServiceAccount: id, Manifest: m}
		render := func() []byte {
			objs, err := renderJobInProcess(context.Background(), in.jobFor(spec, dir, chart))
			if err != nil {
				return []byte(err.Error())
			}
			rendered++
			b, err := json.Marshal(objs)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		first := render()
		for range 10 {
			if next := render(); !bytes.Equal(first, next) {
				t.Errorf("%s: two renderings differ:\n%s\n%s", id, first, next)
				break
			}
		}
	}
	if rendered == 0 {
		t.Fatal("no chart rendered")
	}
}

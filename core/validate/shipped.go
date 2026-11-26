package validate

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
)

// BuiltinModules, given as the modules directory, means the manifests built into the binary
// (UseShipped). Nothing falls back to them by itself: the caller has to name them.
const BuiltinModules = "(built-in)"

var shipped fs.FS

// UseShipped sets the built-in manifests: <id>/module.yaml. Call it before validating.
func UseShipped(manifests fs.FS) { shipped = manifests }

// ManifestPath resolves a module id to its manifest in modulesDir (id or _id directory).
// LoadManifest reads what it returns.
func ManifestPath(modulesDir, id string) (string, bool) {
	for _, d := range []string{id, "_" + id} {
		if modulesDir == BuiltinModules {
			p := d + "/module.yaml"
			if shipped != nil && fs.ValidPath(p) {
				if _, err := fs.Stat(shipped, p); err == nil {
					return BuiltinModules + "/" + p, true
				}
			}
			continue
		}
		p := filepath.Join(modulesDir, d, "module.yaml")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// LoadManifest loads a module.yaml from a path ManifestPath returned, or any file path.
func LoadManifest(path string) (*config.Doc, []problem.Problem) {
	rel, ok := strings.CutPrefix(path, BuiltinModules+"/")
	if !ok || shipped == nil {
		return config.Load(path)
	}
	data, err := fs.ReadFile(shipped, rel)
	if err != nil {
		return nil, []problem.Problem{{File: path, Rule: "parse.read", Severity: problem.Error, Message: err.Error()}}
	}
	return config.LoadBytes(path, data)
}

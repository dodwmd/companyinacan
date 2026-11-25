package validate

import (
	"path/filepath"
	"strings"
	"testing"
)

// QA 6: enabling a module whose backup hook cannot run is warned about wherever manifests are
// validated, before it silently stops every backup.
func TestUnrunnableBackupHookWarns(t *testing.T) {
	for module, want := range map[string]bool{"wiki": true, "status": true, "forms": false} {
		ps := ModuleFile(filepath.Join("..", "..", "modules", module, "module.yaml"))
		var got bool
		for _, p := range ps {
			if p.Rule == "semantic.backup-hooks-image" && strings.Contains(p.Message, "not published") {
				got = true
			}
		}
		if got != want {
			t.Errorf("%s: warned=%v, want %v (%v)", module, got, want, ps)
		}
	}
}

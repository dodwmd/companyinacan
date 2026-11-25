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

// QA2 1: the warnings name the way out and do not claim disabling is not enough.
func TestUnrunnableBackupHookWarningNamesTheWayOut(t *testing.T) {
	for _, p := range ModuleFile(filepath.Join("..", "..", "modules", "crm", "module.yaml")) {
		if p.Rule != "semantic.backup-hooks-image" {
			continue
		}
		for _, want := range []string{"Disable the module", "database is NOT backed up"} {
			if !strings.Contains(p.Message, want) {
				t.Errorf("missing %q: %s", want, p.Message)
			}
		}
		return
	}
	t.Fatal("no warning")
}

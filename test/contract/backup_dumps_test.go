package contract

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
)

// E08b (specs/037-backup-dumps): can-core writes dumps to its own volume; no pod uses a
// hostPath, which can-system's Pod Security level would reject.
func TestCanCoreDumpsVolume(t *testing.T) {
	docs := loadDocs(t, "deploy/can-core/can-core.yaml")
	var claim, volume bool
	for _, p := range ofKind(docs, "PersistentVolumeClaim") {
		if p.name() == "can-dumps" && p.ns() == "can-system" && p.str("spec", "volumeName") == "can-dumps" {
			claim = true
		}
	}
	for _, v := range ofKind(docs, "PersistentVolume") {
		// The path the backup script and `can restore` use, bound to exactly that claim.
		if v.name() == "can-dumps" && v.str("spec", "hostPath", "path") == backup.DefaultSettings().DumpsDir &&
			v.str("spec", "claimRef", "namespace") == "can-system" && v.str("spec", "claimRef", "name") == "can-dumps" &&
			v.str("spec", "persistentVolumeReclaimPolicy") == "Retain" {
			volume = true
		}
	}
	if !claim || !volume {
		t.Fatalf("static can-dumps volume (%v) and claim (%v) must tie can-core to the host dumps directory", volume, claim)
	}
	d := ofKind(docs, "Deployment")[0]
	pod := toJSON(d.get("spec", "template", "spec"))
	for _, want := range []string{`"claimName":"can-dumps"`, `"mountPath":"/var/lib/can-dumps"`, `"CAN_DUMPS_DIR"`, `"value":"/var/lib/can-dumps"`} {
		if !strings.Contains(pod, want) {
			t.Errorf("can-core pod lacks %s", want)
		}
	}
	if strings.Contains(pod, "hostPath") {
		t.Error("can-core must not use a hostPath")
	}
}

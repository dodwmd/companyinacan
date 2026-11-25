package contract

import (
	"strings"
	"testing"
)

// E08b (specs/037-backup-dumps): can-core writes dumps to its own volume; no pod uses a
// hostPath, which can-system's Pod Security level would reject.
func TestCanCoreDumpsVolume(t *testing.T) {
	docs := loadDocs(t, "deploy/can-core/can-core.yaml")
	var claim bool
	for _, p := range ofKind(docs, "PersistentVolumeClaim") {
		if p.name() == "can-dumps" && p.ns() == "can-system" {
			claim = true
		}
	}
	if !claim {
		t.Fatal("no can-dumps PVC in can-system: the host backup script reads its local-path directory by that name")
	}
	d := ofKind(docs, "Deployment")[0]
	pod := toJSON(d.get("spec", "template", "spec"))
	for _, want := range []string{`"claimName":"can-dumps"`, `"mountPath":"/var/lib/can-dumps"`, `"CAN_DUMPS_DIR"`, `"value":"/var/lib/can-dumps/dumps"`} {
		if !strings.Contains(pod, want) {
			t.Errorf("can-core pod lacks %s", want)
		}
	}
	if strings.Contains(pod, "hostPath") {
		t.Error("can-core must not use a hostPath")
	}
}

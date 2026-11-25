package installer

import (
	"context"
	"testing"
)

// With the production limits (address space = start-up size + 256 MiB, 32 MiB heap watchdog) a normal
// chart still renders and a hostile one is stopped.
func TestChildRunsUnderTheProductionLimits(t *testing.T) {
	in, _, _, spec := fixture(t, nil)
	in.cfg.ChildAddressSpaceMB = 0 // default limits
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatalf("stub chart under the real limits: %v", err)
	}
	in, _, _, spec = fixture(t, map[string]string{"stub/chart/templates/dag.yaml": dagTemplate("$l")})
	in.cfg.ChildAddressSpaceMB = 0
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("hostile chart installed")
	}
}

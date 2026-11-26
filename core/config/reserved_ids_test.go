package config

import "testing"

// QA of #52, 10: a module with a platform name would be taken for can-core (mail source, event source,
// approval and notification types, permissions).
func TestPlatformNamesAreNotModuleIDs(t *testing.T) {
	for _, id := range []string{"can-core", "core", "platform", "rbac", "approval", "approvals", "notification", "notifications"} {
		v := PlatformViolations(&Module{ID: id})
		if len(v) != 1 || v[0].Path != "/id" {
			t.Errorf("%s: %v", id, v)
		}
	}
	if v := PlatformViolations(&Module{ID: "crm"}); len(v) != 0 {
		t.Error(v)
	}
}

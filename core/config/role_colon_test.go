package config

import "testing"

// Role names are namespaced "<module>:<role>" downstream; a manifest may not smuggle the
// separator into its own role names.
func TestPlatformViolationsRejectRoleNamesWithColon(t *testing.T) {
	m := &Module{ID: "evil", Permissions: []string{"evil.read"}, Roles: map[string][]string{"forms:admin": {"evil.read"}, "fine": {"evil.read"}, ":": nil}}
	got := map[string]bool{}
	for _, v := range PlatformViolations(m) {
		got[v.Path] = true
	}
	for _, want := range []string{"/roles/forms:admin", "/roles/:"} {
		if !got[want] {
			t.Errorf("no violation at %s: %v", want, got)
		}
	}
	if got["/roles/fine"] {
		t.Error("a plain role name was refused")
	}
}

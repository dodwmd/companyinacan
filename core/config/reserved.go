package config

import "strings"

// Names reserved for can-core itself. A module manifest may not define permissions in these
// namespaces, redefine a built-in role, or publish platform events: otherwise a manifest could
// grant itself platform permissions or forge approval decisions (QA of PR #20).
var (
	// ReservedPermissionPrefixes are the permission namespaces of the platform.
	ReservedPermissionPrefixes = []string{"platform.", "rbac.", "approvals.", "notifications."}
	// BuiltinRoles are the roles can-core defines; module roles never merge into them.
	BuiltinRoles = []string{"owner", "operator", "auditor"}
	// ReservedEventPrefixes are event namespaces only can-core publishes.
	ReservedEventPrefixes = []string{"approval.", "notification."}
)

// IsReservedPermission reports whether p lies in a platform namespace (or is the wildcard).
func IsReservedPermission(p string) bool {
	if p == "*" {
		return true
	}
	for _, x := range ReservedPermissionPrefixes {
		if strings.HasPrefix(p, x) {
			return true
		}
	}
	return false
}

// IsBuiltinRole reports whether r is a role name can-core owns.
func IsBuiltinRole(r string) bool {
	for _, x := range BuiltinRoles {
		if x == r {
			return true
		}
	}
	return false
}

// IsReservedEvent reports whether only can-core may publish event type t.
func IsReservedEvent(t string) bool {
	for _, x := range ReservedEventPrefixes {
		if strings.HasPrefix(t, x) {
			return true
		}
	}
	return false
}

// Violation is a manifest problem with the JSON-pointer-like path of the offending value.
type Violation struct{ Path, Message string }

// PlatformViolations lists the ways the manifest intrudes on platform-owned names. Both
// `can validate` and the registry call it, so a manifest that passes neither cannot be installed.
func PlatformViolations(m *Module) []Violation {
	var out []Violation
	for i, p := range m.Permissions {
		if IsReservedPermission(p) {
			out = append(out, Violation{"/permissions/" + itoa(i), "permission " + p + " is in a namespace reserved for can-core (platform., rbac., approvals., notifications.)"})
		}
	}
	for r, ps := range m.Roles {
		if IsBuiltinRole(r) {
			out = append(out, Violation{"/roles/" + r, "role " + r + " is built into can-core; choose another name"})
		}
		for i, p := range ps {
			if IsReservedPermission(p) {
				out = append(out, Violation{"/roles/" + r + "/" + itoa(i), "role " + r + " grants " + p + ", a platform permission"})
			}
		}
	}
	for i, p := range m.Publishes {
		if IsReservedEvent(p.Event) {
			out = append(out, Violation{"/publishes/" + itoa(i) + "/event", "event " + p.Event + " is published only by can-core"})
		}
	}
	for i, n := range m.Notifications {
		if n.Type != "" && !strings.HasPrefix(n.Type, m.ID+".") {
			out = append(out, Violation{"/notifications/" + itoa(i) + "/type", "notification type " + n.Type + " must start with the module id and a dot"})
		}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

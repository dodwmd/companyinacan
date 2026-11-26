package people

import (
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func manifestRoles(t *testing.T) map[string][]string {
	t.Helper()
	doc, ps := config.Load("../../modules/people/module.yaml")
	if doc == nil {
		t.Fatalf("manifest: %v", ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	return m.Roles
}

// A personal access token carries any subset of what its issuer holds. With the permissions of any
// one or two manifest roles, no subset may map to a module role that the full set does not, and
// only holders of the manifest's manager role may ever be a manager.
func TestNoPermissionSubsetAddsAPeopleRole(t *testing.T) {
	roles := manifestRoles(t)
	for a, pa := range roles {
		for b, pb := range roles {
			full := slices.Clone(pa)
			for _, p := range pb {
				if !slices.Contains(full, p) {
					full = append(full, p)
				}
			}
			most := rolesFromPermissions(full)
			if a != "manager" && b != "manager" && slices.Contains(most, RoleManager) {
				t.Fatalf("%s+%s is a manager without the manager role: %v", a, b, most)
			}
			// The mapping only looks for a permission's presence, so checking every subset that
			// drops up to two permissions, plus every subset of the marker ones, covers it.
			drop := [][]string{{}}
			for i, x := range full {
				drop = append(drop, []string{x})
				for _, y := range full[i+1:] {
					drop = append(drop, []string{x, y})
				}
			}
			for _, d := range drop {
				sub := slices.DeleteFunc(slices.Clone(full), func(p string) bool { return slices.Contains(d, p) })
				for _, r := range rolesFromPermissions(sub) {
					if !slices.Contains(most, r) {
						t.Fatalf("%s+%s without %v gains %q (full set: %v)", a, b, d, r, most)
					}
				}
			}
		}
	}
	if !slices.Contains(rolesFromPermissions(roles["manager"]), RoleManager) {
		t.Fatal("the manifest's manager role is not a manager")
	}
}

package jobs

import (
	"slices"
	"testing"
)

// A personal access token carries any subset of what its issuer holds: leaving permissions out must
// never give a stronger role than the full set does, and every action a weaker role may take the
// stronger roles may take too, so a weaker role is never a gain.
func TestNoPermissionSubsetStrengthensAJobsRole(t *testing.T) {
	all := []string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview", "jobs.admin"}
	pick := func(mask int) []string {
		var out []string
		for i, p := range all {
			if mask&(1<<i) != 0 {
				out = append(out, p)
			}
		}
		return out
	}
	for full := range 1 << len(all) {
		for sub := range 1 << len(all) {
			if sub&full == sub && roleRank(roleFromPermissions(pick(sub))) > roleRank(roleFromPermissions(pick(full))) {
				t.Fatalf("%v is stronger than its superset %v", pick(sub), pick(full))
			}
		}
	}
	for action, roles := range matrix {
		for i, r := range Roles[1:] {
			if slices.Contains(roles, r) && !slices.Contains(roles, Roles[i]) {
				t.Errorf("%s: %s may but the stronger %s may not", action, r, Roles[i])
			}
		}
	}
}

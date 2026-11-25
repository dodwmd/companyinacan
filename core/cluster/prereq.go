package cluster

import "github.com/dodwmd/companyinacan/core/doctor"

// devTools are the doctor checks `can dev up` needs.
var devTools = map[string]bool{"docker or podman": true, "k3d": true, "kubectl": true}

// Prerequisites returns the dev-cluster tools that are not found (reusing core/doctor).
func Prerequisites(look func(string) (string, error)) []doctor.Result {
	var miss []doctor.Result
	for _, r := range doctor.Check(look) {
		if devTools[r.Tool] && r.Status != doctor.StatusOK {
			miss = append(miss, r)
		}
	}
	return miss
}

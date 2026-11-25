// Package doctor checks local prerequisites without modifying the machine.
package doctor

// Statuses.
const (
	StatusOK              = "ok"
	StatusMissing         = "missing"
	StatusOptionalMissing = "optional-missing"
)

// Result is the outcome for one prerequisite.
type Result struct {
	Tool     string `json:"tool"`
	Required bool   `json:"required"`
	Status   string `json:"status"`
	Path     string `json:"path,omitempty"`
	Remedy   string `json:"remedy,omitempty"`
}

type check struct {
	name     string
	binaries []string // any one satisfies the check
	required bool
	remedy   string
}

var checks = []check{
	{"go", []string{"go"}, true, "install Go from https://go.dev/dl/"},
	{"git", []string{"git"}, true, "install git from your package manager"},
	{"make", []string{"make"}, true, "install make (for example the build-essential package)"},
	{"docker or podman", []string{"docker", "podman"}, false, "needed for `make dev`: install Docker or Podman"},
	{"k3d", []string{"k3d"}, false, "needed for `make dev`: https://k3d.io"},
	{"kubectl", []string{"kubectl"}, false, "needed for `make dev`: https://kubernetes.io/docs/tasks/tools/"},
	{"helm", []string{"helm"}, false, "optional, handy for debugging: https://helm.sh/docs/intro/install/"},
}

// Check probes each prerequisite using look (normally exec.LookPath).
func Check(look func(string) (string, error)) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		r := Result{Tool: c.name, Required: c.required}
		for _, b := range c.binaries {
			if p, err := look(b); err == nil {
				r.Status, r.Path = StatusOK, p
				break
			}
		}
		if r.Status == "" {
			r.Remedy = c.remedy
			if c.required {
				r.Status = StatusMissing
			} else {
				r.Status = StatusOptionalMissing
			}
		}
		out = append(out, r)
	}
	return out
}

// HasMissingRequired reports whether any required tool is missing.
func HasMissingRequired(rs []Result) bool {
	for _, r := range rs {
		if r.Required && r.Status != StatusOK {
			return true
		}
	}
	return false
}

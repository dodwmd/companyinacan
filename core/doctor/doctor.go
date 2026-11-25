// Package doctor checks local prerequisites without modifying the machine.
package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Statuses.
const (
	StatusOK              = "ok"
	StatusMissing         = "missing"
	StatusOptionalMissing = "optional-missing"
	StatusUnreachable     = "unreachable" // installed, but the daemon or API did not answer (never fails doctor)
	StatusNotRunning      = "not-running" // reachable, nothing deployed there
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

// Runner runs a command and returns its standard output (os/exec in production).
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ComposeProject is the docker compose project of docs/local-compose.md.
const ComposeProject = "companyinacan"

// Reach reports whether the tools found by look can reach what they manage: the Docker daemon, the
// local compose stack and the Kubernetes API. Each probe is a single cheap command with a short
// timeout, and a tool that is not installed is not probed. Nothing here is required, so it never
// changes doctor's exit code, and command output is never echoed (it could carry credentials).
func Reach(ctx context.Context, look func(string) (string, error), run Runner) []Result {
	var out []Result
	probe := func(name string, args ...string) (string, error) {
		c, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		return run(c, name, args...)
	}
	if _, err := look("docker"); err == nil {
		r := Result{Tool: "docker daemon", Status: StatusOK}
		if v, err := probe("docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
			r.Status, r.Remedy = StatusUnreachable, "start Docker (or check DOCKER_HOST); `make dev` and the compose stack need it"
			out = append(out, r)
		} else {
			r.Path = "server " + strings.TrimSpace(v)
			out = append(out, r)
			c := Result{Tool: "compose stack " + ComposeProject}
			if ids, err := probe("docker", "compose", "-p", ComposeProject, "ps", "-q"); err != nil {
				c.Status, c.Remedy = StatusUnreachable, "could not list the compose project; see docs/local-compose.md"
			} else if n := len(strings.Fields(ids)); n == 0 {
				c.Status, c.Remedy = StatusNotRunning, "optional: docker compose -p "+ComposeProject+" up -d (docs/local-compose.md)"
			} else {
				c.Status, c.Path = StatusOK, fmt.Sprintf("%d containers", n)
			}
			out = append(out, c)
		}
	}
	if _, err := look("kubectl"); err == nil {
		r := Result{Tool: "kubernetes API", Status: StatusOK}
		if _, err := probe("kubectl", "--request-timeout=3s", "get", "--raw=/readyz"); err != nil {
			r.Status, r.Remedy = StatusUnreachable, "no cluster answered: `make dev` creates a local one, or check your kubectl context"
		}
		out = append(out, r)
	}
	return out
}

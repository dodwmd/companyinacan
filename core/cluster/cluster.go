// Package cluster plans and runs the local k3d development cluster (`can dev`).
//
// Plans are plain data (lists of Cmd) so the exact k3d/kubectl invocations can be
// asserted offline and shown with --dry-run. Execution goes through an Executor.
package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Cmd is one external command invocation.
type Cmd struct {
	Name string
	Args []string
}

// String renders the command as a shell-style line (no quoting: no argument contains spaces).
func (c Cmd) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

// Plan is an ordered list of commands.
type Plan []Cmd

// Lines renders each command.
func (p Plan) Lines() []string {
	out := make([]string, len(p))
	for i, c := range p {
		out[i] = c.String()
	}
	return out
}

// Executor runs a command and returns its standard output.
type Executor interface {
	Run(ctx context.Context, c Cmd) (stdout string, err error)
}

// Config locates the cluster definition. Paths are relative to the repository root.
type Config struct {
	Name         string // k3d cluster name
	K3dConfig    string // k3d config file
	ManifestsDir string // kustomize directory applied with kubectl apply -k
}

// DefaultConfig is the cluster `make dev` creates.
func DefaultConfig() Config {
	return Config{Name: "companyinacan-dev", K3dConfig: "dev/k3d.yaml", ManifestsDir: "deploy/base"}
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Validate checks the cluster name (lower-case DNS label, at most 32 characters).
func (c Config) Validate() error {
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("invalid cluster name %q: want 1-32 lower-case letters, digits or '-', not starting or ending with '-'", c.Name)
	}
	return nil
}

func (c Config) kubectl(args ...string) Cmd {
	return Cmd{"kubectl", append([]string{"--context", "k3d-" + c.Name}, args...)}
}

// CreatePlan creates the cluster and applies the base manifests.
func (c Config) CreatePlan() Plan {
	return append(Plan{
		{"k3d", []string{"cluster", "create", c.Name, "--config", c.K3dConfig}},
		c.kubectl("wait", "--for=condition=Ready", "node", "--all", "--timeout=180s"),
	}, c.ApplyPlan()...)
}

// ApplyPlan applies the base manifests to an existing cluster and reloads CoreDNS so the
// coredns-custom ConfigMap takes effect.
func (c Config) ApplyPlan() Plan {
	return Plan{
		c.kubectl("wait", "--for=condition=Established", "crd/helmchartconfigs.helm.cattle.io", "--timeout=180s"),
		c.kubectl("apply", "-k", c.ManifestsDir),
		c.kubectl("-n", "kube-system", "rollout", "restart", "deployment/coredns"),
		c.kubectl("-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=120s"),
	}
}

// DeletePlan removes the cluster.
func (c Config) DeletePlan() Plan {
	return Plan{{"k3d", []string{"cluster", "delete", c.Name}}}
}

// StatusPlan queries the cluster.
func (c Config) StatusPlan() Plan {
	return Plan{{"k3d", []string{"cluster", "list", c.Name, "--output", "json"}}}
}

// Status is the observed state of the cluster.
type Status struct {
	Exists         bool `json:"exists"`
	Running        bool `json:"running"`
	Servers        int  `json:"servers"`
	ServersRunning int  `json:"serversRunning"`
}

// Manager runs plans through an Executor.
type Manager struct {
	cfg Config
	ex  Executor
}

// New returns a Manager.
func New(cfg Config, ex Executor) *Manager { return &Manager{cfg, ex} }

func (m *Manager) runPlan(ctx context.Context, p Plan) error {
	for _, c := range p {
		if _, err := m.ex.Run(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", c, err)
		}
	}
	return nil
}

// Status reports whether the cluster exists and is running. A failing `k3d cluster list`
// is treated as "absent" (k3d exits non-zero for an unknown cluster); a successful call
// with unparseable output is an error.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	out, err := m.ex.Run(ctx, m.cfg.StatusPlan()[0])
	if err != nil {
		return Status{}, nil
	}
	var list []struct {
		Name           string `json:"name"`
		ServersCount   int    `json:"serversCount"`
		ServersRunning int    `json:"serversRunning"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return Status{}, fmt.Errorf("unexpected output from k3d cluster list: %w", err)
	}
	for _, e := range list {
		if e.Name == m.cfg.Name {
			return Status{Exists: true, Running: e.ServersRunning > 0 && e.ServersRunning == e.ServersCount,
				Servers: e.ServersCount, ServersRunning: e.ServersRunning}, nil
		}
	}
	return Status{}, nil
}

// Up creates the cluster if it does not exist, then (re)applies the base manifests.
func (m *Manager) Up(ctx context.Context) error {
	if err := m.cfg.Validate(); err != nil {
		return err
	}
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if st.Exists {
		return m.runPlan(ctx, m.cfg.ApplyPlan())
	}
	return m.runPlan(ctx, m.cfg.CreatePlan())
}

// Down deletes the cluster.
func (m *Manager) Down(ctx context.Context) error {
	if err := m.cfg.Validate(); err != nil {
		return err
	}
	return m.runPlan(ctx, m.cfg.DeletePlan())
}

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
	"time"
)

// Cmd is one external command invocation.
type Cmd struct {
	Name string
	Args []string
	// Retries is how many extra attempts the Manager makes (RetryEvery apart) when the command
	// fails; used to wait for objects that k3s creates asynchronously.
	Retries int
}

// RetryEvery is the pause between retries.
const RetryEvery = 5 * time.Second

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
	return Cmd{Name: "kubectl", Args: append([]string{"--context", "k3d-" + c.Name}, args...)}
}

// CreatePlan creates the cluster and applies the base manifests.
func (c Config) CreatePlan() Plan {
	return append(Plan{
		{Name: "k3d", Args: []string{"cluster", "create", c.Name, "--config", c.K3dConfig}},
		c.kubectl("wait", "--for=condition=Ready", "node", "--all", "--timeout=180s"),
	}, c.ApplyPlan()...)
}

// StartPlan restarts a stopped cluster and waits for its nodes, then re-applies the manifests.
func (c Config) StartPlan() Plan {
	return append(Plan{
		{Name: "k3d", Args: []string{"cluster", "start", c.Name}},
		c.kubectl("wait", "--for=condition=Ready", "node", "--all", "--timeout=180s"),
	}, c.ApplyPlan()...)
}

// ApplyPlan applies the base manifests to an existing cluster and reloads CoreDNS so the
// coredns-custom ConfigMap takes effect.
func (c Config) ApplyPlan() Plan {
	return Plan{
		// k3s's helm-controller registers this CRD asynchronously after the node is Ready, and
		// `kubectl wait` fails immediately on a missing object, so poll for it first.
		withRetries(c.kubectl("get", "crd/helmchartconfigs.helm.cattle.io"), 36),
		c.kubectl("wait", "--for=condition=Established", "crd/helmchartconfigs.helm.cattle.io", "--timeout=180s"),
		c.kubectl("apply", "-k", c.ManifestsDir),
		c.kubectl("-n", "kube-system", "rollout", "restart", "deployment/coredns"),
		c.kubectl("-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=120s"),
	}
}

func withRetries(c Cmd, n int) Cmd { c.Retries = n; return c }

// DeletePlan removes the cluster.
func (c Config) DeletePlan() Plan {
	return Plan{{Name: "k3d", Args: []string{"cluster", "delete", c.Name}}}
}

// StatusPlan queries the cluster.
func (c Config) StatusPlan() Plan {
	return Plan{{Name: "k3d", Args: []string{"cluster", "list", c.Name, "--output", "json"}}}
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
	cfg   Config
	ex    Executor
	sleep func(time.Duration)
}

// New returns a Manager.
func New(cfg Config, ex Executor) *Manager { return &Manager{cfg: cfg, ex: ex, sleep: time.Sleep} }

func (m *Manager) runPlan(ctx context.Context, p Plan) error {
	for _, c := range p {
		for attempt := 0; ; attempt++ {
			_, err := m.ex.Run(ctx, c)
			if err == nil {
				break
			}
			if attempt >= c.Retries || ctx.Err() != nil {
				return fmt.Errorf("%s: %w", c, err)
			}
			m.sleep(RetryEvery)
		}
	}
	return nil
}

// Status reports whether the cluster exists and is running. k3d exits non-zero with "No
// nodes found" for an unknown cluster, which means absent; any other failure (docker not
// running, k3d missing) and unparseable output are errors.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	out, err := m.ex.Run(ctx, m.cfg.StatusPlan()[0])
	if err != nil {
		if isNotFound(err) {
			return Status{}, nil
		}
		return Status{}, fmt.Errorf("%s: %w", m.cfg.StatusPlan()[0], err)
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

func isNotFound(err error) bool {
	m := strings.ToLower(err.Error())
	// Only k3d's own message: "not found" would also match `executable file not found in $PATH`.
	return strings.Contains(m, "no nodes found")
}

// Up creates the cluster if it does not exist, starts it if it is stopped, then
// (re)applies the base manifests.
func (m *Manager) Up(ctx context.Context) error {
	if err := m.cfg.Validate(); err != nil {
		return err
	}
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	switch {
	case st.Running:
		return m.runPlan(ctx, m.cfg.ApplyPlan())
	case st.Exists:
		return m.runPlan(ctx, m.cfg.StartPlan())
	}
	return m.runPlan(ctx, m.cfg.CreatePlan())
}

// Down deletes the cluster. Deleting an absent cluster is a successful no-op.
func (m *Manager) Down(ctx context.Context) error {
	if err := m.cfg.Validate(); err != nil {
		return err
	}
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Exists {
		return nil
	}
	return m.runPlan(ctx, m.cfg.DeletePlan())
}

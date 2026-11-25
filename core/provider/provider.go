// Package provider implements server providers, bootstrap, DNS helpers and the
// SSH tunnel used by `can up|down|status` (epic E06).
//
// Nothing in this package talks to a real server in tests: SSH goes through the
// Runner interface, the Hetzner API through an injectable HTTP client.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrNotImplemented marks provider types that are accepted by company.yaml but
// are handled by another epic. The CLI maps it to exit code 3.
var ErrNotImplemented = errors.New("not implemented")

// NotImplemented wraps ErrNotImplemented with the epic that delivers the feature.
func NotImplemented(what, epic string) error {
	return fmt.Errorf("%s is %w here (planned in epic %s)", what, ErrNotImplemented, epic)
}

// Spec is what a provider needs to know about the company.
type Spec struct {
	Dir     string // company directory (holds company.yaml and .can/)
	Name    string
	Domain  string
	Options map[string]any
}

// Action is one planned step. Done is true when state shows it already happened.
type Action struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Done        bool   `json:"done"`
}

// Phase is the coarse lifecycle position reported by Status.
type Phase string

// Lifecycle phases.
const (
	PhaseAbsent       Phase = "absent"
	PhaseProvisioning Phase = "provisioning"
	PhaseRunning      Phase = "running"
	PhaseReady        Phase = "ready"
	PhaseDegraded     Phase = "degraded"
)

// Check is one line of a status or preflight report.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Status is the result of Provider.Status.
type Status struct {
	Phase  Phase   `json:"phase"`
	Checks []Check `json:"checks,omitempty"`
}

// Provider provisions and tears down the place the company runs.
// Plan, Status and Address never change anything. Create and Destroy are
// idempotent: running them twice converges on the same result.
type Provider interface {
	Name() string
	Plan(ctx context.Context, spec Spec, st *Store) ([]Action, error)
	Create(ctx context.Context, spec Spec, st *Store, out io.Writer) error
	Destroy(ctx context.Context, spec Spec, st *Store, out io.Writer) error
	Status(ctx context.Context, spec Spec, st *Store) (Status, error)
	// Address returns the public IP (or host) of the server, from state.
	Address(st *Store) (string, error)
}

// Deps are the seams tests replace.
type Deps struct {
	// Runner returns a command runner for the given SSH target.
	Runner func(Target) Runner
	// HTTP is the client used for provider APIs.
	HTTP *http.Client
	// Getenv reads environment variables (default os.Getenv).
	Getenv func(string) string
	// ReadFile reads files such as keys and secret refs (default os.ReadFile).
	ReadFile func(string) ([]byte, error)
	// Sleep waits between polls and must honour ctx (default: real sleep).
	Sleep func(ctx context.Context, d time.Duration)
	// Redactor learns every secret the providers resolve.
	Redactor *Redactor
	// APIBase overrides the Hetzner API base URL (tests).
	APIBase string
}

func (d *Deps) defaults() {
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.ReadFile == nil {
		d.ReadFile = os.ReadFile
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if d.Runner == nil {
		d.Runner = func(t Target) Runner { return NewSSHRunner(t) }
	}
	if d.Sleep == nil {
		d.Sleep = func(ctx context.Context, dur time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(dur):
			}
		}
	}
	if d.Redactor == nil {
		d.Redactor = NewRedactor()
	}
}

// Types lists every provider type accepted by company.yaml, sorted.
func Types() []string {
	t := []string{"local", "ssh", "hetzner", "digitalocean", "static"}
	sort.Strings(t)
	return t
}

// New builds the provider for company.yaml's provider.type.
func New(typ string, deps Deps) (Provider, error) {
	deps.defaults()
	switch typ {
	case "ssh":
		return &SSH{deps: deps}, nil
	case "hetzner":
		return &Hetzner{deps: deps}, nil
	case "local":
		return stub{name: "local", epic: "E02", what: "the local provider (k3d via `can dev`)"}, nil
	case "static":
		return stub{name: "static", epic: "E18", what: "the static provider"}, nil
	case "digitalocean":
		return stub{name: "digitalocean", epic: "E12", what: "the digitalocean provider"}, nil
	}
	return nil, fmt.Errorf("unknown provider type %q (known: %v)", typ, Types())
}

// StatePath is where provider state lives for a company directory.
func StatePath(dir string) string { return filepath.Join(dir, ".can", "state.json") }

// stub is a provider whose real work belongs to another epic.
type stub struct{ name, epic, what string }

func (s stub) err() error { return NotImplemented(s.what, s.epic) }

func (s stub) Name() string { return s.name }
func (s stub) Plan(context.Context, Spec, *Store) ([]Action, error) {
	return nil, s.err()
}
func (s stub) Create(context.Context, Spec, *Store, io.Writer) error  { return s.err() }
func (s stub) Destroy(context.Context, Spec, *Store, io.Writer) error { return s.err() }
func (s stub) Status(context.Context, Spec, *Store) (Status, error)   { return Status{}, s.err() }
func (s stub) Address(*Store) (string, error)                         { return "", s.err() }

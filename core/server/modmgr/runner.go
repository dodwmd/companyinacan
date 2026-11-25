package modmgr

import (
	"context"
	"strings"
	"sync"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// Reconciling is what the service needs from the reconciler: a plan, and an apply that never runs
// concurrently with another apply (the periodic loop and the UI share one Runner).
type Reconciling interface {
	Plan(ctx context.Context, d reconcile.Desired) ([]reconcile.Action, error)
	Apply(ctx context.Context, d reconcile.Desired) (reconcile.Result, error)
}

// Runner serialises applies over one reconcile.Reconciler.
type Runner struct {
	r  *reconcile.Reconciler
	mu sync.Mutex
}

// NewRunner wraps r.
func NewRunner(r *reconcile.Reconciler) *Runner { return &Runner{r: r} }

// Plan implements Reconciling.
func (x *Runner) Plan(ctx context.Context, d reconcile.Desired) ([]reconcile.Action, error) {
	return x.r.Plan(ctx, d)
}

// Apply implements Reconciling; it waits for a running apply to finish.
func (x *Runner) Apply(ctx context.Context, d reconcile.Desired) (reconcile.Result, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.r.Apply(ctx, d, reconcile.Options{})
}

// SecretWriter writes a Secret (the email relay credentials). The password never goes anywhere else.
type SecretWriter interface {
	Put(ctx context.Context, namespace, name string, data map[string]string) error
}

// KubeSecrets writes Secrets through the Kubernetes API.
type KubeSecrets struct{ API kube.API }

// Put implements SecretWriter (server-side apply of an Opaque Secret).
func (k KubeSecrets) Put(ctx context.Context, namespace, name string, data map[string]string) error {
	sd := map[string]any{}
	for key, v := range data {
		sd[key] = v
	}
	return k.API.Apply(ctx, kube.Object{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "stringData": sd,
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"app.kubernetes.io/managed-by": "can-core"}},
	})
}

// FakeSecrets is an in-memory SecretWriter for tests.
type FakeSecrets struct {
	mu   sync.Mutex
	Data map[string]map[string]string
	Err  error
}

// NewFakeSecrets returns an empty fake.
func NewFakeSecrets() *FakeSecrets { return &FakeSecrets{Data: map[string]map[string]string{}} }

// Put implements SecretWriter.
func (f *FakeSecrets) Put(_ context.Context, namespace, name string, data map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	cp := map[string]string{}
	for k, v := range data {
		cp[k] = v
	}
	f.Data[namespace+"/"+name] = cp
	return nil
}

// Get returns a stored Secret's data.
func (f *FakeSecrets) Get(namespace, name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Data[namespace+"/"+name]
}

// NetInfo reports the server's public addresses (for the DNS step). Fake in tests.
type NetInfo interface {
	PublicIPs(ctx context.Context) (v4, v6 []string, err error)
}

// StaticNet is a NetInfo with fixed addresses (can-core learns the node address from CAN_NODE_IP).
type StaticNet struct{ V4, V6 []string }

// PublicIPs implements NetInfo.
func (n StaticNet) PublicIPs(context.Context) ([]string, []string, error) { return n.V4, n.V6, nil }

// Instant URL states.
const (
	InstantServing     = "serving"
	InstantUnreachable = "unreachable"
	InstantUnknown     = "unknown"
)

// InstantProbe checks whether the sslip.io instant URL answers (fake in tests; the real probe is
// an outbound HTTPS request and is not part of this epic's verification).
type InstantProbe interface {
	Probe(ctx context.Context, url string) string
}

// instantHost is the sslip.io name of an IPv4 address.
func instantHost(ip string) string { return strings.ReplaceAll(ip, ".", "-") + ".sslip.io" }

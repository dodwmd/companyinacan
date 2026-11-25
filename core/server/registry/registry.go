// Package registry is the capability registry (SPEC §4.3, §4.5): modules register what their
// manifest provides, consumers resolve a capability to the current provider(s) and then call
// them directly. can-core never proxies capability calls.
package registry

import (
	"context"
	"errors"
	"fmt"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/validate"
)

// Errors from Resolve.
var (
	ErrNoProvider = errors.New("no enabled module provides this capability")
	ErrAmbiguous  = errors.New("several modules provide this exclusive capability and none is designated primary")
)

// CoreID is the provider id of capabilities that can-core itself provides (email.send, events, ...).
const CoreID = "can-core"

// Provider is one resolvable provider.
type Provider struct {
	ModuleID string `json:"module"`
	Service  string `json:"service,omitempty"`
	Port     int    `json:"port,omitempty"`
	Schema   string `json:"schema,omitempty"`
	URL      string `json:"url"`
}

// Resolution is the answer to "who provides capability X?".
type Resolution struct {
	Capability string     `json:"capability"`
	Aggregate  bool       `json:"aggregate"`
	Platform   bool       `json:"platform"`
	Providers  []Provider `json:"providers"`
}

// Registry resolves capabilities from the store.
type Registry struct {
	db          *store.DB
	platformURL string
}

// New returns a registry. platformURL is where can-core's own module API is reachable from modules.
func New(db *store.DB, platformURL string) *Registry {
	return &Registry{db: db, platformURL: platformURL}
}

// ServiceURL is the in-cluster URL of a module Service.
func ServiceURL(namespace, service string, port int) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service, namespace, port)
}

// FromManifest derives the provider and subscription rows of a manifest. A subscription without
// `service` defaults to the module id; without `port`, to the port of its first provision, else 80.
func FromManifest(m *config.Module) ([]store.Provider, []store.Subscription) {
	var ps []store.Provider
	for _, p := range m.Provides {
		svc := p.Service
		if svc == "" {
			svc = m.ID
		}
		ps = append(ps, store.Provider{Capability: p.Capability, ModuleID: m.ID, Service: svc, Port: p.Port, Schema: p.Schema})
	}
	defPort := 80
	if len(m.Provides) > 0 && m.Provides[0].Port != 0 {
		defPort = m.Provides[0].Port
	}
	var ss []store.Subscription
	for _, s := range m.Subscribes {
		svc, port := s.Service, s.Port
		if svc == "" {
			svc = m.ID
		}
		if port == 0 {
			port = defPort
		}
		ss = append(ss, store.Subscription{ModuleID: m.ID, Event: s.Event, Service: svc, Port: port, Webhook: s.Webhook})
	}
	return ps, ss
}

// Declares reports whether the manifest lists the capability under requires.capabilities or consumes.
func Declares(m *config.Module, capability string) bool {
	for _, c := range m.Requires.Capabilities {
		if c == capability {
			return true
		}
	}
	for _, c := range m.Consumes {
		if c == capability {
			return true
		}
	}
	return false
}

// Register records the module with its provisions and subscriptions derived from its manifest,
// replacing any earlier registration of the same module.
func (r *Registry) Register(ctx context.Context, m store.Module) error {
	ps, ss := FromManifest(&m.Manifest)
	return r.db.UpsertModule(ctx, m, ps, ss)
}

// SetPrimaries stores the operator's `primary:` choices.
func (r *Registry) SetPrimaries(ctx context.Context, primaries map[string]string) error {
	return r.db.SetPrimaries(ctx, primaries)
}

// Resolve returns the provider(s) of a capability.
func (r *Registry) Resolve(ctx context.Context, capability string) (Resolution, error) {
	known := validate.Capabilities()[capability]
	res := Resolution{Capability: capability, Aggregate: known.Aggregate, Platform: known.Platform}
	if known.Platform {
		res.Providers = []Provider{{ModuleID: CoreID, URL: r.platformURL}}
		return res, nil
	}
	ps, err := r.db.Providers(ctx, capability)
	if err != nil {
		return res, err
	}
	if len(ps) == 0 {
		return res, ErrNoProvider
	}
	if !known.Aggregate && len(ps) > 1 {
		primary, err := r.db.Primary(ctx, capability)
		if err != nil {
			return res, err
		}
		var pick []store.Provider
		for _, p := range ps {
			if p.ModuleID == primary {
				pick = []store.Provider{p}
			}
		}
		if pick == nil {
			return res, ErrAmbiguous
		}
		ps = pick
	}
	for _, p := range ps {
		res.Providers = append(res.Providers, Provider{ModuleID: p.ModuleID, Service: p.Service, Port: p.Port, Schema: p.Schema, URL: ServiceURL(p.Namespace, p.Service, p.Port)})
	}
	return res, nil
}

// List resolves every capability that currently has an enabled provider (admin view).
func (r *Registry) List(ctx context.Context) ([]Resolution, error) {
	all, err := r.db.AllProviders(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Resolution
	for _, p := range all {
		if seen[p.Capability] {
			continue
		}
		seen[p.Capability] = true
		res, err := r.Resolve(ctx, p.Capability)
		if err != nil && !errors.Is(err, ErrAmbiguous) {
			return nil, err
		}
		if errors.Is(err, ErrAmbiguous) { // show every contender
			res.Providers = nil
			for _, q := range all {
				if q.Capability == p.Capability {
					res.Providers = append(res.Providers, Provider{ModuleID: q.ModuleID, Service: q.Service, Port: q.Port, Schema: q.Schema, URL: ServiceURL(q.Namespace, q.Service, q.Port)})
				}
			}
		}
		out = append(out, res)
	}
	return out, nil
}

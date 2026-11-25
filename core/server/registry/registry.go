// Package registry is the capability registry (SPEC §4.3, §4.5): modules register what their
// manifest provides, consumers resolve a capability to the current provider(s) and then call
// them directly. can-core never proxies capability calls.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"

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

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ErrBadService is returned for a service, namespace or port that cannot form a cluster-internal URL.
var ErrBadService = errors.New("invalid service address")

// ServiceURL is the in-cluster URL of a module Service. Service and namespace must be DNS-1123
// labels; the URL is built with net/url and must round-trip to exactly the intended host, so a
// manifest value like "169.254.169.254#" or "evil.com/" can never redirect a signed webhook or a
// consumer call (SSRF).
func ServiceURL(namespace, service string, port int) (string, error) {
	if !dnsLabel.MatchString(namespace) || !dnsLabel.MatchString(service) || port < 1 || port > 65535 {
		return "", fmt.Errorf("%w: %q.%q:%d", ErrBadService, service, namespace, port)
	}
	host := fmt.Sprintf("%s.%s.svc.cluster.local:%d", service, namespace, port)
	u := url.URL{Scheme: "http", Host: host}
	back, err := url.Parse(u.String())
	if err != nil || back.Host != host || back.User != nil || back.Path != "" || back.RawQuery != "" || back.Fragment != "" {
		return "", fmt.Errorf("%w: %q does not round-trip", ErrBadService, host)
	}
	return u.String(), nil
}

// FromManifest derives the provider and subscription rows of a manifest. A subscription without
// `service` defaults to the module id; without `port`, to the port of its first provision, else 80.
// Every service must be a valid DNS label; otherwise the manifest is rejected.
func FromManifest(m *config.Module) ([]store.Provider, []store.Subscription, error) {
	var ps []store.Provider
	for _, p := range m.Provides {
		svc := p.Service
		if svc == "" {
			svc = m.ID
		}
		if _, err := ServiceURL("mod-"+m.ID, svc, p.Port); err != nil {
			return nil, nil, fmt.Errorf("provides %s: %w", p.Capability, err)
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
		if _, err := ServiceURL("mod-"+m.ID, svc, port); err != nil {
			return nil, nil, fmt.Errorf("subscribes %s: %w", s.Event, err)
		}
		ss = append(ss, store.Subscription{ModuleID: m.ID, Event: s.Event, Service: svc, Port: port, Webhook: s.Webhook})
	}
	return ps, ss, nil
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
	ps, ss, err := FromManifest(&m.Manifest)
	if err != nil {
		return err
	}
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
		u, err := ServiceURL(p.Namespace, p.Service, p.Port)
		if err != nil {
			continue // never hand out an address that failed validation
		}
		res.Providers = append(res.Providers, Provider{ModuleID: p.ModuleID, Service: p.Service, Port: p.Port, Schema: p.Schema, URL: u})
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
					if u, err := ServiceURL(q.Namespace, q.Service, q.Port); err == nil {
						res.Providers = append(res.Providers, Provider{ModuleID: q.ModuleID, Service: q.Service, Port: q.Port, Schema: q.Schema, URL: u})
					}
				}
			}
		}
		out = append(out, res)
	}
	return out, nil
}

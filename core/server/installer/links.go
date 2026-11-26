package installer

// Module-to-module connections (specs/056-module-capability-wiring): two NetworkPolicies, one on
// each side, the provider's address in the consumer's configuration and a scoped credential copied
// into the consumer's Secret. deploy/base/admission-policy.yaml (can-core-networkpolicies) refuses
// any other shape.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

const (
	// wirePrefix names the NetworkPolicies of connections. A chart may not use it.
	wirePrefix = "can-wire-"
	labelWire  = "can.dev/wire"      // egress (consumer side) or ingress (provider side)
	labelPeer  = "can.dev/wire-peer" // the module on the other side
)

// linkValues are the chart value overrides of the connections: where each provider is.
func linkValues(s reconcile.Spec) (map[string]any, error) {
	if len(s.Links) == 0 {
		return nil, nil
	}
	conf := map[string]any{}
	for _, l := range s.Links {
		g, ok := modsecrets.GrantFor(s.ID, l.Provider)
		if !ok || !idRE.MatchString(l.Provider) || !idRE.MatchString(l.Service) || l.Port < 1 || l.Port > 65535 {
			return nil, fmt.Errorf("module %q may not be connected to %q", s.ID, l.Provider)
		}
		conf[g.ConfigKey] = map[string]any{
			"url":                 fmt.Sprintf("http://%s.mod-%s.svc.cluster.local:%d", l.Service, l.Provider, l.Port),
			"allow_insecure_http": true, // in the cluster, between two NetworkPolicies
		}
	}
	return map[string]any{"config": conf}, nil
}

func linked(s reconcile.Spec, provider string) bool {
	return slices.ContainsFunc(s.Links, func(l config.Link) bool { return l.Provider == provider })
}

// wireSecrets copies into a module's Secret the credentials of the modules it is connected to, and
// returns the keys it must no longer have. A value that is already there is kept when the
// provider's Secret cannot supply one (a restore).
func (in *Installer) wireSecrets(ctx context.Context, s reconcile.Spec, data map[string]string) (drop []string, err error) {
	for _, g := range modsecrets.Grants() {
		if g.Consumer != s.ID {
			continue
		}
		have, err := in.secretKey(ctx, s.Namespace, secretModule, g.ConsumerKey)
		if err != nil {
			return nil, err
		}
		link, required := linked(s, g.Provider), slices.Contains(s.Manifest.Requires.Capabilities, g.Capability)
		var tok string
		if link {
			if tok, err = in.secretKey(ctx, "mod-"+g.Provider, secretModule, g.ProviderKey); err != nil {
				return nil, err
			}
		}
		// A module that cannot run without the capability keeps the token it has (the network
		// access is removed all the same); a restore keeps what it seeded.
		if tok == "" && (link || required || in.cfg.Quiesce) {
			tok = have
		}
		switch {
		case tok != "":
			data[g.ConsumerKey] = tok
		case link || required:
			return nil, fmt.Errorf("module %s uses the %s capability: enable the %s module (it is installed first, and creates %s)", s.ID, g.Capability, g.Provider, g.ProviderKey)
		case have != "":
			drop = append(drop, g.ConsumerKey)
		}
	}
	return drop, nil
}

// dropSecretKeys removes keys from the module Secret (an apply alone does not).
func (in *Installer) dropSecretKeys(ctx context.Context, id string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	data := map[string]any{}
	for _, k := range keys {
		data[k] = nil
	}
	err := in.cfg.API.MergePatch(ctx, "Secret", "mod-"+id, secretModule, kube.Object{"data": data})
	if err != nil && !kube.IsNotFound(err) {
		return fmt.Errorf("removing %s from Secret %s: %w", strings.Join(keys, ", "), secretModule, err)
	}
	return nil
}

// WiringFingerprint implements reconcile.WiringAware: a digest of the credentials the module would
// be given for Spec.Links, "" while there is none.
func (in *Installer) WiringFingerprint(ctx context.Context, s reconcile.Spec) (string, error) {
	toks := map[string]string{}
	for _, l := range s.Links {
		g, ok := modsecrets.GrantFor(s.ID, l.Provider)
		if !ok {
			continue
		}
		tok, err := in.secretKey(ctx, "mod-"+g.Provider, secretModule, g.ProviderKey)
		if err != nil {
			return "", err
		}
		if tok != "" {
			toks[g.ProviderKey] = tok
		}
	}
	if len(toks) == 0 {
		return "", nil
	}
	return secretVersion(s.HMACSecret, toks), nil
}

// wirePolicy is one side of a connection: the owner's pods to (egress) or from (ingress) the
// peer's pods, on one port.
func wirePolicy(name, owner, peer, direction string, port int) kube.Object {
	pods := func(id string) map[string]any {
		return map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": id}}
	}
	rule := map[string]any{"ports": []any{map[string]any{"protocol": "TCP", "port": port}}}
	peers := []any{map[string]any{
		"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "mod-" + peer}},
		"podSelector":       pods(peer),
	}}
	spec := map[string]any{"podSelector": pods(owner)}
	if direction == "egress" {
		rule["to"] = peers
		spec["policyTypes"], spec["egress"] = []any{"Egress"}, []any{rule}
	} else {
		rule["from"] = peers
		spec["policyTypes"], spec["ingress"] = []any{"Ingress"}, []any{rule}
	}
	o := kube.Object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": name, "namespace": "mod-" + owner}, "spec": spec}
	setLabels(o, map[string]string{labelManaged: "can-core", labelModule: owner, labelWire: direction, labelPeer: peer})
	return o
}

// connect makes the network connections of Spec.Links and removes those the module no longer has.
func (in *Installer) connect(ctx context.Context, s reconcile.Spec) error {
	have, err := in.cfg.API.List(ctx, "NetworkPolicy", s.Namespace, labelWire+"=egress,"+labelManaged+"=can-core")
	if err != nil {
		return fmt.Errorf("listing the module's connections: %w", err)
	}
	for _, o := range have {
		peer, _ := asMap(asMap(o["metadata"])["labels"])[labelPeer].(string)
		if linked(s, peer) || !idRE.MatchString(peer) {
			continue
		}
		if err := in.disconnect(ctx, s.ID, peer); err != nil {
			return err
		}
	}
	for _, l := range s.Links {
		// The provider's side first: without it the consumer's rule opens nothing.
		for _, o := range []kube.Object{
			wirePolicy(wirePrefix+"from-"+s.ID, l.Provider, s.ID, "ingress", l.Port),
			wirePolicy(wirePrefix+"to-"+l.Provider, s.ID, l.Provider, "egress", l.Port),
		} {
			if err := in.cfg.API.Apply(ctx, o); err != nil {
				return fmt.Errorf("connecting %s to %s: NetworkPolicy %s/%s: %w", s.ID, l.Provider, nsOf(o), nameOf(o), err)
			}
		}
	}
	return nil
}

// disconnect removes both sides of the connection from consumer to provider.
func (in *Installer) disconnect(ctx context.Context, consumer, provider string) error {
	for _, p := range [][2]string{{provider, wirePrefix + "from-" + consumer}, {consumer, wirePrefix + "to-" + provider}} {
		if err := in.cfg.API.Delete(ctx, "NetworkPolicy", "mod-"+p[0], p[1]); err != nil && !kube.IsNotFound(err) {
			return fmt.Errorf("disconnecting %s from %s: %w", consumer, provider, err)
		}
	}
	return nil
}

// disconnectAll removes every connection of a module that is being disabled, whichever side it is
// on, and the credentials it was given as a consumer.
func (in *Installer) disconnectAll(ctx context.Context, id string) error {
	have, err := in.cfg.API.List(ctx, "NetworkPolicy", "mod-"+id, labelManaged+"=can-core")
	if err != nil {
		return fmt.Errorf("listing the module's connections: %w", err)
	}
	for _, o := range have {
		l := asMap(asMap(o["metadata"])["labels"])
		peer, _ := l[labelPeer].(string)
		if !idRE.MatchString(peer) {
			continue
		}
		consumer, provider := id, peer
		if l[labelWire] == "ingress" {
			consumer, provider = peer, id
		}
		if err := in.disconnect(ctx, consumer, provider); err != nil {
			return err
		}
	}
	var keys []string
	for _, g := range modsecrets.Grants() {
		if g.Consumer != id {
			continue
		}
		v, err := in.secretKey(ctx, "mod-"+id, secretModule, g.ConsumerKey)
		if err != nil {
			return err
		}
		if v != "" {
			keys = append(keys, g.ConsumerKey)
		}
	}
	return in.dropSecretKeys(ctx, id, keys)
}

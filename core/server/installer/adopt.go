package installer

import (
	"context"
	"fmt"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// verifyAdopted checks a Namespace or RoleBinding that already existed (Create returned a conflict):
// an attacker-prepared or stale object must not be adopted silently.
func (in *Installer) verifyAdopted(ctx context.Context, want kube.Object, s reconcile.Spec) error {
	kind, name, ns := fmt.Sprint(want["kind"]), nameOf(want), nsOf(want)
	have, err := in.cfg.API.Get(ctx, kind, ns, name)
	if err != nil {
		return fmt.Errorf("%s %s exists but cannot be read: %w", kind, name, err)
	}
	switch kind {
	case "Namespace":
		l := asMap(asMap(have["metadata"])["labels"])
		if l["pod-security.kubernetes.io/enforce"] != "restricted" || l["can.dev/namespace-kind"] != "module" ||
			l[labelManaged] != "can-core" || l[labelModule] != s.ID {
			return fmt.Errorf("namespace %s exists but is not a can-core module namespace for %q (labels %v); refusing to adopt it", name, s.ID, l)
		}
	case "RoleBinding":
		rr := asMap(have["roleRef"])
		subs := asList(have["subjects"])
		var sub map[string]any
		if len(subs) == 1 {
			sub = asMap(subs[0])
		}
		if rr["kind"] != "ClusterRole" || rr["name"] != "can-core-module" || sub == nil ||
			sub["kind"] != "ServiceAccount" || sub["name"] != "can-core" || sub["namespace"] != "can-system" {
			return fmt.Errorf("RoleBinding %s/%s exists with a different roleRef or subjects; refusing to adopt it", ns, name)
		}
	}
	return nil
}

// checkAdmission verifies, when RequireAdmission is set, that the can-core admission policies and
// Deny bindings exist. Without them can-core's standing RBAC lets it bind into any namespace, and,
// for a module with routes, claim any host.
func (in *Installer) checkAdmission(ctx context.Context, routed bool) error {
	if !in.cfg.RequireAdmission {
		return nil
	}
	names := []string{"can-core-rolebindings", "can-core-namespaces"}
	if routed {
		names = append(names, "can-core-routes", "can-core-routing-config", "can-core-services")
	}
	for _, n := range names {
		if _, err := in.cfg.API.Get(ctx, "ValidatingAdmissionPolicy", "", n); err != nil {
			return fmt.Errorf("refusing to install: admission policy %s not found (%w); apply deploy/base", n, err)
		}
		b, err := in.cfg.API.Get(ctx, "ValidatingAdmissionPolicyBinding", "", n)
		if err != nil {
			return fmt.Errorf("refusing to create module namespaces: admission policy binding %s not found (%w); apply deploy/base", n, err)
		}
		deny := false
		for _, a := range asList(asMap(b["spec"])["validationActions"]) {
			deny = deny || a == "Deny"
		}
		if !deny {
			return fmt.Errorf("refusing to create module namespaces: binding %s does not Deny", n)
		}
	}
	return nil
}

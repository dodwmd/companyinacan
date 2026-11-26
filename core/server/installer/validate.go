package installer

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// chartKinds are the only kinds a module chart may contain. Everything else is owned by can-core:
// Namespace, RoleBinding, ServiceAccount, ResourceQuota, LimitRange, Ingress and Middleware
// (routing is validated below for IngressRoute only), admission and RBAC objects, bare Pods.
var chartKinds = map[string]bool{
	"ConfigMap": true, "Secret": true, "PersistentVolumeClaim": true, "Service": true,
	"Deployment": true, "StatefulSet": true, "Job": true, "CronJob": true,
	"NetworkPolicy": true, "IngressRoute": true,
}

var (
	dnsRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// hostFor is the one public hostname a module's chart may route: <subdomain>.<domain>, where the
// subdomain is one the module may claim (core/routing).
func (in *Installer) hostFor(s reconcile.Spec) string {
	domain := s.Domain
	if domain == "" {
		domain = in.cfg.Domain
	}
	if s.Manifest == nil || domain == "" || !routing.MayClaim(s.ID, s.Manifest.Runtime.Subdomain) {
		return ""
	}
	return s.Manifest.Runtime.Subdomain + "." + domain
}

// checkObject enforces what a chart object may be. The pod-template rules are in hardenWorkload.
func (in *Installer) checkObject(o kube.Object, s reconcile.Spec) error {
	kind, _ := o["kind"].(string)
	name := nameOf(o)
	if !chartKinds[kind] {
		return fmt.Errorf("chart may not contain %s %s: can-core owns that kind", kind, name)
	}
	if !dnsRE.MatchString(name) {
		return fmt.Errorf("%s name %q is not a DNS-1123 name", kind, name)
	}
	spec, _ := o["spec"].(map[string]any)
	md, _ := o["metadata"].(map[string]any)
	if av, _ := o["apiVersion"].(string); av != kube.APIVersion(kind) {
		return fmt.Errorf("%s %s: apiVersion %q is not %q", kind, name, av, kube.APIVersion(kind))
	}
	for _, f := range []string{"ownerReferences", "finalizers", "generateName", "deletionTimestamp"} {
		if _, ok := md[f]; ok {
			return fmt.Errorf("%s %s: metadata.%s is not allowed", kind, name, f)
		}
	}
	switch kind {
	case "PersistentVolumeClaim":
		if err := checkPVCSpec("persistentvolumeclaim "+name, spec); err != nil {
			return err
		}
	case "Secret":
		if t, _ := o["type"].(string); t != "" && t != "Opaque" {
			return fmt.Errorf("secret %s: type %q is not allowed (only Opaque)", name, t)
		}
	case "Service":
		if t, _ := spec["type"].(string); t != "" && t != "ClusterIP" {
			return fmt.Errorf("service %s: type %q is not allowed (only ClusterIP; routing goes through can-core's Traefik route)", name, t)
		}
		for _, f := range []string{"externalIPs", "loadBalancerIP", "loadBalancerSourceRanges", "externalName", "clusterIP"} {
			if _, ok := spec[f]; ok {
				return fmt.Errorf("service %s: %s is not allowed", name, f)
			}
		}
		for _, p := range asList(spec["ports"]) {
			if pm, _ := p.(map[string]any); pm["nodePort"] != nil {
				return fmt.Errorf("service %s: nodePort is not allowed", name)
			}
		}
	case "NetworkPolicy":
		if err := checkNetworkPolicy(name, spec); err != nil {
			return err
		}
	case "IngressRoute":
		if err := in.checkIngressRoute(name, spec, s); err != nil {
			return err
		}
	}
	for k := range asMap(md["annotations"]) {
		if (kind == "Secret") && strings.Contains(k, "kubernetes.io/") {
			return fmt.Errorf("secret %s: annotation %q is reserved (service-account tokens cannot be minted by a chart)", name, k)
		}
	}
	return nil
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// checkNetworkPolicy allows exactly one shape: pods of this namespace talking to each other.
// Every peer must be a bare podSelector (no namespaceSelector, no ipBlock) and the policy may not
// select all pods with an allow-everything rule. NetworkPolicies are additive, so anything broader
// would void the namespace's default-deny.
func checkNetworkPolicy(name string, spec map[string]any) error {
	if len(asMap(spec["podSelector"])) == 0 {
		return fmt.Errorf("networkpolicy %s: podSelector must select specific pods (matchLabels)", name)
	}
	for k := range spec {
		if k != "podSelector" && k != "policyTypes" && k != "ingress" && k != "egress" {
			return fmt.Errorf("networkpolicy %s: unexpected field %q", name, k)
		}
	}
	for _, dir := range []string{"ingress", "egress"} {
		peers := "from"
		if dir == "egress" {
			peers = "to"
		}
		for _, r := range asList(spec[dir]) {
			rule := asMap(r)
			for k := range rule {
				if k != peers {
					return fmt.Errorf("networkpolicy %s: %s rules may only list %s peers (no ports, no allow-all)", name, dir, peers)
				}
			}
			list := asList(rule[peers])
			if len(list) == 0 {
				return fmt.Errorf("networkpolicy %s: an %s rule without peers allows everything", name, dir)
			}
			for _, p := range list {
				peer := asMap(p)
				if len(asMap(peer["podSelector"])) == 0 {
					return fmt.Errorf("networkpolicy %s: %s peers must be a podSelector with labels", name, dir)
				}
				for k := range peer {
					if k != "podSelector" {
						return fmt.Errorf("networkpolicy %s: %s peer field %q is not allowed (same-namespace pods only)", name, dir, k)
					}
				}
			}
		}
	}
	return nil
}

// checkIngressRoute allows only routes for the module's own host and only to Services in its own
// namespace. Middlewares and TLS options may not reference other namespaces either.
func (in *Installer) checkIngressRoute(name string, spec map[string]any, s reconcile.Spec) error {
	host := in.hostFor(s)
	if host == "" {
		return fmt.Errorf("ingressroute %s: the module declares no runtime.subdomain it may claim, or can-core has no company domain (CAN_COMPANY_DOMAIN); routing is refused", name)
	}
	for _, ep := range asList(spec["entryPoints"]) {
		if ep != "web" && ep != "websecure" {
			return fmt.Errorf("ingressroute %s: entryPoint %v is not allowed (web, websecure)", name, ep)
		}
	}
	routes := asList(spec["routes"])
	if len(routes) == 0 {
		return fmt.Errorf("ingressroute %s has no routes", name)
	}
	for _, r := range routes {
		rt := asMap(r)
		match, _ := rt["match"].(string)
		if err := checkMatch(match, host); err != nil {
			return fmt.Errorf("ingressroute %s: %w", name, err)
		}
		for _, list := range []string{"services", "middlewares"} {
			for _, e := range asList(rt[list]) {
				if n, _ := asMap(e)["name"].(string); strings.Contains(n, "@") {
					return fmt.Errorf("ingressroute %s: %s name %q refers to another provider or namespace (@)", name, list, n)
				}
				if ns, _ := asMap(e)["namespace"].(string); ns != "" && ns != s.Namespace {
					return fmt.Errorf("ingressroute %s: %s may not reference namespace %q", name, list, ns)
				}
				if k, _ := asMap(e)["kind"].(string); k != "" && k != "Service" && list == "services" {
					return fmt.Errorf("ingressroute %s: service kind %q is not allowed", name, k)
				}
			}
		}
	}
	if t := asMap(spec["tls"]); t != nil {
		for f, v := range t {
			// No domains, secretName or options: a module may not request certificates for other
			// names or pick another certificate store. Its own route Host is what gets certified.
			if f != "certResolver" || v != "letsencrypt" {
				return fmt.Errorf("ingressroute %s: tls may only set certResolver: letsencrypt (got %s=%v)", name, f, v)
			}
		}
	}
	return nil
}

package installer

// Public routes of a module (specs/040-module-routing). Charts keep their own IngressRoute for a
// module's declared public pages (runtime.subdomain, checked in guard.go); can-core itself creates
// the route of a web UI or API that company.yaml exposes, the forms route on the site's hosts, the
// security headers and the NetworkPolicy that admits the ingress controller to the routed ports
// and nothing else. None of these objects is part of a chart or of the release record.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// Names of the routing objects can-core owns in a module namespace.
const (
	routeWeb        = "can-web"
	middlewareWeb   = "can-web-headers"
	middlewareSite  = "can-site-headers"
	policyTraefik   = "allow-ingress-from-traefik"
	labelRoute      = "can.dev/route"
	annotationRoute = "can.dev/exposure"
)

func siteRouteName(id string) string { return id + "-public" }
func stripName(id string) string     { return id + "-strip" }

type backend struct {
	service string
	port    int
}

// routePlan is what a module's exposure settings and the cluster's routing facts add up to.
type routePlan struct {
	cfg      routing.Config
	uiHost   string         // set when the web UI is routed
	values   map[string]any // chart value overrides
	objects  []kube.Object  // can-core's IngressRoutes and Middlewares
	backends []backend      // the Services those routes send to
}

func (in *Installer) domain(s reconcile.Spec) string {
	if s.Domain != "" {
		return s.Domain
	}
	return in.cfg.Domain
}

// routingData reads ConfigMap can-system/can-routing; found is false when there is none.
func (in *Installer) routingData(ctx context.Context) (data map[string]string, found bool, err error) {
	o, err := in.cfg.API.Get(ctx, "ConfigMap", routing.Namespace, routing.ConfigMapName)
	if kube.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading ConfigMap %s/%s: %w", routing.Namespace, routing.ConfigMapName, err)
	}
	data = map[string]string{}
	for k, v := range asMap(o["data"]) {
		data[k], _ = v.(string)
	}
	return data, true, nil
}

// withoutFacts is the plan for a cluster that has no routing facts (installed without `can up`):
// the admission policy would refuse any route, so an exposed UI or API is an error, and a module's
// public surface is left unrouted so that the module still installs. A public route that is already
// there is left alone (the upgrade then fails on it rather than taking it offline).
func (in *Installer) withoutFacts(ctx context.Context, s reconcile.Spec, p routePlan) (routePlan, error) {
	missing := fmt.Sprintf("ConfigMap %s/%s does not exist, so nothing can be routed: run `can up`", routing.Namespace, routing.ConfigMapName)
	if s.Expose.UI || s.Expose.API {
		return p, errors.New(missing)
	}
	in.cfg.Log.Warn("not routing: "+missing, "module", s.ID)
	if s.Manifest.Runtime.Subdomain != "" {
		have, err := in.cfg.API.List(ctx, "IngressRoute", s.Namespace, labelModule+"="+s.ID+","+labelManaged+"=can-core")
		if err != nil && !kube.IsNotFound(err) && !kube.IsForbidden(err) { // a new namespace has no RoleBinding yet
			return p, fmt.Errorf("listing routes: %w", err)
		}
		if len(have) == 0 {
			p.values = deepMerge(p.values, map[string]any{"ingress": map[string]any{"enabled": false}})
		}
	}
	return p, nil
}

// routingConfig is the routing facts. In a cluster (RequireAdmission) planRoutes has already
// handled their absence; elsewhere the defaults for the domain apply.
func (in *Installer) routingConfig(ctx context.Context, s reconcile.Spec) (routing.Config, error) {
	data, found, err := in.routingData(ctx)
	switch {
	case err != nil:
		return routing.Config{}, err
	case found:
		return routing.Parse(data)
	}
	return routing.Default(in.domain(s)), nil
}

func (in *Installer) planRoutes(ctx context.Context, s reconcile.Spec) (routePlan, error) {
	var p routePlan
	m, x := s.Manifest, s.Expose
	domain := in.domain(s)
	ui, api := x.UI, x.API
	siteWanted := m.Runtime.SiteRoute != nil && x.PublicOn()
	if !x.PublicOn() && m.Runtime.Subdomain != "" {
		p.values = deepMerge(p.values, map[string]any{"ingress": map[string]any{"enabled": false}})
	}
	publicPages := m.Runtime.Subdomain != "" && x.PublicOn()
	if in.cfg.RequireAdmission && (ui || api || siteWanted || publicPages) {
		_, found, err := in.routingData(ctx)
		if err != nil {
			return p, err
		}
		if !found {
			return in.withoutFacts(ctx, s, p)
		}
	}
	if (!ui && !api && !siteWanted) || domain == "" {
		if (ui || api) && domain == "" {
			in.cfg.Log.Warn("not routing: no company domain", "module", s.ID)
		}
		return p, nil
	}
	cfg, err := in.routingConfig(ctx, s)
	if err != nil {
		return p, err
	}
	p.cfg = cfg
	if cfg.Domain != domain {
		return p, fmt.Errorf("company.yaml says domain %q but ConfigMap %s/%s says %q; run `can apply` (or `can up`) to update the routing facts", domain, routing.Namespace, routing.ConfigMapName, cfg.Domain)
	}
	if (ui || api) && !cfg.TLS {
		in.cfg.Log.Warn("not routing the web UI or API: the stack serves plain HTTP only (no domain yet)", "module", s.ID)
		ui, api = false, false
	}
	if ui || api {
		w := m.Runtime.PublicUI
		if w == nil {
			return p, fmt.Errorf("expose is set but module %s declares no runtime.public_ui", s.ID)
		}
		if !routing.MayClaim(s.ID, s.ID) {
			return p, fmt.Errorf("module %s may not claim the host %s.%s", s.ID, s.ID, domain)
		}
		if err := routing.CheckUIPaths(w.Exact, w.Prefixes); err != nil {
			return p, fmt.Errorf("runtime.public_ui: %w", err)
		}
		host := s.ID + "." + domain
		var routes []any
		add := func(matcher, path string) {
			routes = append(routes, routeRule("Host(`"+host+"`) && "+matcher+"(`"+path+"`)", 0, w.Service, w.Port, middlewareWeb))
		}
		if ui {
			p.uiHost = host
			for _, e := range w.Exact {
				add("Path", e)
			}
			for _, pre := range w.Prefixes {
				add("Path", pre)
				add("PathPrefix", pre+"/")
			}
			// While the UI is routed the module runs with it enabled and, unless asked otherwise,
			// without the paste-a-token form: launch tickets are the way in.
			conf := deepMerge(w.Config, map[string]any{"ui": map[string]any{"token_login": x.TokenLogin}})
			p.values = deepMerge(p.values, map[string]any{"config": conf})
		}
		if api {
			add("PathPrefix", "/v1/")
			// Behind Traefik the module must take the client address Traefik appends, or every
			// caller shares one address for its limits. Modules that can say so do it here.
			if tp, ok := w.Config["trust_proxy"]; ok {
				p.values = deepMerge(p.values, map[string]any{"config": map[string]any{"trust_proxy": tp}})
			}
		}
		p.backends = append(p.backends, backend{w.Service, w.Port})
		p.objects = append(p.objects,
			middleware(s, middlewareWeb, map[string]any{"headers": map[string]any{
				"stsSeconds": 31536000, "contentTypeNosniff": true, "frameDeny": true,
			}}),
			ingressRoute(s, routeWeb, true, routes))
	}
	if siteWanted {
		r := m.Runtime.SiteRoute
		if routing.SitePrefixes[s.ID] != r.Path {
			return p, fmt.Errorf("module %s may not claim %s on the public site", s.ID, r.Path)
		}
		var routes []any
		mws := []string{stripName(s.ID), middlewareSite}
		for _, m := range cfg.SiteMatches(r.Path) {
			routes = append(routes, routeRule(m, routing.SitePriority, r.Service, r.Port, mws...))
		}
		if len(routes) > 0 {
			p.backends = append(p.backends, backend{r.Service, r.Port})
			p.objects = append(p.objects,
				middleware(s, stripName(s.ID), map[string]any{"stripPrefix": map[string]any{"prefixes": []any{r.Path}}}),
				middleware(s, middlewareSite, map[string]any{"headers": map[string]any{"contentTypeNosniff": true}}),
				ingressRoute(s, siteRouteName(s.ID), cfg.TLS, routes))
		}
	}
	return p, nil
}

func routeRule(match string, priority int, service string, port int, middlewares ...string) map[string]any {
	r := map[string]any{"match": match, "kind": "Rule", "services": []any{map[string]any{"name": service, "port": port}}}
	if priority > 0 {
		r["priority"] = priority
	}
	var ms []any
	for _, m := range middlewares {
		ms = append(ms, map[string]any{"name": m})
	}
	if len(ms) > 0 {
		r["middlewares"] = ms
	}
	return r
}

func routeMeta(s reconcile.Spec, name string) map[string]any {
	return map[string]any{"name": name, "namespace": s.Namespace, "labels": map[string]any{labelManaged: "can-core", labelModule: s.ID, labelRoute: "true"}}
}

// ingressRoute is TLS like the rest of the stack: websecure with the letsencrypt resolver.
func ingressRoute(s reconcile.Spec, name string, tls bool, routes []any) kube.Object {
	spec := map[string]any{"entryPoints": []any{"web"}, "routes": routes}
	if tls {
		spec["entryPoints"] = []any{"websecure"}
		spec["tls"] = map[string]any{"certResolver": "letsencrypt"}
	}
	return kube.Object{"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute", "metadata": routeMeta(s, name), "spec": spec}
}

func middleware(s reconcile.Spec, name string, spec map[string]any) kube.Object {
	return kube.Object{"apiVersion": "traefik.io/v1alpha1", "kind": "Middleware", "metadata": routeMeta(s, name), "spec": spec}
}

// routingNames are every routing object can-core may own for a module.
func routingNames(id string) []struct{ kind, name string } {
	return []struct{ kind, name string }{
		{"IngressRoute", routeWeb}, {"IngressRoute", siteRouteName(id)},
		{"Middleware", middlewareWeb}, {"Middleware", stripName(id)}, {"Middleware", middlewareSite},
	}
}

// pruneRoutes deletes the routing objects that are not in keep. It runs before a rollout too, so
// switching exposure off takes effect even when the rollout then fails.
func (in *Installer) pruneRoutes(ctx context.Context, id string, keep []kube.Object) error {
	have := map[string]bool{}
	for _, o := range keep {
		have[key(o)] = true
	}
	ns := "mod-" + id
	for _, n := range routingNames(id) {
		if have[n.kind+"/"+n.name] {
			continue
		}
		if _, err := in.cfg.API.Get(ctx, n.kind, ns, n.name); kube.IsNotFound(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("reading %s %s: %w", n.kind, n.name, err)
		}
		if err := in.cfg.API.Delete(ctx, n.kind, ns, n.name); err != nil {
			return fmt.Errorf("removing %s %s: %w", n.kind, n.name, err)
		}
	}
	return nil
}

func intOf(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// podPort resolves a route's Service port to the pod port a NetworkPolicy has to name.
func podPort(objs []kube.Object, service string, port any) (int, bool) {
	for _, o := range objs {
		if o["kind"] != "Service" || nameOf(o) != service {
			continue
		}
		for _, e := range asList(asMap(o["spec"])["ports"]) {
			pm := asMap(e)
			n, _ := intOf(pm["port"])
			want, isNum := intOf(port)
			if (isNum && want != n) || (!isNum && pm["name"] != port) {
				continue
			}
			switch t := pm["targetPort"].(type) {
			case nil:
				return n, true
			case string:
				if v, err := strconv.Atoi(t); err == nil {
					return v, true
				}
				return containerPort(objs, t)
			default:
				return intOf(t)
			}
		}
	}
	return 0, false
}

func containerPort(objs []kube.Object, name string) (int, bool) {
	for _, o := range objs {
		for _, c := range asList(podSpecRead(o)["containers"]) {
			for _, p := range asList(asMap(c)["ports"]) {
				if pm := asMap(p); pm["name"] == name {
					return intOf(pm["containerPort"])
				}
			}
		}
	}
	return 0, false
}

// podSpecRead is podSpec without creating anything.
func podSpecRead(o kube.Object) map[string]any {
	if k := o["kind"]; k != "Deployment" && k != "StatefulSet" {
		return nil
	}
	return asMap(asMap(asMap(o["spec"])["template"])["spec"])
}

// routedPorts lists the pod ports any route of the module sends to. Every backend of can-core's
// own routes must resolve (otherwise the manifest and the chart disagree).
func routedPorts(chart []kube.Object, p routePlan) ([]int, error) {
	set := map[int]bool{}
	for _, b := range p.backends {
		n, ok := podPort(chart, b.service, b.port)
		if !ok {
			return nil, fmt.Errorf("the module manifest routes service %s port %d, which the chart does not define", b.service, b.port)
		}
		set[n] = true
	}
	for _, o := range chart {
		if o["kind"] != "IngressRoute" {
			continue
		}
		for _, r := range asList(asMap(o["spec"])["routes"]) {
			for _, s := range asList(asMap(r)["services"]) {
				sm := asMap(s)
				name, _ := sm["name"].(string)
				if n, ok := podPort(chart, name, sm["port"]); ok {
					set[n] = true
				}
			}
		}
	}
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

// traefikPolicy narrows the template's allow-ingress-from-traefik policy to the routed ports; with
// none it admits nothing.
func traefikPolicy(tmpl kube.Object, ports []int) kube.Object {
	spec := asMap(tmpl["spec"])
	ingress := []any{}
	if len(ports) > 0 {
		var pl []any
		for _, n := range ports {
			pl = append(pl, map[string]any{"protocol": "TCP", "port": n})
		}
		rule := asMap(asList(spec["ingress"])[0])
		ingress = []any{map[string]any{"from": rule["from"], "ports": pl}}
	}
	spec["ingress"] = ingress
	return tmpl
}

// closedPolicy is a copy of the template policy that admits nothing.
func closedPolicy(tmpl kube.Object) (kube.Object, error) {
	b, err := json.Marshal(tmpl)
	if err != nil {
		return nil, err
	}
	var o kube.Object
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	return traefikPolicy(o, nil), nil
}

// applyRoutes runs after a successful rollout.
func (in *Installer) applyRoutes(ctx context.Context, s reconcile.Spec, policy kube.Object, p routePlan) error {
	if err := in.cfg.API.Apply(ctx, policy); err != nil {
		return fmt.Errorf("applying NetworkPolicy %s: %w", policyTraefik, err)
	}
	for _, o := range p.objects {
		if err := in.cfg.API.Apply(ctx, o); err != nil {
			return fmt.Errorf("applying %s: %w", key(o), err)
		}
	}
	return in.pruneRoutes(ctx, s.ID, p.objects)
}

// closeRoutes removes can-core's routes and shuts the ingress controller out (disable).
func (in *Installer) closeRoutes(ctx context.Context, id string) error {
	if err := in.pruneRoutes(ctx, id, nil); err != nil {
		return err
	}
	err := in.cfg.API.MergePatch(ctx, "NetworkPolicy", "mod-"+id, policyTraefik, kube.Object{"spec": map[string]any{"ingress": []any{}}})
	if kube.IsNotFound(err) {
		return nil
	}
	return err
}

// stampExposure rolls the pods when exposure changes what the module is configured with (a changed
// ConfigMap alone does not).
func stampExposure(objs []kube.Object, values map[string]any) {
	if len(values) == 0 {
		return
	}
	b, _ := json.Marshal(values)
	h := sha256.Sum256(b)
	for _, o := range objs {
		if k := o["kind"]; k != "Deployment" && k != "StatefulSet" {
			continue
		}
		tpl := asMap(asMap(o["spec"])["template"])
		if tpl == nil {
			continue
		}
		md := asMap(tpl["metadata"])
		if md == nil {
			md = map[string]any{}
			tpl["metadata"] = md
		}
		ann := asMap(md["annotations"])
		if ann == nil {
			ann = map[string]any{}
			md["annotations"] = ann
		}
		ann[annotationRoute] = hex.EncodeToString(h[:])[:16]
	}
}

// Routing implements reconcile.RouteAware: the fingerprint makes a change of exposure settings or
// routing facts re-apply the module, and the UI host is recorded for the Apps launcher. It is built
// from the facts as stored, so facts that do not parse fail the modules that need them (in Enable)
// and nothing else.
func (in *Installer) Routing(ctx context.Context, s reconcile.Spec) (reconcile.Routing, error) {
	if s.Manifest == nil {
		return reconcile.Routing{}, nil
	}
	data, found, err := in.routingData(ctx)
	if err != nil {
		return reconcile.Routing{}, err
	}
	b, err := json.Marshal(struct {
		V         int
		Domain    string
		Found     bool
		Facts     map[string]string
		Expose    config.Expose
		UI        *config.PublicUI
		Site      *config.SiteRoute
		Subdomain string
	}{1, in.domain(s), found, data, s.Expose, s.Manifest.Runtime.PublicUI, s.Manifest.Runtime.SiteRoute, s.Manifest.Runtime.Subdomain})
	if err != nil {
		return reconcile.Routing{}, err
	}
	out := reconcile.Routing{Fingerprint: base64.RawStdEncoding.EncodeToString(b)}
	cfg, err := in.routingConfig(ctx, s)
	if err == nil && s.Expose.UI && s.Manifest.Runtime.PublicUI != nil && cfg.TLS && cfg.Domain == in.domain(s) && cfg.Domain != "" && routing.MayClaim(s.ID, s.ID) {
		out.UIHost = s.ID + "." + cfg.Domain
	}
	return out, nil
}

// closePublic removes the chart's own routes (the module's anonymous surface) before a rollout, so
// `public: false` holds even when the rollout fails.
func (in *Installer) closePublic(ctx context.Context, s reconcile.Spec) error {
	items, err := in.cfg.API.List(ctx, "IngressRoute", s.Namespace, labelModule+"="+s.ID+","+labelManaged+"=can-core")
	if err != nil {
		return fmt.Errorf("listing routes: %w", err)
	}
	for _, o := range items {
		if asMap(asMap(o["metadata"])["labels"])[labelRoute] != nil {
			continue // can-core's own: pruneRoutes
		}
		if err := in.deleteOwned(ctx, s, "IngressRoute", nameOf(o)); err != nil {
			return fmt.Errorf("removing route %s: %w", nameOf(o), err)
		}
	}
	return nil
}

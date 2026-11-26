package installer

// Public access beyond the web UI and the forms route (specs/045-public-access): the edge limits,
// the signed-link route (finance invoices) and the headers on routes a chart makes itself.

import (
	"context"
	"fmt"
	"strings"

	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

const (
	middlewareLink  = "can-link-headers"
	middlewareSign  = "can-sign-headers"
	middlewareChart = "can-chart-headers"
	// loginPriority outranks the UI's own rules, whose priority is their length.
	loginPriority = 1000
)

func limitName(group, kind string) string { return "can-" + group + "-" + kind }

// limit adds the two middlewares of an edge limit to the plan (once) and returns their names.
func (p *routePlan) limit(s reconcile.Spec, group string, l routing.Limit) []string {
	rate, inflight := limitName(group, "rate"), limitName(group, "inflight")
	if p.limits == nil {
		p.limits = map[string]bool{}
	}
	if !p.limits[group] {
		p.limits[group] = true
		p.objects = append(p.objects, middleware(s, rate, l.RateSpec()), middleware(s, inflight, l.InFlightSpec()))
	}
	return []string{rate, inflight}
}

// covers reports whether path is one the UI's allowlist already routes.
func covers(exact, prefixes []string, path string) bool {
	for _, e := range exact {
		if path == e {
			return true
		}
	}
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// planLink routes the signed links of runtime.public_link: one path prefix, GET and HEAD, TLS, with
// headers that keep the page and its address out of search engines, caches, frames and referrers.
func (in *Installer) planLink(s reconcile.Spec, p *routePlan, domain string, limit bool) error {
	l := s.Manifest.Runtime.PublicLink
	if routing.LinkPrefixes[s.ID] != l.Prefix || !routing.MayClaim(s.ID, s.ID) {
		return fmt.Errorf("module %s may not publish %s", s.ID, l.Prefix)
	}
	host := s.ID + "." + domain
	mws := []string{middlewareLink}
	p.objects = append(p.objects, middleware(s, middlewareLink, map[string]any{"headers": map[string]any{
		"stsSeconds": 31536000, "contentTypeNosniff": true, "frameDeny": true, "referrerPolicy": "no-referrer",
		"customResponseHeaders": map[string]any{"X-Robots-Tag": "noindex, nofollow", "Cache-Control": "no-store"},
	}}))
	if limit {
		mws = append(mws, p.limit(s, "public", routing.PublicLimit)...)
	}
	var routes []any
	for _, method := range []string{"GET", "HEAD"} {
		match := "Host(`" + host + "`) && PathPrefix(`" + l.Prefix + "`) && Method(`" + method + "`)"
		routes = append(routes, routeRule(match, 0, l.Service, l.Port, mws...))
	}
	values := deepMerge(nil, l.Config)
	if l.BaseURLKey != "" {
		values = deepMerge(values, map[string]any{l.BaseURLKey: "https://" + host})
	}
	p.values = deepMerge(p.values, map[string]any{"config": values})
	p.backends = append(p.backends, backend{l.Service, l.Port})
	p.objects = append(p.objects, ingressRoute(s, routeLink, true, routes))
	return nil
}

// planSign routes the signing pages of runtime.public_sign (specs/049-contract-esign): one path
// prefix, GET, HEAD and the form's POST, TLS, with the same headers as signed links. The module
// authenticates each request by the single-use token in the path.
func (in *Installer) planSign(s reconcile.Spec, p *routePlan, domain string, limit bool) error {
	l := s.Manifest.Runtime.PublicSign
	if routing.SignPrefixes[s.ID] != l.Prefix || !routing.MayClaim(s.ID, s.ID) {
		return fmt.Errorf("module %s may not publish %s", s.ID, l.Prefix)
	}
	host := s.ID + "." + domain
	mws := []string{middlewareSign}
	p.objects = append(p.objects, middleware(s, middlewareSign, map[string]any{"headers": map[string]any{
		"stsSeconds": 31536000, "contentTypeNosniff": true, "frameDeny": true, "referrerPolicy": "no-referrer",
		"customResponseHeaders": map[string]any{"X-Robots-Tag": "noindex, nofollow"},
	}}))
	if limit {
		mws = append(mws, p.limit(s, "public", routing.PublicLimit)...)
	}
	var routes []any
	for _, method := range []string{"GET", "HEAD", "POST"} {
		match := "Host(`" + host + "`) && PathPrefix(`" + l.Prefix + "`) && Method(`" + method + "`)"
		routes = append(routes, routeRule(match, 0, l.Service, l.Port, mws...))
	}
	values := deepMerge(nil, l.Config)
	if l.BaseURLKey != "" {
		values = deepMerge(values, map[string]any{l.BaseURLKey: "https://" + host})
	}
	p.values = deepMerge(p.values, map[string]any{"config": values})
	p.backends = append(p.backends, backend{l.Service, l.Port})
	p.objects = append(p.objects, ingressRoute(s, routeSign, true, routes))
	return nil
}

// guardChartRoutes puts the security headers, and for anonymous modules the edge limit, in front of
// every route the chart makes (the careers pages). The references are added to the rendered objects,
// so the release record and a rollback carry them; the Middlewares join the plan.
func (in *Installer) guardChartRoutes(ctx context.Context, s reconcile.Spec, objs []kube.Object, p *routePlan) error {
	var routes []kube.Object
	for _, o := range objs {
		if o["kind"] == "IngressRoute" {
			routes = append(routes, o)
		}
	}
	if len(routes) == 0 {
		return nil
	}
	headers := map[string]any{"stsSeconds": 31536000, "contentTypeNosniff": true}
	// A wrapped application may be meant to be framed (a chat widget); in-house pages never are.
	if s.Manifest.Provider.Type == "inhouse" {
		headers["frameDeny"] = true
	}
	p.objects = append(p.objects, middleware(s, middlewareChart, map[string]any{"headers": headers}))
	names := []string{middlewareChart}
	if routing.PublicLimited[s.ID] && s.Expose.EdgeLimitOn() {
		data, _, err := in.routingData(ctx)
		if err != nil {
			return err
		}
		if data["edgeLimit"] != "off" {
			names = append(names, p.limit(s, "public", routing.PublicLimit)...)
		}
	}
	for _, o := range routes {
		for _, r := range asList(asMap(o["spec"])["routes"]) {
			rm := asMap(r)
			var mws []any
			for _, n := range names {
				mws = append(mws, map[string]any{"name": n})
			}
			rm["middlewares"] = append(mws, asList(rm["middlewares"])...)
		}
	}
	return nil
}

// applyMiddlewares creates the plan's Middlewares ahead of the routes that name them. The first
// can race the RoleBinding becoming effective, like the namespace's policies.
func (in *Installer) applyMiddlewares(ctx context.Context, p routePlan) error {
	for _, o := range p.objects {
		if o["kind"] != "Middleware" {
			continue
		}
		if err := in.retryForbidden(ctx, func() error { return in.cfg.API.Apply(ctx, o) }); err != nil {
			return fmt.Errorf("applying %s: %w", key(o), err)
		}
	}
	return nil
}

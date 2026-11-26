package launch

// What `can up` does in front of its own routes (specs/045-public-access): the redirect from plain
// HTTP on TLS installs, and the edge limits on the admin sign-in and the public site.

import (
	"context"
	"fmt"

	"github.com/dodwmd/companyinacan/core/routing"
)

// adminLoginRules are the admin UI's unauthenticated sign-in paths. A rule for a path can-core
// does not serve costs nothing.
var adminLoginRules = []string{"Path(`/login`)", "Path(`/api/v1/login`)", "PathPrefix(`/auth/`)", "Path(`/set-password`)"}

const (
	redirectName  = "https-redirect"
	redirectMW    = "can-https-redirect"
	loginPriority = 1000
	// redirectMatch is only what a browser navigates with. A POST over plain HTTP (a form, a
	// login, an API client with its token) still gets 404, so the mistake is seen and fixed
	// instead of the secret being sent in clear on every request from then on.
	redirectMatch = "(Method(`GET`) || Method(`HEAD`)) && PathPrefix(`/`)"
)

func middlewareDoc(name string, spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "Middleware",
		"metadata": map[string]any{"name": name, "namespace": SystemNS, "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}},
		"spec":     spec,
	}
}

// edgeMiddlewares are the Middlewares of a limit and their names.
func edgeMiddlewares(group string, l routing.Limit) (docs []map[string]any, names []string) {
	names = []string{"can-" + group + "-rate", "can-" + group + "-inflight"}
	return []map[string]any{middlewareDoc(names[0], l.RateSpec()), middlewareDoc(names[1], l.InFlightSpec())}, names
}

// RedirectManifests sends plain-HTTP page loads to the same address over HTTPS. It is a route on
// the `web` entry point of the lowest priority, not a setting of the entry point: certificates are
// validated with TLS-ALPN on 443 and are not touched, and removing the route brings plain HTTP
// back. The redirect is temporary, so no browser remembers it if the server falls back to plain
// HTTP. It must only be applied on a TLS install.
func RedirectManifests() ([]byte, error) {
	r := rule(redirectMatch, sitePriority, "site", 8081, redirectMW)
	return marshalDocs([]map[string]any{
		middlewareDoc(redirectMW, map[string]any{"redirectScheme": map[string]any{"scheme": "https", "permanent": false}}),
		ingressRoute(redirectName, SystemNS, []any{r}, RouteOptions{}),
	})
}

// redirect applies the redirect on a TLS install and removes it otherwise: the plain-HTTP
// fallback serves the site on `web`, and an earlier run may have used TLS.
func (r *runner) redirect(ctx context.Context) error {
	if !r.o.Routes.TLS {
		for _, obj := range [][2]string{{"ingressroutes.traefik.io", redirectName}, {"middlewares.traefik.io", redirectMW}} {
			if _, err := r.kube(ctx, nil, "-n", SystemNS, "delete", obj[0], obj[1], "--ignore-not-found"); err != nil {
				return fmt.Errorf("removing the HTTPS redirect: %w", err)
			}
		}
		return nil
	}
	m, err := RedirectManifests()
	if err != nil {
		return err
	}
	if _, err := r.apply(ctx, m, false); err != nil {
		return fmt.Errorf("HTTPS redirect: %w", err)
	}
	return nil
}

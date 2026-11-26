// Package routing holds what the platform knows about public routes (specs/040-module-routing):
// which hostnames and paths a module may claim, and the cluster's routing facts (the company
// domain, TLS, the hosts that serve the public site). can-core, the `can` CLI and the
// ValidatingAdmissionPolicy in deploy/base/admission-policy.yaml apply the same rules; a contract
// test keeps the Go tables and the policy in step.
package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The ConfigMap with the routing facts. The `can` CLI writes it (`can up`, `can apply`); can-core
// only reads it, and the admission policy refuses can-core any write to it.
const (
	Namespace     = "can-system"
	ConfigMapName = "can-routing"
)

// SitePriority is the priority of a module's route on a site host: above the site's catch-all (1).
const SitePriority = 100

// Aliases are the only hostname labels a module may claim besides its own id.
var Aliases = map[string]string{"jobs": "careers", "provisioning": "scim", "analytics": "stats"}

// SitePrefixes are the only paths on the public site's hosts a module may claim.
var SitePrefixes = map[string]string{"forms": "/_forms"}

// reservedLabels are never a module's.
var reservedLabels = map[string]bool{"admin": true, "www": true}

// MayClaim reports whether module id may route the host <label>.<company domain>.
func MayClaim(id, label string) bool {
	if id == "" || label == "" || reservedLabels[label] {
		return false
	}
	if Aliases[id] == label {
		return true
	}
	for _, v := range Aliases {
		if v == label {
			return false
		}
	}
	return label == id
}

var (
	hostRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	exactRE  = regexp.MustCompile(`^/([a-z0-9][a-z0-9._-]{0,62})?$`)
	prefixRE = regexp.MustCompile(`^/[a-z0-9][a-z0-9-]{0,62}$`)
)

// internalSegments are first path segments that are never part of a web UI.
var internalSegments = map[string]bool{"v1": true, "healthz": true, "readyz": true, "metrics": true, "public": true}

// CheckUIPaths refuses a manifest's UI path list that would route more than a web UI: "/" only as
// an exact path, one path segment each, and never the API, health or metrics paths.
func CheckUIPaths(exact, prefixes []string) error {
	if len(exact)+len(prefixes) == 0 {
		return errors.New("no UI paths are listed")
	}
	check := func(p string, re *regexp.Regexp) error {
		if !re.MatchString(p) || internalSegments[strings.TrimPrefix(p, "/")] {
			return fmt.Errorf("%q is not a UI path (one lowercase segment; not the API, health or metrics paths)", p)
		}
		return nil
	}
	for _, p := range exact {
		if err := check(p, exactRE); err != nil {
			return err
		}
	}
	for _, p := range prefixes {
		if err := check(p, prefixRE); err != nil {
			return err
		}
	}
	return nil
}

// Config is the content of the can-routing ConfigMap.
type Config struct {
	Domain string
	// TLS is false only for the plain-HTTP-on-the-IP fallback of `can up`: no UI or API is routed then.
	TLS bool
	// SiteHosts serve the public site; CatchAll serves it for any host.
	SiteHosts []string
	CatchAll  bool
	// NoEdgeLimit switches the edge limits off: a proxy or CDN in front of the server makes every
	// client share its addresses (specs/045-public-access).
	NoEdgeLimit bool
	// NoEdgeLimitReason says what switched them off (ReasonFlag, ReasonProxied), so that `can
	// apply` knows what a change of company.yaml may undo. "" with NoEdgeLimit: written before
	// this was recorded.
	NoEdgeLimitReason string
}

// Why the edge limits are off.
const (
	ReasonFlag    = "flag"    // `can up --no-edge-limits`
	ReasonProxied = "proxied" // dns.proxied
)

// EdgeLimits decides the two fields: the flag outlasts company.yaml, dns.proxied lasts as long
// as it is set.
func EdgeLimits(flag, proxied bool) (off bool, reason string) {
	switch {
	case flag:
		return true, ReasonFlag
	case proxied:
		return true, ReasonProxied
	}
	return false, ""
}

// Default is what applies before anything was written: TLS, the site on the company domain.
func Default(domain string) Config {
	c := Config{Domain: domain, TLS: true}
	if domain != "" {
		c.SiteHosts = []string{domain}
	}
	return c
}

// Validate refuses anything that is not a plain lowercase DNS name.
func (c Config) Validate() error {
	for _, h := range append([]string{c.Domain}, c.SiteHosts...) {
		if len(h) > 253 || !hostRE.MatchString(h) {
			return fmt.Errorf("routing: %q is not a plain lowercase DNS name", h)
		}
	}
	return nil
}

// SiteMatches are the Traefik rules of a module's route on the site: path on every site host, and
// for any host when the site is served that way. can-core creates exactly these and `can up` waits
// for exactly these.
func (c Config) SiteMatches(path string) []string {
	var out []string
	for _, h := range c.SiteHosts {
		out = append(out, "Host(`"+h+"`) && PathPrefix(`"+path+"`)")
	}
	if c.CatchAll {
		out = append(out, "PathPrefix(`"+path+"`)")
	}
	return out
}

// EntryPoint is the Traefik entry point of the stack.
func (c Config) EntryPoint() string {
	if c.TLS {
		return "websecure"
	}
	return "web"
}

var siteHostRule = regexp.MustCompile("^Host\\(`([^`]+)`\\)$")

// FromSiteRoute reads the facts off the site's IngressRoute (can-system/site, as JSON), which `can
// up` made: the hosts that serve the site and whether it does so over TLS. A rule it does not
// understand is an error: guessing would route modules on the wrong names.
func FromSiteRoute(domain string, ingressRoute []byte) (Config, error) {
	var r struct {
		Spec struct {
			EntryPoints []string `json:"entryPoints"`
			Routes      []struct {
				Match string `json:"match"`
			} `json:"routes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(ingressRoute, &r); err != nil {
		return Config{}, fmt.Errorf("routing: the site route is not readable: %w", err)
	}
	c := Config{Domain: domain, TLS: slices.Contains(r.Spec.EntryPoints, "websecure")}
	for _, rt := range r.Spec.Routes {
		switch m := siteHostRule.FindStringSubmatch(rt.Match); {
		case m != nil:
			c.SiteHosts = append(c.SiteHosts, m[1])
		case rt.Match == "PathPrefix(`/`)":
			c.CatchAll = true
		default:
			return Config{}, fmt.Errorf("routing: the site route has a rule this does not understand: %q", rt.Match)
		}
	}
	if len(c.SiteHosts) == 0 && !c.CatchAll {
		return Config{}, errors.New("routing: the site route serves nothing")
	}
	return c, c.Validate()
}

// Data is the ConfigMap data.
func (c Config) Data() map[string]string {
	return map[string]string{
		"domain": c.Domain, "tls": strconv.FormatBool(c.TLS),
		"siteHosts": strings.Join(c.SiteHosts, ","), "catchAll": strconv.FormatBool(c.CatchAll),
		"edgeLimit": edgeLimitValue(c.NoEdgeLimit), "edgeLimitReason": c.NoEdgeLimitReason,
	}
}

func edgeLimitValue(off bool) string {
	if off {
		return "off"
	}
	return "on"
}

// Manifest renders the ConfigMap.
func (c Config) Manifest() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": ConfigMapName, "namespace": Namespace, "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}},
		"data":     c.Data(),
	})
}

// Parse reads the ConfigMap data.
func Parse(data map[string]string) (Config, error) {
	c := Config{Domain: data["domain"], TLS: data["tls"] == "true", CatchAll: data["catchAll"] == "true", NoEdgeLimit: data["edgeLimit"] == "off", NoEdgeLimitReason: data["edgeLimitReason"]}
	for _, h := range strings.Split(data["siteHosts"], ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.SiteHosts = append(c.SiteHosts, h)
		}
	}
	return c, c.Validate()
}

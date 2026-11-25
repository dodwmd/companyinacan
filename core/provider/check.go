package provider

import (
	"fmt"
	"sort"
	"strings"
)

// Issue is a semantic problem found in company.yaml settings without calling
// any API. Path is a JSON pointer into the document.
type Issue struct {
	Rule, Path, Message string
}

// isSecretRef reports whether v has the form of a secret reference.
func isSecretRef(v string) bool { return ValidSecretRef(v) }

// CheckProviderOptions checks provider.options for typ: a secret must be a
// reference, and digitalocean rejects unknown keys, a non-22 port and
// malformed values.
func CheckProviderOptions(typ string, o map[string]any) []Issue {
	var out []Issue
	if v, ok := o["token_secret_ref"].(string); ok && (typ == "digitalocean" || typ == "hetzner") && !isSecretRef(v) {
		// the value is not echoed: a literal secret must not reach output
		out = append(out, Issue{"semantic.secret-ref", "/provider/options/token_secret_ref", "token_secret_ref must be a reference (env:NAME or file:PATH), never the token itself"})
	}
	if typ != "digitalocean" { // digitalocean's option check below covers it
		ip, err := operatorIPString(o)
		if err == nil {
			err = CheckOperatorIP(ip)
		}
		if err != nil {
			out = append(out, Issue{"semantic.provider-option", "/provider/options/operator_ip", err.Error()})
		}
	}
	if typ != "digitalocean" {
		return out
	}
	rest := map[string]any{}
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		known := false
		for _, a := range doOptionKeys {
			known = known || a == k
		}
		if !known {
			out = append(out, Issue{"semantic.provider-option", "/provider/options/" + k, fmt.Sprintf("%q is not a digitalocean option (known: %s)", k, strings.Join(DigitalOceanOptionKeys(), ", "))})
			continue
		}
		if k != "token_secret_ref" {
			rest[k] = o[k]
		}
	}
	if _, err := (&DigitalOcean{}).conf(Spec{Name: "x", Options: rest}); err != nil {
		out = append(out, Issue{"semantic.provider-option", "/provider/options", err.Error()})
	}
	return out
}

// CheckDNS checks the dns block against the company domain.
func CheckDNS(domain string, m map[string]any) []Issue {
	if m == nil {
		return nil
	}
	var out []Issue
	cfg, err := DNSConfigFrom(m)
	if err != nil {
		return nil // unknown keys and wrong types are reported by the schema check
	}
	switch cfg.Provider {
	case "", "manual":
		for _, k := range []string{"zone", "ttl", "proxied", "adopt_existing", "token_secret_ref"} {
			if _, set := m[k]; set {
				out = append(out, Issue{"semantic.dns-option", "/dns/" + k, fmt.Sprintf("dns.%s only applies to dns.provider: cloudflare", k)})
			}
		}
	case "cloudflare":
		if cfg.Zone != "" && domain != cfg.Zone && !strings.HasSuffix(domain, "."+cfg.Zone) {
			out = append(out, Issue{"semantic.dns-zone", "/dns/zone", fmt.Sprintf("zone %q does not contain the company domain %q", cfg.Zone, domain)})
		}
		if cfg.TTL != 0 && cfg.TTL != 1 && cfg.TTL < 60 {
			out = append(out, Issue{"semantic.dns-ttl", "/dns/ttl", "Cloudflare accepts 1 (automatic) or 60..86400 seconds"})
		}
	}
	return out
}

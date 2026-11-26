package provider

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"text/tabwriter"
)

// Record is one DNS record the company needs.
type Record struct {
	Type  string `json:"type"` // A or AAAA
	Name  string `json:"name"` // fully qualified, no trailing dot
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
	// Purpose says what the name is for, in lists shown to the operator.
	Purpose string `json:"purpose,omitempty"`
	// NoAdopt marks a module's hostname (specs/045-public-access): a plugin never changes a record
	// it did not create there, whatever dns.adopt_existing says, and skips the name instead of
	// failing.
	NoAdopt bool `json:"-"`
}

// DNS is a DNS plugin (manual, cloudflare; provider-native is not built yet).
type DNS interface {
	Name() string
	// Apply creates the records, or tells the operator how. Output goes to w.
	Apply(ctx context.Context, domain string, recs []Record, w io.Writer) error
}

// DNSRemover is implemented by plugins that created records and can take
// them down again (`can down`). Plugins that cannot (manual) do not implement it.
type DNSRemover interface {
	// Remove deletes only the records the plugin created (never adopted or
	// foreign ones). It is idempotent.
	Remove(ctx context.Context, domain string, w io.Writer) error
}

// DNSPlanner is implemented by plugins that can describe, without any network
// call, what Apply would do (`can up --plan`).
type DNSPlanner interface {
	PlanRecords(domain string, recs []Record) []string
}

// DNSChecker is implemented by plugins that can verify, read-only, that Apply
// will work (token resolves and has access, zone exists). `can up` calls it
// before it creates a server.
type DNSChecker interface {
	Preflight(ctx context.Context, domain string) error
}

// DNSConfig is the `dns` block of company.yaml.
type DNSConfig struct {
	Provider      string // manual (default) or cloudflare
	Zone          string // Cloudflare zone name; default: found from the domain
	TTL           int    // 0 = DefaultTTL; Cloudflare: 1 (automatic) or 60..86400
	Proxied       bool   // Cloudflare proxy; default false so ACME HTTP-01 reaches the origin
	AdoptExisting bool   // take over an existing unproxied record of the same name and type
	TokenRef      string // env:NAME or file:PATH; default env:CLOUDFLARE_API_TOKEN
}

// DNSConfigFrom reads a decoded `dns` mapping, rejecting unknown keys and
// values of the wrong type.
func DNSConfigFrom(m map[string]any) (DNSConfig, error) {
	var c DNSConfig
	for k := range m {
		switch k {
		case "provider", "zone", "ttl", "proxied", "adopt_existing", "token_secret_ref":
		default:
			return c, fmt.Errorf("dns.%s is not a known setting (known: provider, zone, ttl, proxied, adopt_existing, token_secret_ref)", k)
		}
	}
	o := opts(m)
	c.Provider = o.str("provider", "")
	c.Zone = o.str("zone", "")
	c.TokenRef = o.str("token_secret_ref", "")
	var err error
	if c.TTL, err = o.integerE("ttl", 0); err != nil {
		return c, fmt.Errorf("dns.ttl must be a whole number, got %v", m["ttl"])
	}
	for _, f := range []struct {
		key string
		dst *bool
	}{{"proxied", &c.Proxied}, {"adopt_existing", &c.AdoptExisting}} {
		if v, ok := m[f.key]; ok && v != nil {
			b, isBool := v.(bool)
			if !isBool {
				return c, fmt.Errorf("dns.%s must be true or false, got %v", f.key, v)
			}
			*f.dst = b
		}
	}
	return c, nil
}

// NewDNS returns the DNS plugin called name when it needs no settings. The
// default is "manual". Use NewDNSWith for plugins that call an API.
func NewDNS(name string) (DNS, error) {
	switch name {
	case "", "manual":
		return Manual{}, nil
	case "cloudflare":
		return nil, fmt.Errorf("the cloudflare DNS plugin needs a dns block in company.yaml (provider: cloudflare, token_secret_ref)")
	case "provider-native":
		return nil, NotImplemented("the "+name+" DNS plugin", "E12")
	}
	return nil, fmt.Errorf("unknown DNS plugin %q (known: manual, cloudflare)", name)
}

// NewDNSWith returns the DNS plugin called name with its settings. st holds
// the records a plugin created so `can down` can remove them.
func NewDNSWith(name string, cfg DNSConfig, deps Deps, st *Store) (DNS, error) {
	switch name {
	case "cloudflare":
		deps.defaults()
		return newCloudflare(cfg, deps, st)
	}
	return NewDNS(name)
}

// DefaultTTL is used for generated records.
const DefaultTTL = 300

// Records returns the records for domain: the apex plus one per subdomain.
func Records(domain, ip string, subs []string) ([]Record, error) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return nil, fmt.Errorf("%q is not an IP address", ip)
	}
	typ := "A"
	if a.Unmap().Is6() {
		typ = "AAAA"
	}
	recs := []Record{{Type: typ, Name: domain, Value: a.Unmap().String(), TTL: DefaultTTL}}
	for _, s := range subs {
		recs = append(recs, Record{Type: typ, Name: s + "." + domain, Value: a.Unmap().String(), TTL: DefaultTTL})
	}
	return recs, nil
}

// Manual prints the records for the operator to add at their DNS host.
type Manual struct{}

// Name implements DNS.
func (Manual) Name() string { return "manual" }

// Apply implements DNS.
func (Manual) Apply(_ context.Context, domain string, recs []Record, w io.Writer) error {
	fmt.Fprintf(w, "Add these DNS records for %s at your DNS host:\n\n", domain)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tNAME\tVALUE\tTTL\tFOR")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.Type, r.Name, r.Value, r.TTL, r.Purpose)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nPropagation time is not part of the five-minute budget. Re-run `can up` once the records resolve to switch to your domain.")
	return nil
}

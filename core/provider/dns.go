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
}

// DNS is a DNS plugin. Cloudflare and provider-native plugins arrive in
// later epics behind the same interface.
type DNS interface {
	Name() string
	// Apply creates the records, or tells the operator how. Output goes to w.
	Apply(ctx context.Context, domain string, recs []Record, w io.Writer) error
}

// NewDNS returns the DNS plugin called name. The default is "manual".
func NewDNS(name string) (DNS, error) {
	switch name {
	case "", "manual":
		return Manual{}, nil
	case "cloudflare", "provider-native":
		return nil, NotImplemented("the "+name+" DNS plugin", "E12")
	}
	return nil, fmt.Errorf("unknown DNS plugin %q (known: manual)", name)
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
	recs := []Record{{typ, domain, a.Unmap().String(), DefaultTTL}}
	for _, s := range subs {
		recs = append(recs, Record{typ, s + "." + domain, a.Unmap().String(), DefaultTTL})
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
	fmt.Fprintln(tw, "TYPE\tNAME\tVALUE\tTTL")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", r.Type, r.Name, r.Value, r.TTL)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nPropagation time is not part of the five-minute budget. Re-run `can up` once the records resolve to switch to your domain.")
	return nil
}

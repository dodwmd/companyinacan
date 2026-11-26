package cli

// The DNS records a company needs, including one per module hostname that company.yaml puts on the
// internet (specs/045-public-access), for `can up` and `can status`.

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/routing"
)

var recordPurposes = map[string]string{"admin": "admin UI", "help": "help"}

// companyRecords returns the records for the server at ip: the domain, the platform's own names and
// the module hosts. A module whose manifest cannot be read is reported, not guessed. Without an IP
// address there are no records (err says so).
func (t *target) companyRecords(ip, modulesDir string) (recs []provider.Record, warnings []string, err error) {
	domain := t.company.Domain
	if recs, err = provider.Records(domain, ip, instantSubdomains); err != nil {
		return nil, nil, err
	}
	names := map[string]bool{}
	for i := range recs {
		r := &recs[i]
		names[r.Name] = true
		r.Purpose = "site"
		for sub, p := range recordPurposes {
			if r.Name == sub+"."+domain {
				r.Purpose = p
			}
		}
	}
	md := resolveModulesDir(modulesDir, t.dir)
	surfaces, unknown := routing.CompanySurfaces(t.company, func(id string) *config.Module {
		m, err := loadModule(md, id)
		if err != nil {
			return nil
		}
		return m
	})
	for _, id := range unknown {
		warnings = append(warnings, fmt.Sprintf("module %q has no readable manifest in %s: any DNS record it needs is not listed", id, md))
	}
	for _, h := range routing.DNSHosts(domain, surfaces) {
		if names[h.Host] {
			continue
		}
		recs = append(recs, provider.Record{Type: recs[0].Type, Name: h.Host, Value: recs[0].Value, TTL: provider.DefaultTTL, Purpose: h.Purpose, NoAdopt: true})
	}
	return recs, warnings, nil
}

// Where a record's name points now, as this machine's resolver sees it.
const (
	dnsOK        = "ok"
	dnsMissing   = "missing"
	dnsElsewhere = "elsewhere"
)

type dnsStatus struct {
	provider.Record
	State string   `json:"state"`
	Seen  []string `json:"seen,omitempty"`
}

// resolveRecords looks every name up. It informs only: DNS has its own clock, and a proxy in front
// (dns.proxied) answers with its own addresses.
func resolveRecords(ctx context.Context, res provider.Resolver, recs []provider.Record) []dnsStatus {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := make([]dnsStatus, 0, len(recs))
	for _, r := range recs {
		s := dnsStatus{Record: r, State: dnsMissing}
		addrs, err := res.LookupHost(ctx, r.Name)
		if err == nil && len(addrs) > 0 {
			s.State, s.Seen = dnsElsewhere, addrs
			want, _ := netip.ParseAddr(r.Value)
			if slices.ContainsFunc(addrs, func(a string) bool {
				got, err := netip.ParseAddr(a)
				return err == nil && got.Unmap() == want
			}) {
				s.State, s.Seen = dnsOK, nil
			}
		}
		out = append(out, s)
	}
	return out
}

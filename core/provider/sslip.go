package provider

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// SSLIPDomain is the wildcard DNS service used for instant URLs. It is a
// volunteer-run external service, so every use has a fallback (see PlanInstant).
const SSLIPDomain = "sslip.io"

// Let's Encrypt limits we stay under (letsencrypt.org/docs/rate-limits).
// The real limits are shared with every other sslip.io user, so these are
// conservative bounds on what this company may add, not guarantees.
const (
	// maxSameSetPerWeek is below LE's 5 duplicate certificates per week.
	maxSameSetPerWeek = 4
	// maxFailuresPerHour is below LE's 5 failed validations per hour.
	maxFailuresPerHour = 3
)

// InstantMode says how the site is first served.
type InstantMode string

// Instant URL modes.
const (
	ModeHTTPS        InstantMode = "https"
	ModeHTTPFallback InstantMode = "http-fallback"
)

// InstantPlan is the decision for the instant (pre-DNS) URL.
type InstantPlan struct {
	Mode   InstantMode `json:"mode"`
	Reason string      `json:"reason,omitempty"`
	Hosts  []string    `json:"hosts,omitempty"` // names needing certificates (https mode)
	URLs   []string    `json:"urls"`
	// AdminViaTunnelOnly is true in fallback mode: the admin UI is not
	// exposed over plain HTTP and is reached through the SSH tunnel.
	AdminViaTunnelOnly bool `json:"admin_via_tunnel_only"`
}

// Dashed returns the sslip.io label for an IP: 203.0.113.7 -> 203-0-113-7.
func Dashed(ip string) (string, error) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("%q is not an IP address", ip)
	}
	a = a.Unmap()
	if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() {
		return "", fmt.Errorf("%s is not a public address; sslip.io instant URLs need one", ip)
	}
	if a.Is4() {
		return strings.ReplaceAll(a.String(), ".", "-"), nil
	}
	return strings.ReplaceAll(a.String(), ":", "-"), nil
}

// InstantHosts returns the hostnames for ip: the bare name plus one per
// subdomain, e.g. "203-0-113-7.sslip.io", "admin.203-0-113-7.sslip.io".
func InstantHosts(ip string, subs []string) ([]string, error) {
	d, err := Dashed(ip)
	if err != nil {
		return nil, err
	}
	base := d + "." + SSLIPDomain
	hosts := []string{base}
	for _, s := range subs {
		hosts = append(hosts, s+"."+base)
	}
	return hosts, nil
}

// Resolver looks names up (net.Resolver satisfies it).
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

func hostSetKey(hosts []string) string {
	h := append([]string(nil), hosts...)
	sort.Strings(h)
	return strings.Join(h, ",")
}

// PlanInstant decides between real-TLS sslip.io URLs and the plain-HTTP
// fallback. It checks, in order: that sslip.io resolves the name to this
// server, and that issuing certificates now would stay under the ACME rate
// limits given our recorded history. Nothing is issued here.
func PlanInstant(ctx context.Context, ip string, subs []string, now time.Time, history []Issuance, res Resolver) InstantPlan {
	fallback := func(reason string) InstantPlan {
		return InstantPlan{
			Mode:               ModeHTTPFallback,
			Reason:             reason,
			URLs:               []string{"http://" + ip},
			AdminViaTunnelOnly: true,
		}
	}
	hosts, err := InstantHosts(ip, subs)
	if err != nil {
		return fallback(err.Error())
	}
	addrs, err := res.LookupHost(ctx, hosts[0])
	if err != nil {
		return fallback("sslip.io did not resolve " + hosts[0] + ": " + err.Error())
	}
	want, _ := netip.ParseAddr(ip)
	ok := false
	for _, a := range addrs {
		if got, err := netip.ParseAddr(a); err == nil && got.Unmap() == want.Unmap() {
			ok = true
		}
	}
	if !ok {
		return fallback(fmt.Sprintf("sslip.io resolved %s to %v, not %s", hosts[0], addrs, ip))
	}

	key := hostSetKey(hosts)
	same, failures := 0, 0
	for _, h := range history {
		if h.OK && hostSetKey(h.Hosts) == key && now.Sub(h.At) < 7*24*time.Hour {
			same++
		}
		if !h.OK && now.Sub(h.At) < time.Hour {
			failures++
		}
	}
	if same >= maxSameSetPerWeek {
		return fallback(fmt.Sprintf("%d certificates for these names in the last 7 days; Let's Encrypt allows 5 per week", same))
	}
	if failures >= maxFailuresPerHour {
		return fallback(fmt.Sprintf("%d failed issuances in the last hour; backing off to stay under the Let's Encrypt failed-validation limit", failures))
	}

	urls := make([]string, len(hosts))
	for i, h := range hosts {
		urls[i] = "https://" + h
	}
	return InstantPlan{Mode: ModeHTTPS, Hosts: hosts, URLs: urls}
}

// RecordIssuance appends an issuance attempt to st and drops entries older
// than 14 days.
func RecordIssuance(st *State, hosts []string, ok bool, now time.Time) {
	st.Issued = append(st.Issued, Issuance{At: now.UTC(), Hosts: hosts, OK: ok})
	kept := st.Issued[:0]
	for _, i := range st.Issued {
		if now.Sub(i.At) < 14*24*time.Hour {
			kept = append(kept, i)
		}
	}
	st.Issued = kept
}

package notify

import (
	"net/netip"
	"testing"
)

// The webhook destination denylist: nothing that is not a routable public address.
func TestPublicAddr(t *testing.T) {
	refuse := map[string]string{
		"0.0.0.0":                "this-network 0.0.0.0/8",
		"0.1.2.3":                "this-network 0.0.0.0/8",
		"10.0.0.1":               "private",
		"127.0.0.1":              "loopback",
		"169.254.169.254":        "link-local metadata",
		"172.16.0.1":             "private",
		"192.168.1.1":            "private",
		"100.64.0.1":             "carrier-grade NAT 100.64.0.0/10",
		"100.127.255.254":        "carrier-grade NAT upper edge",
		"192.0.0.1":              "IETF protocol assignments 192.0.0.0/24",
		"192.0.2.1":              "documentation",
		"192.88.99.1":            "6to4 relay anycast 192.88.99.0/24",
		"198.18.0.1":             "benchmarking 198.18.0.0/15",
		"198.19.255.255":         "benchmarking upper edge",
		"198.51.100.1":           "documentation",
		"203.0.113.1":            "documentation",
		"224.0.0.1":              "multicast",
		"240.0.0.1":              "reserved",
		"255.255.255.255":        "broadcast (reserved 240.0.0.0/4)",
		"::":                     "unspecified",
		"::1":                    "loopback",
		"::2":                    "IPv4-compatible ::/96",
		"::7f00:1":               "IPv4-compatible form of 127.0.0.1",
		"::808:808":              "IPv4-compatible form, even of a public IPv4",
		"::ffff:127.0.0.1":       "IPv4-mapped loopback",
		"::ffff:10.1.2.3":        "IPv4-mapped private",
		"::ffff:100.64.0.1":      "IPv4-mapped CGNAT",
		"64:ff9b::7f00:1":        "NAT64 embedding 127.0.0.1",
		"64:ff9b::a00:1":         "NAT64 embedding 10.0.0.1",
		"64:ff9b::6440:1":        "NAT64 embedding 100.64.0.1",
		"64:ff9b::c000:1":        "NAT64 embedding 192.0.0.1",
		"64:ff9b::c612:1":        "NAT64 embedding 198.18.0.1",
		"64:ff9b::0.0.0.1":       "NAT64 embedding 0.0.0.1",
		"64:ff9b:1::1":           "local-use NAT64 64:ff9b:1::/48",
		"64:ff9b:1:ffff::1":      "local-use NAT64 upper part",
		"2001::1":                "Teredo 2001::/32",
		"2001:0:4136:e378::1":    "Teredo with an embedded server",
		"2001:db8::1":            "documentation",
		"2002::1":                "6to4",
		"2002:7f00:1::1":         "6to4 embedding 127.0.0.1",
		"3fff::1":                "documentation 3fff::/20",
		"3fff:fff:ffff::1":       "documentation upper edge",
		"100::1":                 "discard-only",
		"fc00::1":                "unique local",
		"fe80::1":                "link-local",
		"ff02::1":                "multicast",
		"::ffff:192.88.99.1":     "IPv4-mapped 6to4 relay",
		"::ffff:0.0.0.0":         "IPv4-mapped this-network",
		"64:ff9b::c058:6301":     "NAT64 embedding 192.88.99.1",
		"64:ff9b::c000:0201":     "NAT64 embedding 192.0.2.1",
		"::ffff:198.51.100.7":    "IPv4-mapped documentation",
		"64:ff9b::cb00:7101":     "NAT64 embedding 203.0.113.1",
		"64:ff9b::f000:1":        "NAT64 embedding 240.0.0.1",
		"64:ff9b::e000:1":        "NAT64 embedding multicast 224.0.0.1",
		"64:ff9b::a9fe:a9fe":     "NAT64 embedding 169.254.169.254",
		"64:ff9b::ac10:1":        "NAT64 embedding 172.16.0.1",
		"64:ff9b::c0a8:101":      "NAT64 embedding 192.168.1.1",
		"64:ff9b::":              "NAT64 embedding 0.0.0.0",
		"::ffff:169.254.169.254": "IPv4-mapped metadata",
	}
	for s, why := range refuse {
		if publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s (%s) was treated as public", s, why)
		}
	}
	allow := map[string]string{
		"8.8.8.8":              "public",
		"1.1.1.1":              "public",
		"100.63.255.255":       "just below CGNAT",
		"100.128.0.1":          "just above CGNAT",
		"192.0.1.1":            "between 192.0.0.0/24 and 192.0.2.0/24",
		"192.88.98.1":          "just below the 6to4 relay block",
		"192.88.100.1":         "just above the 6to4 relay block",
		"198.17.255.255":       "just below benchmarking",
		"198.20.0.1":           "just above benchmarking",
		"2001:4860:4860::8888": "public IPv6",
		"2606:4700:4700::1111": "public IPv6",
		"2001:1::1":            "outside 2001::/32",
		"3ffe::1":              "outside 3fff::/20",
		"::ffff:8.8.8.8":       "IPv4-mapped public",
		"64:ff9b::808:808":     "NAT64 embedding 8.8.8.8",
		"64:ff9b:2::1":         "outside the local-use NAT64 /48",
	}
	for s, why := range allow {
		if !publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s (%s) was refused", s, why)
		}
	}
}

// Package netguard is the outbound-destination denylist shared by every place can-core connects to
// an address it did not choose itself (webhook channels, Web Push endpoints): only routable public
// addresses are allowed, judged at connect time so DNS rebinding does not help.
package netguard

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "192.88.99.0/24", "2002::/16", "2001:db8::/32", "100::/64", "2001::/32", "3fff::/20", "64:ff9b:1::/48", "::/96"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// Public reports whether ip is a routable public address. IPv4-mapped and NAT64 (64:ff9b::/96)
// addresses are judged by the IPv4 address they embed. Refused outright: 6to4 (2002::/16) and its
// relay anycast (192.88.99.0/24), Teredo (2001::/32), local-use NAT64 (64:ff9b:1::/48), the
// deprecated IPv4-compatible form (::/96), the documentation ranges and 3fff::/20, and the other
// special-purpose IPv4 blocks listed in nonPublic.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && nat64.Contains(ip) {
		b := ip.As16()
		return Public(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// DenyPrivate is a net.Dialer Control function that rejects non-public destinations.
func DenyPrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !Public(ip) {
		return errors.New("destination is not a public address")
	}
	return nil
}

package netguard

import (
	"net/netip"
	"testing"
)

func TestPublic(t *testing.T) {
	for s, want := range map[string]bool{"8.8.8.8": true, "2606:4700::1111": true, "10.0.0.1": false, "127.0.0.1": false, "169.254.169.254": false, "::1": false, "fd00::1": false, "::ffff:10.0.0.1": false, "100.64.0.1": false, "64:ff9b::a00:1": false} {
		if got := Public(netip.MustParseAddr(s)); got != want {
			t.Errorf("Public(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestDenyPrivate(t *testing.T) {
	if err := DenyPrivate("tcp", "127.0.0.1:443", nil); err == nil {
		t.Error("loopback accepted")
	}
	if err := DenyPrivate("tcp", "8.8.8.8:443", nil); err != nil {
		t.Error(err)
	}
	if err := DenyPrivate("tcp", "not-an-ip:443", nil); err == nil {
		t.Error("hostname accepted at connect time")
	}
}

package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Regression tests for the E12c QA findings.

func deleteIndex(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestDODestroyFindsDropletWhenStateHasKeyAndFirewallButNoServer(t *testing.T) {
	f := newFakeDO(t)
	f.keyName = "can-acme-co"
	f.firewalls = []*doFirewall{{ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: ownedTags()}}
	f.droplets = []*doDroplet{{ID: 8080, Name: "can-acme-co", Tags: ownedTags()}}
	x := doFixture(t, f, readyRunner())
	if err := x.st.Mutate(func(s *State) error {
		s.Provider, s.SSHKeyID, s.SSHKeyOwned, s.FirewallUUID = "digitalocean", 5011, true, "fw-0000-aaaa"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if !f.droplets[0].deleted {
		t.Fatalf("droplet orphaned: %v", f.calls)
	}
	d, fw := deleteIndex(f.calls, "DELETE /droplets/"), deleteIndex(f.calls, "DELETE /firewalls/")
	if d < 0 || fw < 0 || d > fw {
		t.Fatalf("droplet must go before the firewall: %v", f.calls)
	}
}

func TestDODestroyWithStateButNoServerLeavesForeignDropletAlone(t *testing.T) {
	f := newFakeDO(t)
	f.firewalls = []*doFirewall{{ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: ownedTags()}}
	f.droplets = []*doDroplet{{ID: 8080, Name: "can-acme-co", Tags: []string{"prod"}}}
	x := doFixture(t, f, readyRunner())
	_ = x.st.Mutate(func(s *State) error { s.Provider, s.FirewallUUID = "digitalocean", "fw-0000-aaaa"; return nil })
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /droplets") != 0 {
		t.Fatalf("deleted a foreign droplet: %v", f.calls)
	}
}

func TestHetznerDestroyFindsServerWhenStateHasKeyAndFirewallButNoServer(t *testing.T) {
	f := newFakeHetzner(t)
	f.haveServer, f.haveFW, f.haveKey, f.keyOwned = true, true, true, true
	p, st, _ := hetznerFixture(t, f, readyRunner())
	if err := st.Mutate(func(s *State) error {
		s.Provider, s.SSHKeyID, s.SSHKeyOwned, s.FirewallID = "hetzner", 1011, true, 2022
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.haveServer {
		t.Fatalf("server orphaned: %v", f.calls)
	}
	s, fw := deleteIndex(f.calls, "DELETE /servers/"), deleteIndex(f.calls, "DELETE /firewalls/")
	if s < 0 || fw < 0 || s > fw {
		t.Fatalf("server must go before the firewall: %v", f.calls)
	}
}

func TestHetznerDestroyWithStateButNoServerLeavesForeignServerAlone(t *testing.T) {
	f := newFakeHetzner(t)
	f.haveServer, f.foreignServer, f.haveFW = true, true, true
	p, st, _ := hetznerFixture(t, f, readyRunner())
	_ = st.Mutate(func(s *State) error { s.Provider, s.FirewallID = "hetzner", 2022; return nil })
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /servers") != 0 {
		t.Fatalf("deleted a foreign server: %v", f.calls)
	}
}

func TestDOFirewallOwnershipIsStrict(t *testing.T) {
	cases := map[string]*doFirewall{
		"extra tag":        {ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: append(ownedTags(), "web")},
		"attached droplet": {ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: ownedTags(), Droplets: []int64{999}},
	}
	for name, fw := range cases {
		t.Run(name+" is not adopted", func(t *testing.T) {
			f := newFakeDO(t)
			f.firewalls = []*doFirewall{fw}
			x := doFixture(t, f, readyRunner())
			if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "not created by can") {
				t.Fatalf("got %v", err)
			}
			if f.count("POST /droplets") != 0 {
				t.Fatal("went on")
			}
		})
		t.Run(name+" is not deleted", func(t *testing.T) {
			f := newFakeDO(t)
			f.firewalls = []*doFirewall{fw}
			x := doFixture(t, f, readyRunner())
			_ = x.st.Mutate(func(s *State) error { s.Provider, s.FirewallUUID = "digitalocean", "fw-0000-aaaa"; return nil })
			if err := x.p.Destroy(context.Background(), doSpec(), x.st, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "refusing to delete firewall") {
				t.Fatalf("got %v", err)
			}
			if f.count("DELETE") != 0 {
				t.Fatal("deleted")
			}
		})
	}
}

func TestDOAdoptedFirewallRulesAreUpdatedWhenOperatorIPChanged(t *testing.T) {
	f := newFakeDO(t)
	old := (doConf{fwName: "can-acme-co", ownerTags: ownedTags()}).firewall().InboundRules // ssh open to all
	b, _ := json.Marshal(old)
	f.firewalls = []*doFirewall{{ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: ownedTags(), Inbound: b}}
	x := doFixture(t, f, readyRunner())
	spec := doSpec()
	spec.Options["operator_ip"] = "198.51.100.9"
	var out strings.Builder
	if err := x.p.Create(context.Background(), spec, x.st, &out); err != nil {
		t.Fatal(err)
	}
	if f.count("PUT /firewalls/fw-0000-aaaa") != 1 || !strings.Contains(string(f.bodies["PUT /firewalls/fw-0000-aaaa"]), "198.51.100.9/32") || !strings.Contains(out.String(), "updating") {
		t.Fatalf("not updated: %v out=%q", f.calls, out.String())
	}
}

func TestDOKeyDeleteVerifiesNameAndFingerprint(t *testing.T) {
	f := newFakeDO(t)
	f.keyName = "my-laptop" // someone renamed the key, or the id now points at another key
	x := doFixture(t, f, readyRunner())
	_ = x.st.Mutate(func(s *State) error { s.Provider, s.SSHKeyID, s.SSHKeyOwned = "digitalocean", 5012, true; return nil })
	var out strings.Builder
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &out); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /account/keys") != 0 || !strings.Contains(out.String(), "kept") {
		t.Fatalf("deleted a key whose name changed: %v %q", f.calls, out.String())
	}
}

func TestDOLostStateNeverClaimsAnExistingKey(t *testing.T) {
	f := newFakeDO(t)
	f.keyName = "can-acme-co"
	x := doFixture(t, f, readyRunner())
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE") != 0 {
		t.Fatalf("deleted: %v", f.calls)
	}
}

func TestOperatorIPValidation(t *testing.T) {
	for _, bad := range []string{"nonsense", "0.0.0.0/0", "::/0", "198.51.100.9/33", "0.0.0.0"[:0] + "300.1.1.1"} {
		if err := CheckOperatorIP(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"", "198.51.100.9", "198.51.100.0/24", "2001:db8::1", "2001:db8::/32"} {
		if err := CheckOperatorIP(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	x := doFixture(t, newFakeDO(t), readyRunner())
	spec := doSpec()
	spec.Options["operator_ip"] = "0.0.0.0/0"
	if err := x.p.Create(context.Background(), spec, x.st, io.Discard); err == nil || !strings.Contains(err.Error(), "leave operator_ip unset") {
		t.Fatalf("do: %v", err)
	}
	h := Hetzner{deps: testDeps(nil)}
	hs := hetznerSpec()
	hs.Options["operator_ip"] = "::/0"
	if _, err := h.conf(hs); err == nil {
		t.Fatal("hetzner accepted ::/0")
	}
	for _, typ := range []string{"ssh", "hetzner", "digitalocean"} {
		if len(CheckProviderOptions(typ, map[string]any{"operator_ip": "0.0.0.0/0"})) == 0 {
			t.Errorf("%s: validate accepted 0.0.0.0/0", typ)
		}
	}
}

func TestSecretRefValidation(t *testing.T) {
	for _, ok := range []string{"env:DIGITALOCEAN_TOKEN", "env:_X1", "file:/run/secrets/do", "file:~/.config/can/token"} {
		if !ValidSecretRef(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"env:", "env:dop_v1_0123456789abcdef", "env:HAS SPACE", "env:1ABC", "env:A-B", "file:", "file: /x", "file:/x\n", "plain", "env:" + strings.Repeat("A", 65)} {
		if ValidSecretRef(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	d := testDeps(nil)
	_, err := d.ResolveSecret("env:dop_v1_0123456789abcdef")
	if err == nil || strings.Contains(err.Error(), "0123456789") {
		t.Fatalf("token echoed: %v", err)
	}
	if len(CheckProviderOptions("digitalocean", map[string]any{"token_secret_ref": "env:dop_v1_0123456789abcdef"})) == 0 {
		t.Fatal("validate accepted a token as env name")
	}
}

func TestCloudflareRemoveLeavesRepointedRecords(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	ctx := context.Background()
	if err := x.p.Apply(ctx, "acme.example", cfRecs(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if state, _ := x.st.Load(); state.DNSRecords[0].Content != "203.0.113.7" {
		t.Fatalf("content not remembered: %+v", state.DNSRecords)
	}
	f.records[0].Content = "192.0.2.50" // an operator repointed the apex, comment untouched
	f.records[1].Proxied = true         // proxied changed: warn but delete
	var out strings.Builder
	if err := x.p.Remove(ctx, "acme.example", &out); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 || f.records[0].Name != "acme.example" || f.records[0].Content != "192.0.2.50" {
		t.Fatalf("records left: %+v", f.records)
	}
	if !strings.Contains(out.String(), "changed by an operator") || !strings.Contains(out.String(), "proxied was changed") {
		t.Fatalf("out %q", out.String())
	}
	if state, _ := x.st.Load(); len(state.DNSRecords) != 0 {
		t.Fatalf("state: %+v", state.DNSRecords)
	}
}

func TestCloudflareWarnsAboutRoundRobinRecords(t *testing.T) {
	f := newFakeCF(t)
	f.records = []*cfRecord{
		{ID: "o", Type: "A", Name: "acme.example", Content: "203.0.113.7", TTL: 300, Comment: "managed-by=can"},
		{ID: "x", Type: "A", Name: "acme.example", Content: "198.51.100.1", TTL: 300},
	}
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	recs, _ := Records("acme.example", "203.0.113.7", nil)
	var out strings.Builder
	if err := x.p.Apply(context.Background(), "acme.example", recs, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "round robin") || f.count("PUT") != 0 || f.records[1].Content != "198.51.100.1" {
		t.Fatalf("out %q calls %v", out.String(), f.calls)
	}
}

func TestCloudflareApplyResetsProxyToConfiguredValue(t *testing.T) {
	f := newFakeCF(t)
	f.records = []*cfRecord{{ID: "o", Type: "A", Name: "acme.example", Content: "203.0.113.7", TTL: 1, Proxied: true, Comment: "managed-by=can"}}
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	recs, _ := Records("acme.example", "203.0.113.7", nil)
	if err := x.p.Apply(context.Background(), "acme.example", recs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.records[0].Proxied || f.records[0].TTL != 300 {
		t.Fatalf("documented behaviour: proxied is set back to the configured value: %+v", f.records[0])
	}
}

func TestCloudflarePreflightReadsDNSRecords(t *testing.T) {
	f := newFakeCF(t)
	f.hook = func(r *http.Request) (int, string, http.Header) {
		if r.URL.Path == "/zones/zone1/dns_records" {
			return 403, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`, nil
		}
		return 0, "", nil
	}
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	err := x.p.Preflight(context.Background(), "acme.example")
	if err == nil || !strings.Contains(err.Error(), "cannot read DNS records") {
		t.Fatalf("got %v", err)
	}
}

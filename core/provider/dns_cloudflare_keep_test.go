package provider

import (
	"context"
	"io"
	"strings"
	"testing"
)

// QA: on a module's name, a foreign record of the wanted type is skipped even when can owns a
// record of another type there, and adopt_existing never overwrites it.
func TestCloudflareModuleNameForeignRecordBesideOwnedOtherType(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		f := newFakeCF(t)
		f.records = []*cfRecord{
			{ID: "own6", Type: "AAAA", Name: "jobs.acme.example", Content: "2001:db8::1", TTL: 300, Comment: "managed-by=can"},
			{ID: "x4", Type: "A", Name: "jobs.acme.example", Content: "192.0.2.9", TTL: 300, Comment: "mine"},
		}
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare", AdoptExisting: adopt})
		var out strings.Builder
		rec := Record{Type: "A", Name: "jobs.acme.example", Value: "203.0.113.7", TTL: 300, NoAdopt: true}
		if err := x.p.Apply(context.Background(), "acme.example", []Record{rec}, &out); err != nil {
			t.Fatalf("adopt=%v: %v", adopt, err)
		}
		if f.count("PUT") != 0 || f.count("POST") != 0 || !strings.Contains(out.String(), "skipped A jobs.acme.example") {
			t.Errorf("adopt=%v: %v %s", adopt, f.calls, out.String())
		}
	}
}

// QA: proxy and TTL an operator set stay, unless company.yaml says otherwise.
func TestCloudflareKeepsOperatorProxyAndTTL(t *testing.T) {
	rec := Record{Type: "A", Name: "acme.example", Value: "203.0.113.7", TTL: 300}
	owned := func(content string, ttl int, proxied bool) *fakeCF {
		f := newFakeCF(t)
		f.records = []*cfRecord{{ID: "r1", Type: "A", Name: "acme.example", Content: content, TTL: ttl, Proxied: proxied, Comment: "managed-by=can"}}
		return f
	}
	apply := func(f *fakeCF, dns map[string]any) string {
		t.Helper()
		dns["provider"] = "cloudflare"
		cfg, err := DNSConfigFrom(dns)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := cfFixture(t, f, cfg).p.Apply(context.Background(), "acme.example", []Record{rec}, io.MultiWriter(&out)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	for _, c := range []struct {
		name        string
		ttl         int
		proxied     bool
		dns         map[string]any
		wantTTL     int
		wantProxied bool
	}{
		{"proxy turned on in the dashboard", 1, true, map[string]any{}, 1, true},
		{"TTL changed in the dashboard", 3600, false, map[string]any{}, 3600, false},
		{"dns.proxied: false asks", 1, true, map[string]any{"proxied": false}, 300, false},
		{"dns.proxied: true asks", 3600, false, map[string]any{"proxied": true}, 1, true},
		{"dns.ttl asks", 3600, false, map[string]any{"ttl": 600}, 600, false},
	} {
		// the address is current, then it changes: neither run may flip what was not asked for
		for _, content := range []string{"203.0.113.7", "192.0.2.1"} {
			f := owned(content, c.ttl, c.proxied)
			out := apply(f, c.dns)
			r := f.records[0]
			if r.Content != "203.0.113.7" || r.TTL != c.wantTTL || r.Proxied != c.wantProxied {
				t.Errorf("%s (from %s): %+v\n%s", c.name, content, r, out)
			}
			if content == "203.0.113.7" && len(c.dns) == 0 && f.count("PUT") != 0 {
				t.Errorf("%s: wrote although nothing changed: %v", c.name, f.calls)
			}
		}
	}
	// adopting keeps the foreign record's TTL too
	f := newFakeCF(t)
	f.records = []*cfRecord{{ID: "x1", Type: "A", Name: "acme.example", Content: "192.0.2.1", TTL: 3600, Comment: "mine"}}
	apply(f, map[string]any{"adopt_existing": true})
	if r := f.records[0]; r.Content != "203.0.113.7" || r.TTL != 3600 || r.Proxied {
		t.Errorf("adopted: %+v", r)
	}
}

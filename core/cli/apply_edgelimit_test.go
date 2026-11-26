package cli

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/provider"
)

// QA: `can apply` keeps what `can up --no-edge-limits` switched off, and follows dns.proxied.
func TestApplyKeepsTheEdgeLimitChoice(t *testing.T) {
	cm := func(domain, limit, reason string) string {
		return `{"data":{"domain":"` + domain + `","tls":"true","siteHosts":"` + domain + `","edgeLimit":"` + limit + `","edgeLimitReason":"` + reason + `"}}`
	}
	site := `{"spec":{"entryPoints":["websecure"],"routes":[{"match":"Host(` + "`a.example`" + `)"}]}}`
	proxied := applyCompany + "dns: {provider: cloudflare, proxied: true}\n"
	for _, c := range []struct {
		name, company, facts string
		remembered           bool
		want                 []string // nil: the routing facts are not written
	}{
		{"flag, same domain", applyCompany, cm("a.example", "off", "flag"), false, nil},
		{"flag, domain changed", applyCompany, cm("old.example", "off", "flag"), false, []string{`edgeLimit: "off"`, "edgeLimitReason: flag"}},
		{"flag, ConfigMap gone", applyCompany, "", true, []string{`edgeLimit: "off"`, "edgeLimitReason: flag"}},
		{"flag turned off since, stale state elsewhere", applyCompany, cm("a.example", "on", ""), true, nil},
		{"dns.proxied added", proxied, cm("a.example", "on", ""), false, []string{`edgeLimit: "off"`, "edgeLimitReason: proxied"}},
		{"dns.proxied removed", applyCompany, cm("a.example", "off", "proxied"), false, []string{`edgeLimit: "on"`}},
		{"off with no recorded reason stays", applyCompany, cm("a.example", "off", ""), false, nil},
		{"off with no recorded reason stays on a new domain", applyCompany, cm("old.example", "off", ""), false, []string{`edgeLimit: "off"`}},
	} {
		f := &fileExec{facts: c.facts, site: site}
		useExec(t, f)
		dir := writeCompany(t, c.company)
		if c.remembered {
			if err := provider.NewStore(dir).Mutate(func(s *provider.State) error { s.NoEdgeLimits = true; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		code, out, errs := run(t, "apply", dir, "--modules-dir", "../../modules")
		if code != ExitOK {
			t.Fatalf("%s: %d %q %q", c.name, code, out, errs)
		}
		var facts string
		for _, m := range f.written {
			if strings.Contains(m, "name: can-routing") {
				facts = m
			}
		}
		if (facts != "") != (c.want != nil) {
			t.Fatalf("%s: facts written: %q", c.name, facts)
		}
		for _, w := range c.want {
			if !strings.Contains(facts, w) {
				t.Errorf("%s: lacks %s in %s", c.name, w, facts)
			}
		}
	}
}

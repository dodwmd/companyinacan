package installer

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/equity"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// linkFixture installs the real in-house charts into one fake cluster.
func linkFixture(t *testing.T) (*Installer, *kube.Fake, func(id string, linked bool) reconcile.Spec) {
	t.Helper()
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir = "../../../modules"
	return in, api, func(id string, linked bool) reconcile.Spec {
		t.Helper()
		s := spec
		s.ID, s.Namespace, s.ServiceAccount = id, "mod-"+id, id
		s.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
		s.Manifest.Requires.Database = "none"
		if linked {
			s.Links = testLinks(id)
		}
		if err := in.Enable(context.Background(), s); err != nil {
			t.Fatalf("enable %s: %v", id, err)
		}
		return s
	}
}

func TestEquityIsConnectedToPeopleAndDisconnected(t *testing.T) {
	_, api, enable := linkFixture(t)
	enable("people", false)
	enable("equity", true)

	egress := toJSON(dig(get(t, api, "NetworkPolicy", "mod-equity", "can-wire-to-people"), "spec"))
	ingress := toJSON(dig(get(t, api, "NetworkPolicy", "mod-people", "can-wire-from-equity"), "spec"))
	for what, c := range map[string][2]string{
		"egress":  {egress, `{"egress":[{"ports":[{"port":8081,"protocol":"TCP"}],"to":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"mod-people"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"people"}}}]}],"podSelector":{"matchLabels":{"app.kubernetes.io/name":"equity"}},"policyTypes":["Egress"]}`},
		"ingress": {ingress, `{"ingress":[{"from":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"mod-equity"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"equity"}}}],"ports":[{"port":8081,"protocol":"TCP"}]}],"podSelector":{"matchLabels":{"app.kubernetes.io/name":"people"}},"policyTypes":["Ingress"]}`},
	} {
		if c[0] != c[1] {
			t.Errorf("%s:\n%s\nwant\n%s", what, c[0], c[1])
		}
	}
	tok := secretValue(t, api, "mod-people", modsecrets.PeopleLookupKey)
	if len(tok) < 24 || tok == secretValue(t, api, "mod-people", modsecrets.PeopleDirectoryKey) {
		t.Fatal("people has no separate lookup token")
	}
	if secretValue(t, api, "mod-equity", modsecrets.EquityDirectoryKey) != tok {
		t.Fatal("equity does not hold the lookup token")
	}
	conf := func() *equity.Config {
		t.Helper()
		text, _ := dig(get(t, api, "ConfigMap", "mod-equity", "equity-config"), "data", "equity.yaml").(string)
		c, err := equity.ParseConfig([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := conf().Directory.URL; got != "http://people-api.mod-people.svc.cluster.local:8081" {
		t.Fatalf("directory.url = %q", got)
	}

	// people disabled: equity is applied without the connection and keeps nothing of it.
	enable("equity", false)
	for _, p := range [][2]string{{"mod-equity", "can-wire-to-people"}, {"mod-people", "can-wire-from-equity"}} {
		if exists(api, "NetworkPolicy", p[0], p[1]) {
			t.Errorf("%s/%s stayed", p[0], p[1])
		}
	}
	if conf().Directory.URL != "" || secretValue(t, api, "mod-equity", modsecrets.EquityDirectoryKey) != "" {
		t.Error("equity keeps the directory after the disconnect")
	}
}

// Disabling either side removes both policies at once, before any consumer is applied again.
func TestDisableRemovesConnections(t *testing.T) {
	for _, off := range []string{"people", "equity"} {
		t.Run(off, func(t *testing.T) {
			in, api, enable := linkFixture(t)
			enable("people", false)
			enable("equity", true)
			enable("provisioning", true)
			if err := in.Disable(context.Background(), off); err != nil {
				t.Fatal(err)
			}
			if exists(api, "NetworkPolicy", "mod-equity", "can-wire-to-people") || exists(api, "NetworkPolicy", "mod-people", "can-wire-from-equity") {
				t.Error("equity is still connected")
			}
			other := exists(api, "NetworkPolicy", "mod-provisioning", "can-wire-to-people") && exists(api, "NetworkPolicy", "mod-people", "can-wire-from-provisioning")
			if want := off == "equity"; other != want {
				t.Errorf("provisioning connected = %v, want %v", other, want)
			}
			if off == "equity" && secretValue(t, api, "mod-equity", modsecrets.EquityDirectoryKey) != "" {
				t.Error("a disabled consumer keeps the token")
			}
		})
	}
}

func TestOnlyGrantedModulesAreConnected(t *testing.T) {
	in, api, enable := linkFixture(t)
	enable("people", false)
	enable("work", false)
	for _, o := range api.Objects {
		if strings.HasPrefix(nameOf(o), wirePrefix) {
			t.Errorf("unexpected %s", nameOf(o))
		}
	}
	s := enable("work", false)
	s.Links = testLinks("equity")
	if err := in.Enable(context.Background(), s); err == nil || !strings.Contains(err.Error(), "may not be connected") {
		t.Fatalf("work was connected to people: %v", err)
	}
}

func TestAChartCannotNameAConnectionPolicy(t *testing.T) {
	in, _, _, spec := fixture(t, map[string]string{"stub/chart/templates/np.yaml": "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: can-wire-to-people\nspec:\n  podSelector: {matchLabels: {app: stub}}\n  policyTypes: [Egress]\n  egress: [{to: [{podSelector: {matchLabels: {app: stub}}}]}]\n"})
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "can-core owns it") {
		t.Fatalf("a chart defined a can-wire policy: %v", err)
	}
}

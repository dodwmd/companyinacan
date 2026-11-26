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

// noRights answers as the API server does for a namespace can-core has no RoleBinding in.
type noRights struct {
	*kube.Fake
	ns string
}

func (n noRights) Get(ctx context.Context, kind, ns, name string) (kube.Object, error) {
	if ns == n.ns {
		return nil, &kube.StatusError{Code: 403, Reason: "Forbidden"}
	}
	return n.Fake.Get(ctx, kind, ns, name)
}

// Planning to enable people where equity runs: mod-people does not exist yet.
func TestFingerprintBeforeTheProviderExists(t *testing.T) {
	in, api, enable := linkFixture(t)
	s := enable("equity", false)
	s.Links = testLinks("equity")
	in.cfg.API = noRights{Fake: api, ns: "mod-people"}
	if f, err := in.WiringFingerprint(context.Background(), s); err != nil || f != "" {
		t.Fatalf("fingerprint %q, %v", f, err)
	}
}

// A restore reconnects nothing and reads nothing of the provider: it keeps the seeded credential.
func TestRestoreKeepsTheSeededCredential(t *testing.T) {
	in, api, _ := linkFixture(t)
	const seeded = "seeded-lookup-token-seeded-lookup-token"
	in.cfg.Quiesce, in.cfg.API = true, noRights{Fake: api, ns: "mod-people"}
	in.cfg.Seed = func(ctx context.Context, s reconcile.Spec) error {
		return api.Apply(ctx, secret(s.Namespace, secretModule, s.ID, map[string]string{modsecrets.EquityDirectoryKey: seeded}))
	}
	_, _, _, spec := fixture(t, nil)
	spec.ID, spec.Namespace, spec.ServiceAccount = "equity", "mod-equity", "equity"
	spec.Manifest = loadManifest(t, "../../../modules/equity/module.yaml")
	spec.Manifest.Requires.Database, spec.Links = "none", testLinks("equity")
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := secretValue(t, api, "mod-equity", modsecrets.EquityDirectoryKey); got != seeded {
		t.Errorf("credential %q", got)
	}
	if exists(api, "NetworkPolicy", "mod-equity", "can-wire-to-people") {
		t.Error("a restore made the connection")
	}
}

// identity: directory without the connection is refused: equity must not fall back to attestation.
func TestDirectoryModeNeedsTheConnection(t *testing.T) {
	in, api, _ := linkFixture(t)
	_, _, _, spec := fixture(t, nil)
	spec.ID, spec.Namespace, spec.ServiceAccount = "equity", "mod-equity", "equity"
	spec.Manifest = loadManifest(t, "../../../modules/equity/module.yaml")
	spec.Manifest.Requires.Database, spec.Settings = "none", map[string]any{"identity": "directory"}
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "change the setting back") {
		t.Fatalf("installed without the connection: %v", err)
	}
	spec.Links = testLinks("equity") // connected, but people has made no credential
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "enable the people module") {
		t.Fatalf("installed without the credential: %v", err)
	}
	if exists(api, "Deployment", "mod-equity", "equity") {
		t.Error("equity was started in the other mode")
	}
}

func TestBreakGlassComesFromCompanySettings(t *testing.T) {
	in, api, _ := linkFixture(t)
	_, _, _, spec := fixture(t, nil)
	spec.ID, spec.Namespace, spec.ServiceAccount = "equity", "mod-equity", "equity"
	spec.Manifest = loadManifest(t, "../../../modules/equity/module.yaml")
	spec.Manifest.Requires.Database = "none"
	env := func() string {
		t.Helper()
		return toJSON(dig(get(t, api, "Deployment", "mod-equity", "equity"), "spec", "template", "spec", "containers"))
	}
	spec.Settings = map[string]any{"break_glass_until": "2026-12-01"}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(env(), `{"name":"CAN_EQUITY_BREAK_GLASS_UNTIL","value":"2026-12-01"}`) {
		t.Errorf("env: %s", env())
	}
	spec.Settings = nil
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env(), "BREAK_GLASS") {
		t.Error("the window stays after the setting is removed")
	}
}

func TestAChartCannotNameAConnectionPolicy(t *testing.T) {
	in, _, _, spec := fixture(t, map[string]string{"stub/chart/templates/np.yaml": "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: can-wire-to-people\nspec:\n  podSelector: {matchLabels: {app: stub}}\n  policyTypes: [Egress]\n  egress: [{to: [{podSelector: {matchLabels: {app: stub}}}]}]\n"})
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "can-core owns it") {
		t.Fatalf("a chart defined a can-wire policy: %v", err)
	}
}

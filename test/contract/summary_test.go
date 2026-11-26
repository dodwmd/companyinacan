package contract

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/server/overview"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
)

// The keys of summary.v1 are the ones can-core knows how to show, each module that owns one offers
// the capability on a service and port it already serves, and nobody else does.
func TestSummaryContract(t *testing.T) {
	var top map[string]any
	if err := json.Unmarshal(capabilities.SummaryV1, &top); err != nil {
		t.Fatal(err)
	}
	keys := strs(asMap(asMap(asMap(asMap(top["$defs"])["Figure"])["properties"])["key"])["enum"])
	var known, owners []string
	for _, f := range overview.Figures {
		known = append(known, f.Key)
		if !slices.Contains(owners, f.Module) {
			owners = append(owners, f.Module)
		}
	}
	slices.Sort(keys)
	slices.Sort(known)
	if strings.Join(keys, " ") != strings.Join(known, " ") {
		t.Errorf("summary.v1 keys %v differ from can-core's %v", keys, known)
	}

	for _, id := range []string{"analytics", "board", "crm", "entity", "equity", "files", "finance", "forms", "jobs", "newsletter", "people", "provisioning", "status", "support", "wiki", "work"} {
		m := readYAML(t, "modules/"+id+"/module.yaml")
		served, offers := map[string]bool{}, ""
		for _, p := range asList(m["provides"]) {
			pm := asMap(p)
			at := asStr(pm["service"]) + ":" + toString(pm["port"])
			if asStr(pm["capability"]) == modclient.SummaryCapability {
				offers = at
				if asStr(pm["schema"]) != "summary.v1" {
					t.Errorf("%s: summary schema %v", id, pm["schema"])
				}
				continue
			}
			served[at] = true
		}
		if (offers != "") != slices.Contains(owners, id) {
			t.Errorf("%s: offers summary = %v, but can-core's keys say %v", id, offers != "", slices.Contains(owners, id))
		}
		if offers != "" && !served[offers] {
			t.Errorf("%s: summary is served at %s, which no other provision of the module uses", id, offers)
		}
	}
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSummaryAnswerShape(t *testing.T) {
	s := compileSchema(t, "summary.v1.json", capabilities.SummaryV1, "#/$defs/Summary")
	good, err := json.Marshal(modclient.Summary{Version: modclient.SummaryVersion, Figures: []modclient.SummaryFigure{
		{Key: "invoices.overdue", Count: 2, Amounts: []modclient.SummaryAmount{{Currency: "AUD", Minor: 12345}}}, {Key: "leads.new"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJSON(t, s, good); err != nil {
		t.Errorf("a real answer is rejected: %v", err)
	}
	// No free text can travel: an unknown key, an extra property or a name instead of a currency.
	for _, bad := range []string{
		`{"version":1,"figures":[{"key":"people.names","count":1}]}`,
		`{"version":1,"figures":[{"key":"leads.new","count":1,"label":"Ada Lovelace"}]}`,
		`{"version":1,"figures":[{"key":"invoices.overdue","count":1,"amounts":[{"currency":"Ada","minor":1}]}]}`,
		`{"version":1,"figures":[],"note":"x"}`,
		`{"version":2,"figures":[]}`,
		`{"version":1,"figures":[{"key":"leads.new","count":-1}]}`,
	} {
		if validateJSON(t, s, []byte(bad)) == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
}

// In the compose stack can-core reaches each module that offers a summary by its cluster name.
func TestSummaryComposeAliases(t *testing.T) {
	compose := asMap(readYAML(t, "docker-compose.yml")["services"])
	for id := range asMap(readYAML(t, "dev/compose/config/company.yaml")["modules"]) {
		for _, p := range asList(readYAML(t, "modules/"+id+"/module.yaml")["provides"]) {
			pm := asMap(p)
			if asStr(pm["capability"]) != modclient.SummaryCapability {
				continue
			}
			host := asStr(pm["service"]) + ".mod-" + id + ".svc.cluster.local"
			if !slices.Contains(strs(asMap(asMap(asMap(compose[id])["networks"])["default"])["aliases"]), host) {
				t.Errorf("compose service %s has no network alias %s", id, host)
			}
		}
	}
}

// can-core formats money without importing the finance module: the two must agree.
func TestSummaryMoneyExponents(t *testing.T) {
	for _, c := range []string{"AUD", "USD", "EUR", "GBP", "JPY", "KRW", "CLP", "VND", "ISK", "BHD", "KWD", "OMR", "JOD", "TND", "CHF", "INR"} {
		exp, ok := finance.CurrencyExponent(c)
		if !ok {
			t.Fatalf("%s unknown to finance", c)
		}
		want := c + " " + finance.FormatMinor(123456, c)
		if got := overview.FormatMoney(c, 123456); got != want {
			t.Errorf("%s (exponent %d): %q, finance writes %q", c, exp, got, want)
		}
	}
}

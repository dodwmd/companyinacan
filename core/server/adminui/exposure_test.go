package adminui

// The module page's "Internet access" section (specs/045-public-access).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func TestModulePageShowsWhatIsExposed(t *testing.T) {
	m := newMM(t)
	s := m.loginAs("viewer")
	body := m.do("GET", "/modules/web", nil, s.cookie).body
	for _, want := range []string{"Internet access", "Public pages", "https://web.acme.example", "Change this in company.yaml"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}

	// switched off in the file: no address is offered
	must(t, os.WriteFile(m.store.Path, []byte(mmCompany+"    expose: {public: false}\n"), 0o644))
	body = m.do("GET", "/modules/web", nil, s.cookie).body
	if strings.Contains(body, "https://web.acme.example") || !strings.Contains(body, ">Off<") {
		t.Errorf("public: false:\n%s", body)
	}
}

func TestExposureRowsFollowTheManifest(t *testing.T) {
	m := newMM(t)
	man := &config.Module{}
	man.Runtime.PublicUI = &config.PublicUI{}
	man.Runtime.PublicLink = &config.PublicLink{Prefix: "/public/invoices/"}
	must(t, os.WriteFile(m.store.Path, []byte("schema: 1\nname: Acme\ndomain: acme.example\nprovider:\n  type: local\nmodules:\n  finance:\n    expose: {ui: true, public: true}\n"), 0o644))
	rows := func() map[string]string {
		out := map[string]string{}
		for _, r := range m.ui.exposure(context.Background(), "finance", man).Rows {
			out[r.Label] = r.State + " " + r.URL
		}
		return out
	}
	want := map[string]string{
		"Web UI":        "On, not routed yet https://finance.acme.example",
		"API":           "Off ",
		"Invoice links": "On https://finance.acme.example/public/invoices/",
	}
	if got := rows(); len(got) != len(want) {
		t.Fatalf("%v", got)
	} else {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %q, want %q", k, got[k], v)
			}
		}
	}
	man.Runtime.RoutedUIHost = "finance.acme.example"
	if got := rows()["Web UI"]; got != "On https://finance.acme.example" {
		t.Errorf("routed: %q", got)
	}
}

package contract

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/validate"
)

// The forms assumed when no modules directory is at hand are the ones the forms chart ships.
func TestShippedFormsMatchTheFormsChart(t *testing.T) {
	got, err := config.FormsOfModules(filepath.Join("..", "..", "modules"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, config.ShippedForms()) {
		t.Fatalf("modules/forms/chart/values.yaml has %+v, config.ShippedForms %+v", got, config.ShippedForms())
	}
	if none, err := config.FormsOfModules(t.TempDir()); err != nil || !reflect.DeepEqual(none, config.ShippedForms()) {
		t.Fatalf("without the module: %+v %v", none, err)
	}
}

func TestValidateChecksTheContactForm(t *testing.T) {
	const head = "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n"
	check := func(body string) []problem.Problem {
		return validate.CompanyBytes("company.yaml", []byte(head+body), filepath.Join("..", "..", "modules"))
	}
	const emailed = "modules: {forms: {settings: {notify_email: [hello@a.example]}}}\n"
	if ps := check(emailed + "site: {contact: {}}\n"); len(ps) != 0 {
		t.Fatalf("a contact section with a recipient: %v", ps)
	}
	if ps := check("modules: {forms: {}}\nsite: {contact: {}}\n"); problem.HasErrors(ps) || len(ps) != 1 || ps[0].Rule != "semantic.contact-no-notify" {
		t.Fatalf("without a recipient: %v", ps)
	}
	ps := check("modules: {forms: {}}\nsite: {contact: {form: nope}}\n")
	if !problem.HasErrors(ps) || ps[0].Path != "/site/contact/form" || !strings.Contains(ps[0].Message, `no form "nope"`) {
		t.Fatalf("unknown form: %v", ps)
	}
	for body, n := range map[string]int{"site: {contact: {}}\n": 2, "modules: {forms: {enabled: false}}\nsite: {contact: {}}\n": 1} {
		ps = check(body)
		if problem.HasErrors(ps) || len(ps) != n || ps[0].Severity != problem.Warning || !strings.Contains(ps[0].Message, "forms") {
			t.Errorf("%q: want %d warnings, got %v", body, n, ps)
		}
	}
	if ps := check("modules: {forms: {}}\nsite: {contact: {href: /x}}\n"); !problem.HasErrors(ps) {
		t.Error("an unknown key under site.contact was accepted")
	}
}

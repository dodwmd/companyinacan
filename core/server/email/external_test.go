package email

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// external_recipients (specs/049-contract-esign) opens the allow-list for finance only.
func TestExternalRecipientsIsFinanceOnly(t *testing.T) {
	e := &config.Email{Modules: map[string]config.EmailModule{
		"finance": {ExternalRecipients: true}, "forms": {ExternalRecipients: true}, "jobs": {},
	}}
	p, err := Source{Domain: "acme.example"}.For(&config.Company{Domain: "acme.example", Email: e})
	if err != nil {
		t.Fatal(err)
	}
	for module, want := range map[string]bool{"finance": true, "forms": false, "jobs": false} {
		if got := p.Rules(module).AnyRecipient; got != want {
			t.Errorf("%s: %v", module, got)
		}
	}
	issues := config.ValidateEmail(e)
	if len(issues) != 1 || issues[0].Path != "/email/modules/forms/external_recipients" {
		t.Errorf("%v", issues)
	}
}

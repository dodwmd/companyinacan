package validate

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/problem"
)

// These settings were accepted and ignored before specs/059: none of them may start refusing a company.
func TestSupportSettingsNeverRefuseACompany(t *testing.T) {
	y := `schema: 1
name: Acme
domain: acme.example
provider: {type: local}
modules:
  support:
    settings:
      support_address: admin@acme.example
      retention_days: 90
      inbound: {mode: imap, imap: {host: "imaps://x", port: 0, encryption: none}}
`
	ps := CompanyBytes("company.yaml", []byte(y), "../../modules")
	warned := 0
	for _, p := range ps {
		if p.Severity == problem.Error {
			t.Errorf("refused: %s", p.String())
		}
		if p.Rule == "semantic.support-mail" {
			warned++
		}
	}
	if warned < 3 {
		t.Errorf("only %d warnings: %v", warned, ps)
	}
}

package finance

import (
	"encoding/json"
	"testing"
)

var (
	acct    = []string{"finance.read", "finance.write", "finance.ledger.read", "finance.ledger.write"}
	ledAdm  = []string{"finance.read", "finance.ledger.read", "finance.ledger.write", "finance.ledger.admin"}
	ledApp  = []string{"finance.read", "finance.ledger.read", "finance.ledger.approve"}
	sealHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// ledgerOn applies the default chart and mappings and creates a VAT rate, using the bootstrap token.
func ledgerOn(t *testing.T, e *env) {
	t.Helper()
	e.must(201, adminTok, "POST", "/v1/tax-rates", obj{"code": "VAT", "name": "VAT", "rate_ppm": 73000}, nil)
	e.must(200, adminTok, "POST", "/v1/ledger/accounts/template", obj{"apply_mappings": true}, nil)
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

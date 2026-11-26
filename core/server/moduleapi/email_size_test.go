package moduleapi

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/email"
)

// QA of #52, 7: a text of the allowed size must fit the request however its characters are escaped: Go's JSON
// encoder writes <, > and & as six bytes each.
func TestEmailTextFullOfEscapedCharactersIsAccepted(t *testing.T) {
	h := emailHarness(t, emailCompany)
	body := map[string]any{"to": []string{"sales@acme.example"}, "subject": "Hello", "text": strings.Repeat("<>&", email.MaxTextBytes/3)}
	if r, b := h.call("POST", "/v1/email", "tok-crm", body); r.StatusCode != 202 {
		t.Fatal(r.StatusCode, string(b))
	}
}

package verify

import (
	"strings"
	"testing"
)

func TestSupportMailChecks(t *testing.T) {
	line := `{"config":{"state":"ok"},"fetch":{"state":"failing","detail":"IMAP: the server refused the user name or password"},"send":{"state":"not_configured","detail":"no relay"}}`
	s, ok := ParseMailStatus("warning: something\n\n" + line + "\n")
	if !ok || !s.Failing() || s.Fetch.Detail == "" {
		t.Fatalf("%v %+v", ok, s)
	}
	cs := evalSupportMail(Facts{"support_mail": {line}}, ServerOptions{SupportMail: true})
	var got []string
	for _, c := range cs {
		got = append(got, c.ID+"="+string(c.Status))
		if strings.Contains(c.Detail, "password") {
			t.Errorf("the report repeats what the module said: %s", c.Detail)
		}
	}
	want := "support.mail-config=" + string(Pass) + " support.mail-fetch=" + string(Fail) + " support.mail-send=" + string(Skip)
	if strings.Join(got, " ") != want {
		t.Errorf("%v, want %s", got, want)
	}
	if cs := evalSupportMail(Facts{}, ServerOptions{SupportMail: true}); len(cs) != 3 || cs[0].Status != Fail {
		t.Errorf("no report is a failure: %v", cs)
	}
	if cs := evalSupportMail(Facts{}, ServerOptions{}); cs != nil {
		t.Errorf("no support inbox, nothing to say: %v", cs)
	}
	for _, bad := range []string{"", "ok", `{"config":{"state":"great"},"fetch":{"state":"ok"},"send":{"state":"ok"}}`, `{"config":` + strings.Repeat("x", 5000)} {
		if _, ok := ParseMailStatus(bad); ok {
			t.Errorf("accepted %.60q", bad)
		}
	}
}

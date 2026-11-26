package moduleapi

import (
	"context"
	"strings"
	"testing"
)

// A summary grant is introspected like a token (specs/053-business-overview): once, by the module
// it was minted for, as kind "summary" and never "user".
func TestIntrospectSummaryGrant(t *testing.T) {
	e := newTicketEnv(t)
	grant, err := e.svc.MintSummary(context.Background(), "user:alice", "work")
	if err != nil {
		t.Fatal(err)
	}
	ask := func(module, audience string) string {
		status, body := e.call("/v1/tokens/introspect", "tok-"+module, map[string]string{"token": grant, "audience": audience})
		if status != 200 {
			t.Fatalf("%d %s", status, body)
		}
		return body
	}
	for name, body := range map[string]string{
		"another module":                   ask("people", "people"),
		"another module naming the target": ask("people", "work"),
		"the wrong audience":               ask("work", "people"),
	} {
		if body != `{"active":false}` {
			t.Errorf("%s: %s", name, body)
		}
	}
	if status, _ := e.call("/v1/tokens/introspect", "", map[string]string{"token": grant}); status != 401 {
		t.Errorf("anonymous: %d", status)
	}
	body := ask("work", "work")
	for _, want := range []string{`"active":true`, `"kind":"summary"`, `"subject":"user:alice"`, `"permissions":["work.read"]`, `"expires_at"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s: %s", want, body)
		}
	}
	if body := ask("work", "work"); body != `{"active":false}` {
		t.Errorf("answered twice: %s", body)
	}
}

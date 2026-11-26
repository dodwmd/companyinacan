package provision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// QA: a 200 that is not a SCIM ListResponse is not "no account": a leaver whose account was never
// looked at must not be reported done.
func TestSCIMLookupNeedsAListResponse(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	c := &SCIMClient{ConnName: "idp", Base: srv.URL, Token: "t", HTTP: NewOutboundClient(true, 5*time.Second)}
	id := Identity{UserName: "ann@example.com", PartyID: "pers_0000000000000000000000c1"}
	ctx := context.Background()
	for _, b := range []string{`{}`, `{"status":"ok"}`, `{"Resources":[]}`, `{"totalResults":null,"Resources":[]}`, `{"totalResults":1}`, `{"totalResults":-1}`, `[]`} {
		body = b
		if ext, err := c.FindAccount(ctx, id); err == nil {
			t.Errorf("%s: read as an answer (%q)", b, ext)
		} else if code, perm := errCode(err); code != "bad_response" || !perm {
			t.Errorf("%s: %v", b, err)
		}
		if err := c.RemoveFromGroup(ctx, "", "u1", "staff"); err == nil {
			t.Errorf("%s: group removal passed as done", b)
		}
	}
	body = `{"totalResults":0}` // some servers leave Resources out of an empty list
	if ext, err := c.FindAccount(ctx, id); err != nil || ext != "" {
		t.Errorf("empty list: %q %v", ext, err)
	}
}

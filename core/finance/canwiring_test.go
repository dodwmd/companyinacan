package finance

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

const finPAT = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

func canEnv(t *testing.T) (*env, *fakecore.Core) {
	t.Helper()
	core := fakecore.New(t, "finance")
	e := newEnv(t, func(c *Config, o *Options) {
		c.Webhook.URL = "" // events go through can-core only
		o.Can = core.Client(t)
	})
	return e, core
}

func TestPersonalAccessTokenAuthenticatesAndRespectsItsPermissions(t *testing.T) {
	e, core := canEnv(t)
	core.AddToken(finPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{PermRead}})
	if code := e.do(finPAT, "GET", "/v1/parties", nil, nil); code != 200 {
		t.Fatalf("read with a read token = %d", code)
	}
	if code := e.do(finPAT, "POST", "/v1/parties", obj{"kind": "customer", "name": "Acme"}, nil); code != 403 {
		t.Fatalf("write with a read-only token = %d", code)
	}
	core.AddToken(finPAT+"W", fakecore.Token{Subject: "user:bob", Permissions: []string{PermWrite}})
	if code := e.do(finPAT+"W", "POST", "/v1/parties", obj{"kind": "customer", "name": "Acme"}, nil); code != 201 {
		t.Fatalf("write with a write token = %d", code)
	}
}

func TestUnknownForeignAndPermissionlessTokensAreRefused(t *testing.T) {
	e, core := canEnv(t)
	core.AddToken(finPAT+"F", fakecore.Token{Subject: "user:eve", Permissions: []string{PermAdmin}, Module: "people"})
	core.AddToken(finPAT+"N", fakecore.Token{Subject: "user:bob", Permissions: []string{"work.admin"}})
	for name, tok := range map[string]string{"unknown": finPAT + "?", "foreign": finPAT + "F", "no finance permission": finPAT + "N"} {
		if code := e.do(tok, "GET", "/v1/parties", nil, nil); code != 401 {
			t.Errorf("%s = %d", name, code)
		}
	}
	core.SetDown(true)
	core.AddToken(finPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	if code := e.do(finPAT, "GET", "/v1/parties", nil, nil); code != 401 {
		t.Fatalf("can-core down must refuse personal tokens, got %d", code)
	}
	if code := e.do(adminTok, "GET", "/v1/parties", nil, nil); code != 200 {
		t.Fatalf("bootstrap token = %d", code)
	}
}

func TestFinanceEventsGoThroughCanCore(t *testing.T) {
	e, core := canEnv(t)
	core.AddToken(finPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	var p struct {
		ID int64 `json:"id"`
	}
	e.must(201, finPAT, "POST", "/v1/parties", obj{"kind": "customer", "name": "Acme"}, &p)
	if e.app.dispatcher == nil {
		t.Fatal("no dispatcher under can-core")
	}
	_, _ = e.app.dispatcher.DeliverDue(context.Background()) // nothing queued yet: parties emit no event
	if n := len(core.Published()); n != 0 {
		t.Fatalf("%d events for a party create", n)
	}
}

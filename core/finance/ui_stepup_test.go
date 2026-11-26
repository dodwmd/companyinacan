package finance

import (
	"net/url"
	"testing"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

// QA: the erasure step-up of a token session asks can-core again. A token revoked a moment ago is
// still in the introspection cache, and must not erase.
func TestUIErasureStepUpBypassesTheIntrospectionCache(t *testing.T) {
	core := fakecore.New(t, "finance")
	u := newUIEnv(t, func(c *Config, o *Options) {
		c.Webhook.URL = ""
		o.Can = core.Client(t)
	})
	core.AddToken(finPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	adm := u.login(t, finPAT)
	r := adm.post("/parties", url.Values{"kind": {"customer"}, "name": {"Acme Pty"}, "currency": {"USD"}})
	status(t, r, 303)
	party := locID(t, r.Header().Get("Location"), "/parties/")
	erase := "/parties/" + party + "/erase"
	status(t, adm.get(erase), 200)

	core.Revoke(finPAT)
	status(t, adm.get(erase), 200) // an ordinary page still takes the cached answer
	status(t, adm.post(erase, url.Values{"confirm": {"ERASE"}, "token": {finPAT}}), 422)
	if p, err := u.app.store.GetParty(t.Context(), &Principal{Permissions: []string{PermAdmin}}, int64(mustAtoi(t, party))); err != nil || p.Erased {
		t.Fatalf("erased with a revoked token: %+v %v", p, err)
	}

	core.AddToken(finPAT+"2", fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	adm = u.login(t, finPAT+"2")
	status(t, adm.get(erase), 200)
	status(t, adm.post(erase, url.Values{"confirm": {"ERASE"}, "token": {finPAT + "2"}}), 200)
}

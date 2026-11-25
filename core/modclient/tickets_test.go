package modclient_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

func TestRedeemAndRevalidate(t *testing.T) {
	core := fakecore.New(t, "work")
	cl := core.Client(t)
	ctx := context.Background()
	tk := core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice", Permissions: []string{"work.read"}, ReturnPath: "/projects"})
	s, err := cl.RedeemTicket(ctx, tk, core.Binding(tk))
	if err != nil || !s.Active || s.Subject != "user:alice" || s.Name != "Alice" || s.ReturnPath != "/projects" || !strings.HasPrefix(s.Session, modclient.HandlePrefix) || s.ExpiresAt.IsZero() {
		t.Fatalf("%+v %v", s, err)
	}
	// One use.
	if s2, err := cl.RedeemTicket(ctx, tk, core.Binding(tk)); err != nil || s2.Active || s2.Session != "" {
		t.Fatalf("replay: %+v %v", s2, err)
	}
	v, err := cl.RevalidateSession(ctx, s.Session)
	if err != nil || !v.Active || v.Subject != "user:alice" || len(v.Permissions) != 1 {
		t.Fatalf("%+v %v", v, err)
	}
	core.SetSessionPermissions("user:alice", []string{"work.read", "work.write"})
	if v, _ = cl.RevalidateSession(ctx, s.Session); len(v.Permissions) != 2 {
		t.Fatalf("permissions did not follow: %+v", v)
	}
	core.EndSessions("user:alice")
	if v, err = cl.RevalidateSession(ctx, s.Session); err != nil || v.Active {
		t.Fatalf("an ended session is active: %+v %v", v, err)
	}
}

func TestPeekAndTheLaunchCookie(t *testing.T) {
	core := fakecore.New(t, "work")
	cl := core.Client(t)
	ctx := context.Background()
	tk := core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice", Permissions: []string{"work.read"}})
	if p, err := cl.PeekTicket(ctx, tk, modclient.BindingPrefix+strings.Repeat("A", 43)); err != nil || p.Active {
		t.Fatalf("peek with another browser's nonce: %+v %v", p, err)
	}
	if p, err := cl.PeekTicket(ctx, tk, core.Binding(tk)); err != nil || !p.Active || p.Subject != "user:alice" || p.Session != "" {
		t.Fatalf("%+v %v", p, err)
	}
	if s, err := cl.RedeemTicket(ctx, tk, core.Binding(tk)); err != nil || !s.Active {
		t.Fatalf("peek consumed the ticket: %+v %v", s, err)
	}
	// Over https only the __Secure- name counts: the plain one could be planted over http.
	r := httptest.NewRequest("GET", "/", nil)
	plain := modclient.BindingPrefix + strings.Repeat("p", 43)
	r.AddCookie(&http.Cookie{Name: modclient.LaunchCookieName("work", false), Value: plain})
	if got := modclient.LaunchBinding(r, "work", true); got != "" {
		t.Fatalf("a secure UI read %q", got)
	}
	if got := modclient.LaunchBinding(r, "work", false); got != plain {
		t.Fatalf("a plain-http UI read %q", got)
	}
	r.AddCookie(&http.Cookie{Name: modclient.LaunchCookieName("work", true), Value: core.Binding(tk)})
	if got := modclient.LaunchBinding(r, "work", false); got != core.Binding(tk)+" "+plain {
		t.Fatalf("the secure cookie is tried first: %q", got)
	}
	if got := modclient.LaunchBinding(r, "people", false); got != "" {
		t.Fatalf("another module's cookie: %q", got)
	}
}

// A nonce planted by another host of the company domain arrives ahead of the genuine one: the
// genuine one is still tried, for peek and for redeem.
func TestAPlantedLaunchCookieDoesNotHideTheGenuineOne(t *testing.T) {
	core := fakecore.New(t, "work")
	cl := core.Client(t)
	ctx := context.Background()
	tk := core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{"work.read"}})
	r := httptest.NewRequest("GET", "/", nil)
	name := modclient.LaunchCookieName("work", true)
	r.AddCookie(&http.Cookie{Name: name, Value: modclient.BindingPrefix + strings.Repeat("p", 43)})
	r.AddCookie(&http.Cookie{Name: name, Value: core.Binding(tk)})
	b := modclient.LaunchBinding(r, "work", true)
	if p, err := cl.PeekTicket(ctx, tk, b); err != nil || !p.Active {
		t.Fatalf("peek: %+v %v", p, err)
	}
	if s, err := cl.RedeemTicket(ctx, tk, b); err != nil || !s.Active || s.AuthenticatedAt.IsZero() {
		t.Fatalf("redeem: %+v %v", s, err)
	}
	// Bounded: a pile of planted cookies is not tried without end.
	r = httptest.NewRequest("GET", "/", nil)
	for _, c := range "abcdefghij" {
		r.AddCookie(&http.Cookie{Name: name, Value: modclient.BindingPrefix + strings.Repeat(string(c), 43)})
	}
	if n := len(strings.Fields(modclient.LaunchBinding(r, "work", true))); n != 4 {
		t.Fatalf("%d candidates", n)
	}
}

func TestJunkIsNeverSentToCore(t *testing.T) {
	core := fakecore.New(t, "work")
	cl := core.Client(t)
	for _, bad := range []string{"", "x", "canpat_" + strings.Repeat("a", 43), strings.Repeat("a", 43), "cant_" + strings.Repeat("!", 43)} {
		if s, err := cl.RedeemTicket(context.Background(), bad, "junk"); err != nil || s.Active {
			t.Fatalf("%q: %+v %v", bad, s, err)
		}
	}
	if _, err := cl.RevalidateSession(context.Background(), "junk"); err != nil {
		t.Fatal(err)
	}
	if core.Redeems != 0 || core.Revalidates != 0 {
		t.Fatalf("junk reached can-core: %d %d", core.Redeems, core.Revalidates)
	}
}

func TestCoreDownIsAnErrorNotAnActiveAnswer(t *testing.T) {
	core := fakecore.New(t, "work")
	cl := core.Client(t)
	tk := core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{"work.read"}})
	core.SetDown(true)
	if s, err := cl.RedeemTicket(context.Background(), tk, core.Binding(tk)); !errors.Is(err, modclient.ErrUnavailable) || s.Active {
		t.Fatalf("%+v %v", s, err)
	}
	core.SetDown(false)
	if s, err := cl.RedeemTicket(context.Background(), tk, core.Binding(tk)); err != nil || !s.Active {
		t.Fatalf("the ticket must survive an outage of can-core: %+v %v", s, err)
	}
}

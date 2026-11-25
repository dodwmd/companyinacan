package files

import (
	"errors"
	"testing"
)

// QA major 6: a person removed from a space (or demoted) kept the links they had made.
func TestRemovingAMemberRevokesTheirLinks(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "S")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf(""))
	link := func(p *Principal) string {
		t.Helper()
		tok, _, err := e.s.CreateShare(ctx, p, f.ID, ShareRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	works := func(tok string) bool {
		c, err := e.s.OpenShare(ctx, tok, "1.1.1.1")
		if err == nil {
			_ = c.Body.Close()
			return true
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		return false
	}
	e.member(e.alice, sp.ID, "user", "user:bob", "editor")
	e.member(e.alice, sp.ID, "user", "user:carol", "editor")
	mine, bobs, carols := link(e.alice), link(e.bob), link(e.carol)

	if err := e.s.RemoveMember(ctx, e.alice, sp.ID, "user", "user:bob"); err != nil {
		t.Fatal(err)
	}
	if works(bobs) {
		t.Error("a removed member's link still works")
	}
	if !works(mine) || !works(carols) {
		t.Error("other people's links were revoked")
	}
	e.member(e.alice, sp.ID, "user", "user:carol", "viewer")
	if works(carols) {
		t.Error("a demoted member's link still works")
	}

	// through a group: removing the group's grant ends the links made on the strength of it
	e.member(e.alice, sp.ID, "group", "grp-eng", "editor")
	dave := &Principal{Subject: "user:dave", Perms: []string{PermWrite}, Groups: []string{"grp-eng"}}
	erin := &Principal{Subject: "user:erin", Perms: []string{PermWrite}, Groups: []string{"grp-eng"}}
	e.member(e.alice, sp.ID, "user", "user:erin", "editor")
	daves, erins := link(dave), link(erin)
	e.member(e.alice, sp.ID, "group", "grp-eng", "viewer")
	if works(daves) {
		t.Error("a link made through a demoted group still works")
	}
	if !works(erins) {
		t.Error("a link of someone who is still an editor in their own right was revoked")
	}
	if err := e.s.RemoveMember(ctx, e.alice, sp.ID, "user", "user:erin"); err != nil {
		t.Fatal(err)
	}
	if works(erins) {
		t.Error("the last grant went and the link still works")
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'share.revoke')`); n != 4 {
		t.Errorf("revocations audited: %d, want 4", n)
	}
}

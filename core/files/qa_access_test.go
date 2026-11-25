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

// QA major 8: the admin purge route ignored holds and retention dates, so any admin token could
// destroy held files.
func TestPurgeHookNeedsTheRestoreCredentialToIgnoreHolds(t *testing.T) {
	e := newEnv(t)
	sp := e.space(e.alice, "S")
	f := e.upload(e.alice, sp.ID, "a.pdf", pdf(""))
	g := e.upload(e.alice, sp.ID, "b.pdf", pdf(""))
	if err := e.s.SetHold(ctx, e.root, f.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	until := e.now().AddDate(1, 0, 0)
	if err := e.s.SetHold(ctx, e.root, g.ID, false, &until); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.ID, g.ID} {
		if err := e.s.PurgeSubject(ctx, e.root, id, ClassContent); !errors.Is(err, ErrConflict) {
			t.Errorf("admin purge of a held or retained file = %v, want conflict", err)
		}
	}
	if len(e.blobs()) != 2 {
		t.Fatal("a held file was destroyed")
	}
	restore := &Principal{Subject: "svc:restore", Restore: true}
	for _, id := range []string{f.ID, g.ID} {
		if err := e.s.PurgeSubject(ctx, restore, id, ClassContent); err != nil {
			t.Errorf("restore replay: %v", err)
		}
	}
	if len(e.blobs()) != 0 || e.count(`SELECT COUNT(*) FROM audit_log WHERE (action = 'file.restore_purge') AND (actor = 'svc:restore') AND (outcome = 'ok')`) != 2 {
		t.Error("the restore replay did not purge, or was not audited as its own action")
	}
	// the restore credential is for the purge hook only
	if _, err := e.s.EraseSubject(ctx, restore, "pers_x"); !errors.Is(err, ErrForbidden) {
		t.Errorf("restore principal erasing = %v", err)
	}
	if _, err := e.s.ListAudit(ctx, restore, 0, 1); !errors.Is(err, ErrForbidden) {
		t.Errorf("restore principal reading the audit log = %v", err)
	}
}

func TestRestoreTokenOpensOnlyThePurgeRoute(t *testing.T) {
	a := newAPIEnv(t)
	const restoreTok = "restore-token-abcdefghij0123456789klmnop"
	a.api.Auth = MultiAuth{a.api.Auth, StaticRestoreToken{Token: restoreTok}}
	if resp, _ := a.json("POST", "/v1/purge", restoreTok, map[string]string{"subject": "pers_x", "class": ClassContent}); resp.StatusCode != 204 {
		t.Errorf("purge with the restore token: %d", resp.StatusCode)
	}
	for _, path := range []string{"/v1/spaces", "/v1/audit", "/v1/erasures", "/v1/admin/files", "/v1/files"} {
		if resp, _ := a.do("GET", path, restoreTok, nil); resp.StatusCode != 403 {
			t.Errorf("%s with the restore token: %d, want 403", path, resp.StatusCode)
		}
	}
	if resp, _ := a.json("POST", "/v1/purge", patAlice, map[string]string{"subject": "pers_x", "class": ClassContent}); resp.StatusCode != 403 {
		t.Errorf("purge as a writer: %d", resp.StatusCode)
	}
}

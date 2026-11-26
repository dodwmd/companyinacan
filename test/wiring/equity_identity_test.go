package wiring

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/equity"
	"github.com/dodwmd/companyinacan/core/people"
)

// equity's directory client against the real people API: the lookup equity relies on to know
// who a caller is (specs/051-holder-identity).
func TestEquityResolvesCallersThroughThePeopleDirectory(t *testing.T) {
	ctx := context.Background()
	kr, err := people.ParseKeyring("k1 " + strings.Repeat("ab", 32) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := people.Open(filepath.Join(t.TempDir(), "p.db"), people.Options{Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	hr := people.Actor{Subject: "svc-can-core", Roles: []string{people.RoleHR, people.RoleAdmin}}
	ann, err := svc.CreatePerson(ctx, hr, people.PersonInput{Kind: "employee", Status: "active", GivenName: "Ann", FamilyName: "A", SSOSubject: "user:ann"})
	if err != nil {
		t.Fatal(err)
	}
	// an hr person cannot say which account they are, nor stop being it
	self := people.Actor{Subject: "user:hr", Roles: []string{people.RoleHR}}
	if _, err := svc.CreatePerson(ctx, self, people.PersonInput{Kind: "employee", Status: "active", GivenName: "H", FamilyName: "R", SSOSubject: "user:HR"}); err == nil {
		t.Fatal("self-link on create")
	}
	annHR := people.Actor{Subject: "user:ann", Roles: []string{people.RoleHR}, PersonID: ann.ID, HasPerson: true, Status: "active"}
	other := "user:other"
	if _, err := svc.UpdatePerson(ctx, annHR, ann.ID, people.PersonPatch{SSOSubject: &other}); err == nil {
		t.Fatal("relinking one's own record")
	}

	tokens := &people.StaticTokens{}
	const hrToken, staffToken = "directory-token-hr-000000000000", "directory-token-staff-000000000"
	tokens.Add(hrToken, people.Principal{Subject: "svc-equity", Roles: []string{people.RoleHR}})
	tokens.Add(staffToken, people.Principal{Subject: "user:ann", Roles: []string{people.RoleEmployee}})
	api := &people.API{Svc: svc, Auth: tokens, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	d := &equity.HTTPDirectory{BaseURL: srv.URL, Token: hrToken}
	if got, err := d.PersonForSubject(ctx, "user:ann"); err != nil || got != ann.ID {
		t.Fatalf("ann = %q, %v", got, err)
	}
	if got, err := d.PersonForSubject(ctx, "user:nobody"); err != nil || got != "" {
		t.Fatalf("nobody = %q, %v", got, err)
	}
	if ok, err := d.PersonExists(ctx, ann.ID); err != nil || !ok {
		t.Fatalf("exists = %v, %v", ok, err)
	}
	if ok, err := d.PersonExists(ctx, "pers_0000000000000000000000ff"); err != nil || ok {
		t.Fatalf("made-up id exists = %v, %v", ok, err)
	}
	// the credential can-core copies to equity: the read-only identity lookup of the real service
	lookupToken := strings.Repeat("l00kup", 6)
	people.AddLookupToken(tokens, lookupToken)
	look := &equity.HTTPDirectory{BaseURL: srv.URL, Token: lookupToken}
	if got, err := look.PersonForSubject(ctx, "user:ann"); err != nil || got != ann.ID {
		t.Fatalf("lookup: ann = %q, %v", got, err)
	}
	if got, err := look.PersonForSubject(ctx, "user:nobody"); err != nil || got != "" {
		t.Fatalf("lookup: nobody = %q, %v", got, err)
	}
	if ok, err := look.PersonExists(ctx, ann.ID); err != nil || !ok {
		t.Fatalf("lookup: exists = %v, %v", ok, err)
	}
	if ok, err := look.PersonExists(ctx, "pers_0000000000000000000000ff"); err != nil || ok {
		t.Fatalf("lookup: made-up id exists = %v, %v", ok, err)
	}
	// a token that may not see account links, or no token, is "cannot know", never "nobody"
	for _, tok := range []string{staffToken, "wrong-token-000000000000000000"} {
		d := &equity.HTTPDirectory{BaseURL: srv.URL, Token: tok}
		if got, err := d.PersonForSubject(ctx, "user:ann"); err == nil {
			t.Fatalf("token %q answered %q", tok, got)
		}
	}
}

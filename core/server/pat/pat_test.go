package pat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type env struct {
	s   *Service
	db  *store.DB
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{db: db, now: time.Unix(1_700_000_000, 0)}
	db.Now = func() time.Time { return e.now }
	e.s = New(db, audit.New(db))
	e.s.Now = func() time.Time { return e.now }
	for _, id := range []string{"work", "people"} {
		m := config.Module{ID: id, Version: "1.0.0", Permissions: []string{id + ".read", id + ".write"}}
		if err := db.UpsertModule(ctx, store.Module{ID: id, Version: "1.0.0", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, Manifest: m}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func issue(t *testing.T, e *env, mod string, perms ...string) Issued {
	t.Helper()
	iss, err := e.s.Issue(context.Background(), "user:alice", IssueRequest{Name: "ci", Module: mod, Permissions: perms, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return iss
}

func TestIssueThenIntrospectIsScopedToTheModule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iss := issue(t, e, "work", "work.read")
	if !strings.HasPrefix(iss.Token, TokenPrefix) || len(iss.Token) < 40 {
		t.Fatalf("token shape %q", iss.Token)
	}
	r, ok := e.s.Introspect(ctx, "work", iss.Token)
	if !ok || r.Subject != "user:alice" || len(r.Permissions) != 1 || r.Permissions[0] != "work.read" || r.TokenID != iss.ID {
		t.Fatalf("%+v %v", r, ok)
	}
	// Another module never learns that the token exists, let alone who holds it.
	if r, ok := e.s.Introspect(ctx, "people", iss.Token); ok || r.Subject != "" || len(r.Permissions) != 0 {
		t.Fatalf("a token bound to work answered for people: %+v", r)
	}
}

func TestTokenIsHashedAtRest(t *testing.T) {
	e := newEnv(t)
	iss := issue(t, e, "work", "work.read")
	var stored string
	if err := e.db.QueryRow(`SELECT token_hash FROM api_tokens WHERE id=?`, iss.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || strings.Contains(stored, iss.Token) || stored == iss.Token {
		t.Fatalf("token stored in the clear: %q", stored)
	}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM api_tokens WHERE name LIKE ? OR subject LIKE ? OR permissions LIKE ?`, "%"+iss.Token+"%", "%"+iss.Token+"%", "%"+iss.Token+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("token value found in a column: %d %v", n, err)
	}
}

func TestExpiryRevocationAndDisabledModule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iss := issue(t, e, "work", "work.read")
	e.now = e.now.Add(2 * time.Hour)
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); ok {
		t.Fatal("an expired token was accepted")
	}
	e.now = time.Unix(1_700_000_000, 0)
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); !ok {
		t.Fatal("token should be valid again after the clock reset")
	}
	if err := e.s.Revoke(ctx, "user:alice", iss.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); ok {
		t.Fatal("a revoked token was accepted")
	}
}

func TestIssueRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cases := map[string]IssueRequest{
		"unknown module":        {Name: "x", Module: "nope", Permissions: []string{"work.read"}, TTL: time.Hour},
		"undeclared permission": {Name: "x", Module: "work", Permissions: []string{"work.root"}, TTL: time.Hour},
		"other module's perm":   {Name: "x", Module: "work", Permissions: []string{"people.read"}, TTL: time.Hour},
		"no permissions":        {Name: "x", Module: "work", TTL: time.Hour},
		"no name":               {Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour},
		"too long lived":        {Name: "x", Module: "work", Permissions: []string{"work.read"}, TTL: 400 * 24 * time.Hour},
		"no expiry":             {Name: "x", Module: "work", Permissions: []string{"work.read"}},
		"control chars in name": {Name: "a\nb", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour},
	}
	for name, req := range cases {
		if _, err := e.s.Issue(ctx, "user:alice", req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := e.s.Issue(ctx, "party:7", IssueRequest{Name: "x", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour}); err == nil {
		t.Error("only a signed-in administrator (user:...) may issue tokens")
	}
}

func TestIssuerMustHoldWhatItGrants(t *testing.T) {
	e := newEnv(t)
	e.s.Holds = func(_ context.Context, subject, perm string) (bool, error) { return perm == "work.read", nil }
	ctx := context.Background()
	if _, err := e.s.Issue(ctx, "user:alice", IssueRequest{Name: "x", Module: "work", Permissions: []string{"work.write"}, TTL: time.Hour}); err == nil {
		t.Fatal("granted a permission the issuer does not hold")
	}
	if _, err := e.s.Issue(ctx, "user:alice", IssueRequest{Name: "x", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionsNoLongerInTheManifestAreDropped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iss := issue(t, e, "work", "work.read", "work.write")
	m := config.Module{ID: "work", Version: "2.0.0", Permissions: []string{"work.read"}}
	if err := e.db.UpsertModule(ctx, store.Module{ID: "work", Version: "2.0.0", Namespace: "mod-work", ServiceAccount: "work", State: store.StateEnabled, Manifest: m}, nil, nil); err != nil {
		t.Fatal(err)
	}
	r, ok := e.s.Introspect(ctx, "work", iss.Token)
	if !ok || len(r.Permissions) != 1 || r.Permissions[0] != "work.read" {
		t.Fatalf("%+v %v", r, ok)
	}
}

func TestIntrospectGarbageIsInactiveAndBounded(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{"", "x", TokenPrefix, TokenPrefix + "short", strings.Repeat("a", 5000), TokenPrefix + strings.Repeat("A", 5000)} {
		if _, ok := e.s.Introspect(context.Background(), "work", tok); ok {
			t.Errorf("%.20q accepted", tok)
		}
	}
}

func TestLiveTokenCapPerModule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < MaxLivePerModule; i++ {
		if _, err := e.s.Issue(ctx, "user:alice", IssueRequest{Name: "t", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := e.s.Issue(ctx, "user:alice", IssueRequest{Name: "t", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour}); err == nil {
		t.Fatal("no cap on live tokens")
	}
}

func TestAuditEntriesNeverCarryTheToken(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iss := issue(t, e, "work", "work.read")
	_ = e.s.Revoke(ctx, "user:alice", iss.ID, false)
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action LIKE 'token.%'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit rows %d %v", n, err)
	}
	if err := e.db.QueryRow(`SELECT count(*) FROM audit_log WHERE detail LIKE ? OR target LIKE ?`, "%"+iss.Token+"%", "%"+iss.Token+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the token reached the audit log: %d %v", n, err)
	}
}

package ticket

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/identity"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type env struct {
	s    *Service
	db   *store.DB
	now  time.Time
	mu   sync.Mutex
	held map[string]bool   // "subject|permission"
	bind map[string]string // ticket -> the nonce cookie value of the launching browser

	sessions int
}

func (e *env) hold(subject string, perms ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range perms {
		e.held[subject+"|"+p] = true
	}
}

func (e *env) drop(subject, perm string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.held, subject+"|"+perm)
}

func (e *env) disable(t *testing.T, username string) {
	t.Helper()
	u, err := e.db.AdminByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(context.Background(), `UPDATE admin_users SET disabled_at=1 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
}

func addModule(t *testing.T, db *store.DB, id string, ui bool, state string) {
	t.Helper()
	m := config.Module{ID: id, Name: id, Version: "1.0.0", Permissions: []string{id + ".read", id + ".write"}}
	m.Runtime.Subdomain = id
	if ui {
		m.Runtime.WebLogin = &config.WebLogin{ViaSubdomain: true}
	}
	if err := db.UpsertModule(context.Background(), store.Module{ID: id, Version: "1.0.0", Namespace: "mod-" + id, ServiceAccount: id, State: state, Manifest: m}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{db: db, now: time.Unix(1_700_000_000, 0), held: map[string]bool{}, bind: map[string]string{}}
	db.Now = func() time.Time { return e.now }
	for _, u := range []string{"alice", "bob"} {
		if _, err := db.CreateAdmin(ctx, u, "x", "y"); err != nil {
			t.Fatal(err)
		}
	}
	addModule(t, db, "work", true, store.StateEnabled)
	addModule(t, db, "people", true, store.StateEnabled)
	addModule(t, db, "wiki", false, store.StateEnabled)
	addModule(t, db, "jobs", true, store.StateDisabled)
	e.s = &Service{
		DB: db, Audit: audit.New(db), Now: func() time.Time { return e.now },
		Launcher: Launcher{Domain: "example.org"},
		Holds: func(_ context.Context, subject, perm string) (bool, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.held[subject+"|"+perm], nil
		},
		Sleep: func(context.Context, time.Duration) {},
	}
	e.hold("user:alice", "work.read", "work.write", "people.read", "wiki.read", "jobs.read")
	return e
}

// admin returns a live admin session of the account behind subject, as the admin UI would hand it
// to Launch (a session that was never stored for an unknown account).
func (e *env) admin(t *testing.T, subject string, change ...func(*store.Session)) store.Session {
	t.Helper()
	ctx := context.Background()
	u, err := e.db.AdminByUsername(ctx, strings.TrimPrefix(subject, "user:"))
	if err != nil {
		return store.Session{IDHash: "none"}
	}
	e.mu.Lock()
	e.sessions++
	h := fmt.Sprintf("%064d", e.sessions)
	e.mu.Unlock()
	s := store.Session{IDHash: h, UserID: u.ID, CSRF: "c", CreatedAt: e.now, ExpiresAt: e.now.Add(12 * time.Hour), Source: store.SourceLocal}
	for _, c := range change {
		c(&s)
	}
	if err := e.db.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func launch(t *testing.T, e *env, subject, module, ret string) (string, string) {
	t.Helper()
	res, err := e.s.Launch(context.Background(), subject, module, ret, e.admin(t, subject))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	u, err := url.Parse(res.URL)
	if err != nil || u.Query().Get("t") == "" {
		t.Fatalf("launch URL %q", res.URL)
	}
	e.bind[u.Query().Get("t")] = res.Binding
	return u.Query().Get("t"), res.URL
}

// redeem is what the module does for the browser that launched: the ticket plus that browser's
// nonce cookie.
func (e *env) redeem(module, ticket string) (Redeemed, bool) {
	return e.s.Redeem(context.Background(), module, ticket, e.bind[ticket])
}

// Login CSRF: a ticket is worth nothing without the nonce cookie of the browser that launched it.
func TestRedeemNeedsTheLaunchingBrowsersNonce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk, _ := launch(t, e, "user:alice", "work", "")
	mine := e.bind[tk]
	if !strings.HasPrefix(mine, BindingPrefix) || len(mine) < 40 || strings.Contains(tk, mine) {
		t.Fatalf("binding %q", mine)
	}
	other, _ := launch(t, e, "user:alice", "work", "")
	for name, b := range map[string]string{"missing": "", "junk": "x", "another launch's": e.bind[other], "forged": BindingPrefix + strings.Repeat("A", 43)} {
		if r, ok := e.s.Redeem(ctx, "work", tk, b); ok || r.Subject != "" {
			t.Fatalf("%s binding redeemed: %+v", name, r)
		}
		if p, ok := e.s.Peek(ctx, "work", tk, b); ok || p.Subject != "" {
			t.Fatalf("%s binding peeked: %+v", name, p)
		}
	}
	// None of that spent the ticket for the browser that launched it.
	if _, ok := e.s.Redeem(ctx, "work", tk, mine); !ok {
		t.Fatal("the launching browser can no longer redeem")
	}
}

func TestPeekNamesTheAccountWithoutConsuming(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk, _ := launch(t, e, "user:alice", "work", "")
	for i := 0; i < 3; i++ {
		p, ok := e.s.Peek(ctx, "work", tk, e.bind[tk])
		if !ok || p.Subject != "user:alice" || p.Name == "" {
			t.Fatalf("%+v %v", p, ok)
		}
	}
	if _, ok := e.s.Peek(ctx, "people", tk, e.bind[tk]); ok {
		t.Fatal("another module peeked")
	}
	if _, ok := e.redeem("work", tk); !ok {
		t.Fatal("peek consumed the ticket")
	}
	if _, ok := e.s.Peek(ctx, "work", tk, e.bind[tk]); ok {
		t.Fatal("a spent ticket peeked")
	}
}

func TestBindingCookie(t *testing.T) {
	e := newEnv(t)
	res, err := e.s.Launch(context.Background(), "user:alice", "work", "", e.admin(t, "user:alice"))
	if err != nil {
		t.Fatal(err)
	}
	ck, err := e.s.BindingCookie(res, "admin.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if ck.Name != "__Secure-can_launch_work" || ck.Value != res.Binding || ck.Domain != "example.org" || ck.Path != "/login/ticket" ||
		!ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.MaxAge <= 0 || ck.MaxAge > 60 {
		t.Fatalf("%+v", ck)
	}
	// Admin UI reached on a name that shares nothing with the module: the cookie could never arrive.
	for _, host := range []string{"localhost:8080", "evil.example", "10.0.0.1", ""} {
		if _, err := e.s.BindingCookie(res, host); err == nil {
			t.Errorf("admin host %q: no error", host)
		}
	}
	// Plain-http development on one host (ports do not separate cookies): host-only, not Secure.
	dev := &Service{Launcher: Launcher{Domain: "acme.example", AllowHTTP: true}}
	ck, err = dev.BindingCookie(LaunchResult{Module: "work", Binding: res.Binding, Base: "http://localhost:8081/ui"}, "localhost:8080")
	if err != nil || ck.Name != "can_launch_work" || ck.Domain != "" || ck.Secure || ck.Path != "/ui/login/ticket" {
		t.Fatalf("%+v %v", ck, err)
	}
	// A single-label domain is not a valid cookie Domain.
	dev.Launcher.Domain = "localhost"
	if _, err := dev.BindingCookie(LaunchResult{Module: "work", Binding: res.Binding, Base: "http://work.localhost:8080"}, "admin.localhost:8080"); err == nil {
		t.Fatal("Domain=localhost would be dropped by the browser")
	}
}

// A module session never outlives the identity provider's revalidation deadline of the admin
// session that launched it.
func TestSessionIsCappedAtTheLaunchersDeadline(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	deadline := e.now.Add(10 * time.Minute)
	until := func(at time.Time) func(*store.Session) {
		return func(s *store.Session) { s.Source, s.RevalidateAt = store.SourceSSO, at }
	}
	res, err := e.s.Launch(ctx, "user:alice", "work", "", e.admin(t, "user:alice", until(deadline)))
	if err != nil {
		t.Fatal(err)
	}
	tk := res.URL[strings.Index(res.URL, "t=")+2:]
	r, ok := e.s.Redeem(ctx, "work", tk, res.Binding)
	if !ok || !r.SessionExpires.Equal(deadline) {
		t.Fatalf("%+v %v", r, ok)
	}
	e.now = deadline.Add(time.Second)
	if _, ok := e.s.Revalidate(ctx, "work", r.Handle); ok {
		t.Fatal("active past the deadline")
	}
	if _, err := e.s.Launch(ctx, "user:alice", "work", "", e.admin(t, "user:alice", until(e.now.Add(-time.Second)))); err == nil {
		t.Fatal("launched from an expired sign-in")
	}
}

// Credential changes, session revocation and unlinking end module sessions and unused tickets.
func TestAccountChangesEndLaunchSessionsAndUnusedTickets(t *testing.T) {
	for name, change := range map[string]func(e *env, u store.AdminUser) error{
		"password or TOTP change or reset": func(e *env, u store.AdminUser) error {
			return e.db.SetAdminCredentials(context.Background(), u.ID, "new-hash", "")
		},
		"revoking one admin session": func(e *env, u store.AdminUser) error {
			ctx := context.Background()
			h := strings.Repeat("a", 64)
			if err := e.db.CreateSession(ctx, store.Session{IDHash: h, UserID: u.ID, CSRF: "c", CreatedAt: e.now, ExpiresAt: e.now.Add(time.Hour), Source: "local"}); err != nil {
				return err
			}
			found, err := e.db.RevokeSession(ctx, u.ID, h[:store.PublicIDLen])
			if !found && err == nil {
				t.Fatal("no session revoked")
			}
			return err
		},
		"SSO unlink of a local account": func(e *env, u store.AdminUser) error {
			ctx := context.Background()
			if _, err := e.db.ExecContext(ctx, `INSERT INTO sso_identities(issuer_hash,subject,user_id,email,name,groups_json,amr_json,created_at,last_login,linked_by) VALUES ('h','s',?,'','','[]','[]',0,0,'root')`, u.ID); err != nil {
				return err
			}
			id, err := e.db.SSOIdentityByUser(ctx, u.ID)
			if err != nil {
				return err
			}
			return e.db.UnlinkSSOIdentity(ctx, id.ID, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			used, _ := launch(t, e, "user:alice", "work", "")
			r, ok := e.redeem("work", used)
			if !ok {
				t.Fatal("redeem")
			}
			unused, _ := launch(t, e, "user:alice", "work", "")
			u, err := e.db.AdminByUsername(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			if err := change(e, u); err != nil {
				t.Fatal(err)
			}
			if _, ok := e.s.Revalidate(ctx, "work", r.Handle); ok {
				t.Error("the module session survived")
			}
			if _, ok := e.redeem("work", unused); ok {
				t.Error("an unused ticket survived")
			}
		})
	}
}

func TestLaunchThenRedeem(t *testing.T) {
	e := newEnv(t)
	tk, raw := launch(t, e, "user:alice", "work", "/projects/ABC")
	if raw != "https://work.example.org/login/ticket?t="+tk {
		t.Fatalf("URL %q", raw)
	}
	if !LooksLikeTicket(tk) {
		t.Fatalf("ticket shape %q", tk)
	}
	r, ok := e.redeem("work", tk)
	if !ok || r.Subject != "user:alice" || r.Name == "" || r.ReturnPath != "/projects/ABC" || !LooksLikeHandle(r.Handle) {
		t.Fatalf("%+v %v", r, ok)
	}
	if strings.Join(r.Permissions, ",") != "work.read,work.write" {
		t.Fatalf("permissions %v: must be exactly the module's permissions the user holds", r.Permissions)
	}
	if !r.SessionExpires.After(e.now) {
		t.Fatal("session handle has no expiry")
	}
}

func TestReplayFails(t *testing.T) {
	e := newEnv(t)
	tk, _ := launch(t, e, "user:alice", "work", "")
	if _, ok := e.redeem("work", tk); !ok {
		t.Fatal("first redeem failed")
	}
	if r, ok := e.redeem("work", tk); ok || r.Subject != "" || r.Handle != "" {
		t.Fatalf("second redeem answered: %+v", r)
	}
}

func TestConcurrentRedeemHasOneWinner(t *testing.T) {
	e := newEnv(t)
	tk, _ := launch(t, e, "user:alice", "work", "")
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := e.redeem("work", tk); ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d redeems succeeded for one ticket", wins)
	}
}

func TestTicketIsBoundToItsModule(t *testing.T) {
	e := newEnv(t)
	tk, _ := launch(t, e, "user:alice", "work", "")
	if r, ok := e.redeem("people", tk); ok || r.Subject != "" {
		t.Fatalf("a ticket for work was redeemed by people: %+v", r)
	}
	// The failed attempt by another module did not burn the ticket for its audience.
	if _, ok := e.redeem("work", tk); !ok {
		t.Fatal("the audience can no longer redeem after a foreign attempt")
	}
}

func TestExpiryAndSkew(t *testing.T) {
	e := newEnv(t)
	tk, _ := launch(t, e, "user:alice", "work", "")
	e.now = e.now.Add(MaxTicketTTL + time.Second)
	if _, ok := e.redeem("work", tk); ok {
		t.Fatal("an expired ticket was accepted")
	}
	// Expiry uses can-core's clock only: a module's clock is never consulted.
	e.now = time.Unix(1_700_000_000, 0)
	tk2, _ := launch(t, e, "user:alice", "work", "")
	e.now = e.now.Add(MaxTicketTTL - time.Second)
	if _, ok := e.redeem("work", tk2); !ok {
		t.Fatal("a ticket inside its lifetime was refused")
	}
	if MaxTicketTTL > time.Minute {
		t.Fatalf("ticket lifetime %s exceeds 60 seconds", MaxTicketTTL)
	}
}

func TestOnlyShapedTicketsAreLookedUp(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"", "x", "cant_", strings.Repeat("a", 500), "cant_" + strings.Repeat("!", 43), " "} {
		if _, ok := e.redeem("work", bad); ok {
			t.Fatalf("%q redeemed", bad)
		}
	}
}

func TestReturnPathAllowlist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, bad := range []string{"//evil.example", "/\\evil", "https://evil.example/", "javascript:alert(1)", "/a/../b", "/a?x=1", "/a#f", "/a b", "/%2e%2e/", "/" + strings.Repeat("a", 200), "x", "/ü", "/a\r\nSet-Cookie: x=1", "/ui//x", "/a/./b"} {
		if _, err := e.s.Launch(ctx, "user:alice", "work", bad, e.admin(t, "user:alice")); err == nil {
			t.Errorf("return path %q accepted", bad)
		}
	}
	for _, good := range []string{"", "/", "/projects/ABC-1", "/people/pers_0123", "/a_b-c/d"} {
		if _, err := e.s.Launch(ctx, "user:alice", "work", good, e.admin(t, "user:alice")); err != nil {
			t.Errorf("return path %q refused: %v", good, err)
		}
	}
}

func TestMintRequiresAllowedActiveUserAndModule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, c := range map[string]struct{ subject, module string }{
		"no permission in the module": {"user:bob", "work"},
		"unknown user":                {"user:nobody", "work"},
		"module without a UI":         {"user:alice", "wiki"},
		"disabled module":             {"user:alice", "jobs"},
		"unknown module":              {"user:alice", "ghost"},
		"not a user subject":          {"party:p1", "work"},
		"bad module id":               {"user:alice", "../work"},
	} {
		if _, err := e.s.Launch(ctx, c.subject, c.module, "", e.admin(t, c.subject)); err == nil {
			t.Errorf("%s: a ticket was minted", name)
		}
	}
	e.disable(t, "alice")
	if _, err := e.s.Launch(ctx, "user:alice", "work", "", e.admin(t, "user:alice")); err == nil {
		t.Fatal("a disabled user minted a ticket")
	}
}

func TestDisabledAfterMintCannotRedeem(t *testing.T) {
	e := newEnv(t)
	tk, _ := launch(t, e, "user:alice", "work", "")
	e.disable(t, "alice")
	if _, ok := e.redeem("work", tk); ok {
		t.Fatal("a disabled user's ticket redeemed")
	}
}

func TestRolesRemovedAfterMintLeaveNothingToRedeem(t *testing.T) {
	e := newEnv(t)
	tk, _ := launch(t, e, "user:alice", "work", "")
	e.drop("user:alice", "work.read")
	e.drop("user:alice", "work.write")
	if _, ok := e.redeem("work", tk); ok {
		t.Fatal("redeemed with no permission left")
	}
}

func TestMintRateLimitIsPerUser(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.hold("user:bob", "work.read")
	for i := 0; i < MintPerMinute; i++ {
		if _, err := e.s.Launch(ctx, "user:alice", "work", "", e.admin(t, "user:alice")); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if _, err := e.s.Launch(ctx, "user:alice", "work", "", e.admin(t, "user:alice")); err != ErrRateLimited {
		t.Fatalf("31st mint: %v", err)
	}
	if _, err := e.s.Launch(ctx, "user:bob", "work", "", e.admin(t, "user:bob")); err != nil {
		t.Fatalf("another user is throttled: %v", err)
	}
	e.now = e.now.Add(61 * time.Second)
	if _, err := e.s.Launch(ctx, "user:alice", "work", "", e.admin(t, "user:alice")); err != nil {
		t.Fatalf("still throttled a minute later: %v", err)
	}
}

func TestTicketsAndHandlesAreHashedAtRest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk, _ := launch(t, e, "user:alice", "work", "")
	r, _ := e.redeem("work", tk)
	for _, v := range []string{tk, r.Handle} {
		var n int
		if err := e.db.QueryRow(`SELECT (SELECT count(*) FROM login_tickets WHERE id=?1 OR token_hash=?1 OR subject=?1 OR return_path=?1) + (SELECT count(*) FROM ui_launch_sessions WHERE handle_hash=?1 OR ticket_id=?1 OR subject=?1)`, v).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("a secret value is stored in the clear")
		}
	}
	recs, err := audit.New(e.db).Query(ctx, audit.Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var minted, redeemed bool
	for _, rec := range recs {
		for _, secret := range []string{tk, r.Handle} {
			if strings.Contains(rec.Detail, secret) || strings.Contains(rec.Target, secret) || strings.Contains(rec.Actor, secret) {
				t.Fatalf("the audit log holds a secret: %+v", rec)
			}
		}
		minted = minted || rec.Action == "ui.ticket.mint"
		redeemed = redeemed || rec.Action == "ui.ticket.redeem"
	}
	if !minted || !redeemed {
		t.Fatalf("mint and redeem must be audited (mint=%v redeem=%v)", minted, redeemed)
	}
}

func TestFailedRedeemsAreSlowedButValidOnesNever(t *testing.T) {
	e := newEnv(t)
	var slept int32
	e.s.Sleep = func(context.Context, time.Duration) { atomic.AddInt32(&slept, 1) }
	e.s.FailFree = 3
	good, _ := launch(t, e, "user:alice", "work", "")
	for i := 0; i < 10; i++ {
		e.redeem("work", "cant_"+strings.Repeat("A", 43))
	}
	if slept != 7 {
		t.Fatalf("expected the 7 failures past the free quota to wait, got %d", slept)
	}
	before := slept
	if _, ok := e.redeem("work", good); !ok {
		t.Fatal("a valid ticket was refused during a flood")
	}
	if slept != before {
		t.Fatal("a valid redeem was delayed")
	}
	// Another module's budget is separate.
	other, _ := launch(t, e, "user:alice", "people", "")
	if _, ok := e.redeem("people", other); !ok {
		t.Fatal("people was throttled by work's failures")
	}
}

func TestAllInactiveAnswersCarryNothing(t *testing.T) {
	e := newEnv(t)
	used, _ := launch(t, e, "user:alice", "work", "")
	e.redeem("work", used)
	foreign, _ := launch(t, e, "user:alice", "work", "")
	exp, _ := launch(t, e, "user:alice", "work", "")
	var answers []Redeemed
	for _, c := range []struct{ mod, tk string }{{"work", used}, {"people", foreign}, {"work", "cant_" + strings.Repeat("B", 43)}, {"work", "junk"}} {
		r, ok := e.redeem(c.mod, c.tk)
		if ok {
			t.Fatalf("%v redeemed", c)
		}
		answers = append(answers, r)
	}
	e.now = e.now.Add(2 * time.Minute)
	r, ok := e.redeem("work", exp)
	if ok {
		t.Fatal("expired redeemed")
	}
	answers = append(answers, r)
	for _, a := range answers {
		if a.Subject != "" || a.Handle != "" || a.Name != "" || a.ReturnPath != "" || len(a.Permissions) != 0 {
			t.Fatalf("a failure carried data: %+v", a)
		}
	}
}

func TestRevalidate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk, _ := launch(t, e, "user:alice", "work", "")
	r, _ := e.redeem("work", tk)

	v, ok := e.s.Revalidate(ctx, "work", r.Handle)
	if !ok || v.Subject != "user:alice" || strings.Join(v.Permissions, ",") != "work.read,work.write" {
		t.Fatalf("%+v %v", v, ok)
	}
	if _, ok := e.s.Revalidate(ctx, "people", r.Handle); ok {
		t.Fatal("another module revalidated the handle")
	}
	for _, bad := range []string{"", "junk", "cans_" + strings.Repeat("A", 43)} {
		if _, ok := e.s.Revalidate(ctx, "work", bad); ok {
			t.Fatalf("%q revalidated", bad)
		}
	}
	// A permission lost is a permission gone at the next check; losing all ends the session.
	e.drop("user:alice", "work.write")
	v, ok = e.s.Revalidate(ctx, "work", r.Handle)
	if !ok || strings.Join(v.Permissions, ",") != "work.read" {
		t.Fatalf("permissions after a role change: %+v %v", v, ok)
	}
	e.drop("user:alice", "work.read")
	if _, ok := e.s.Revalidate(ctx, "work", r.Handle); ok {
		t.Fatal("still active with no permission")
	}
}

func TestRevalidateEndsWithTheAccountAndTheSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	mk := func() Redeemed {
		tk, _ := launch(t, e, "user:alice", "work", "")
		r, ok := e.redeem("work", tk)
		if !ok {
			t.Fatal("redeem")
		}
		return r
	}
	r := mk()
	n, err := e.db.RevokeLaunchSessions(ctx, "user:alice", e.now)
	if err != nil || n != 1 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if _, ok := e.s.Revalidate(ctx, "work", r.Handle); ok {
		t.Fatal("a revoked session revalidated")
	}
	r = mk()
	e.now = e.now.Add(SessionLifetime + time.Second)
	if _, ok := e.s.Revalidate(ctx, "work", r.Handle); ok {
		t.Fatal("a session past its absolute lifetime revalidated")
	}
	e.now = time.Unix(1_700_000_000, 0)
	r = mk()
	e.disable(t, "alice")
	if _, ok := e.s.Revalidate(ctx, "work", r.Handle); ok {
		t.Fatal("a disabled user's session revalidated")
	}
}

func TestAppsListsOnlyWhatTheUserMayOpen(t *testing.T) {
	e := newEnv(t)
	apps, err := e.s.Apps(context.Background(), "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range apps {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "people,work" {
		t.Fatalf("apps %v: modules without a UI, disabled ones and ones without a permission must not appear", ids)
	}
	if apps, _ := e.s.Apps(context.Background(), "user:bob"); len(apps) != 0 {
		t.Fatalf("bob sees %v", apps)
	}
}

// An upstream app that is only routed (runtime.ui) cannot take tickets: it is never offered, so a
// ticket is never sent to a program that does not expect one.
func TestOnlyModulesDeclaringWebLoginAreLaunchable(t *testing.T) {
	m := config.Module{ID: "wiki"}
	m.Runtime.Subdomain = "wiki"
	l := Launcher{Domain: "example.org", Overrides: map[string]string{"wiki": "https://wiki.example.org"}}
	if b, ok := l.Base(store.Module{ID: "wiki", Manifest: m}); ok {
		t.Fatalf("a module without web_login is launchable at %s", b)
	}
	// Declared but not served on the subdomain: only the operator's address counts, and a bare
	// origin gets the manifest's base path.
	m.Runtime.WebLogin = &config.WebLogin{BasePath: "/ui"}
	if _, ok := (Launcher{Domain: "example.org"}).Base(store.Module{ID: "wiki", Manifest: m}); ok {
		t.Fatal("launchable without an address")
	}
	if b, ok := l.Base(store.Module{ID: "wiki", Manifest: m}); !ok || b != "https://wiki.example.org/ui" {
		t.Fatalf("%q %v", b, ok)
	}
}

// Ending an account's access (identity.revoke) marks its module sessions revoked.
func TestIdentityRevocationEndsLaunchSessions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.hold("user:bob", "work.read") // alice is the first admin, so the owner
	tk, _ := launch(t, e, "user:bob", "work", "")
	r, ok := e.redeem("work", tk)
	if !ok {
		t.Fatal("redeem")
	}
	id := &identity.Service{DB: e.db, Audit: audit.New(e.db), Now: func() time.Time { return e.now }}
	if _, err := id.RevokeNow(ctx, "user:alice", "user:bob"); err != nil {
		t.Fatal(err)
	}
	var revoked int
	if err := e.db.QueryRow(`SELECT count(*) FROM ui_launch_sessions WHERE revoked_at<>0`).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("revoked sessions: %d %v", revoked, err)
	}
	if _, ok := e.s.Revalidate(ctx, "work", r.Handle); ok {
		t.Fatal("still active after the account's access was ended")
	}
}

func TestLauncherURLRules(t *testing.T) {
	mod := func(sub, base string) store.Module {
		m := config.Module{ID: "work"}
		m.Runtime.Subdomain = sub
		m.Runtime.WebLogin = &config.WebLogin{BasePath: base, ViaSubdomain: true}
		return store.Module{ID: "work", Manifest: m}
	}
	l := Launcher{Domain: "example.org"}
	if b, ok := l.Base(mod("work", "/ui")); !ok || b != "https://work.example.org/ui" {
		t.Fatalf("manifest URL %q %v", b, ok)
	}
	if _, ok := l.Base(mod("", "")); ok {
		t.Fatal("no subdomain and no override must not launch")
	}
	if _, ok := (Launcher{}).Base(mod("work", "")); ok {
		t.Fatal("no company domain: refused")
	}
	if _, ok := l.Base(mod("admin", "")); ok {
		t.Fatal("a module on the admin host would share its origin")
	}
	for _, bad := range []string{
		"http://work.example.org", "https://evil.example", "https://work.example.org.evil.example", "https://u:p@work.example.org",
		"https://work.example.org?x=1", "https://work.example.org/#f", "ftp://work.example.org", "https://work.example.org/a/../b", "//work.example.org", "https://admin.example.org", "https://work.example.org/UI", "https://work.example.org:99999",
	} {
		l := Launcher{Domain: "example.org", Overrides: map[string]string{"work": bad}}
		if b, ok := l.Base(mod("", "")); ok {
			t.Errorf("override %q accepted as %q", bad, b)
		}
	}
	good := Launcher{Domain: "example.org", Overrides: map[string]string{"work": "https://tracker.example.org/ui"}}
	if b, ok := good.Base(mod("", "")); !ok || b != "https://tracker.example.org/ui" {
		t.Fatalf("override %q %v", b, ok)
	}
	dev := Launcher{Domain: "example.org", AllowHTTP: true, Overrides: map[string]string{"work": "http://localhost:8080/ui"}}
	if b, ok := dev.Base(mod("", "")); !ok || b != "http://localhost:8080/ui" {
		t.Fatalf("dev override %q %v", b, ok)
	}
	if _, ok := (Launcher{Domain: "example.org", Overrides: map[string]string{"work": "http://localhost:8080/ui"}}).Base(mod("", "")); ok {
		t.Fatal("http accepted outside development")
	}
}

// accountChanges are the events that must end single sign-on into modules for alice. sso says the
// admin session in use came from the identity provider (unlinking ends only those).
var accountChanges = map[string]struct {
	sso    bool
	change func(e *env, u store.AdminUser, admin store.Session) error
}{
	"password or TOTP change": {change: func(e *env, u store.AdminUser, _ store.Session) error {
		return e.db.SetAdminCredentials(context.Background(), u.ID, "new-hash", "new-seed")
	}},
	"admin session revoked": {change: func(e *env, u store.AdminUser, admin store.Session) error {
		_, err := e.db.RevokeSession(context.Background(), u.ID, store.PublicSessionID(admin))
		return err
	}},
	"SSO unlink": {sso: true, change: func(e *env, u store.AdminUser, _ store.Session) error {
		ctx := context.Background()
		id, err := e.db.SSOIdentityByUser(ctx, u.ID)
		if err != nil {
			return err
		}
		return e.db.UnlinkSSOIdentity(ctx, id.ID, nil)
	}},
}

// raceEnv arranges for change to happen while the next Launch or Redeem is between its checks and
// its write: the permission lookup is made there, outside any transaction.
func raceEnv(t *testing.T, name string) (e *env, admin store.Session, arm func()) {
	t.Helper()
	c := accountChanges[name]
	e = newEnv(t)
	ctx := context.Background()
	u, err := e.db.AdminByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var mod []func(*store.Session)
	if c.sso {
		if _, err := e.db.ExecContext(ctx, `INSERT INTO sso_identities(issuer_hash,subject,user_id,email,name,groups_json,amr_json,created_at,last_login,linked_by) VALUES ('h','s',?,'','','[]','[]',0,0,'root')`, u.ID); err != nil {
			t.Fatal(err)
		}
		mod = append(mod, func(s *store.Session) { s.Source = store.SourceSSO })
	}
	admin = e.admin(t, "user:alice", mod...)
	holds, armed := e.s.Holds, false
	e.s.Holds = func(ctx context.Context, subject, perm string) (bool, error) {
		if armed {
			armed = false
			if err := c.change(e, u, admin); err != nil {
				t.Error(err)
			}
		}
		return holds(ctx, subject, perm)
	}
	return e, admin, func() { armed = true }
}

func TestAChangeWhileATicketIsBeingRedeemedLeavesNoSession(t *testing.T) {
	for name := range accountChanges {
		t.Run(name, func(t *testing.T) {
			e, admin, arm := raceEnv(t, name)
			ctx := context.Background()
			res, err := e.s.Launch(ctx, "user:alice", "work", "", admin)
			if err != nil {
				t.Fatal(err)
			}
			arm() // after the ticket is consumed, before the module session is stored
			r, ok := e.s.Redeem(ctx, "work", res.URL[strings.Index(res.URL, "t=")+2:], res.Binding)
			if ok || r.Handle != "" {
				t.Fatalf("redeemed across the change: %+v", r)
			}
			var n int
			if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_launch_sessions WHERE revoked_at=0`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%d live module sessions (%v)", n, err)
			}
		})
	}
}

func TestAChangeWhileLaunchingMintsNoTicket(t *testing.T) {
	for name := range accountChanges {
		t.Run(name, func(t *testing.T) {
			e, admin, arm := raceEnv(t, name)
			ctx := context.Background()
			arm()
			if _, err := e.s.Launch(ctx, "user:alice", "work", "", admin); err != ErrNotAllowed {
				t.Fatalf("launched from a session that ended meanwhile: %v", err)
			}
			var n int
			if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM login_tickets`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%d tickets (%v)", n, err)
			}
		})
	}
}

func TestPeekAppliesTheSamePermissionCheckAsRedeem(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tk, _ := launch(t, e, "user:alice", "work", "")
	e.drop("user:alice", "work.read")
	e.drop("user:alice", "work.write")
	if p, ok := e.s.Peek(ctx, "work", tk, e.bind[tk]); ok || p.Subject != "" || p.Name != "" {
		t.Fatalf("peek named somebody redeem would refuse: %+v", p)
	}
}

// The time the person authenticated travels from the admin session to the module session.
func TestAuthenticationTimeIsTheAdminSessionsNotTheLaunch(t *testing.T) {
	for name, c := range map[string]struct {
		session func(s *store.Session)
		want    time.Duration // before the launch; -1 = unknown
	}{
		"local sign-in": {func(s *store.Session) { s.CreatedAt = s.CreatedAt.Add(-time.Hour) }, time.Hour},
		"identity provider auth_time": {func(s *store.Session) {
			s.Source, s.AuthAt = store.SourceSSO, s.CreatedAt.Add(-3*time.Hour)
		}, 3 * time.Hour},
		"identity provider said nothing": {func(s *store.Session) { s.Source = store.SourceSSO }, -1},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			admin := e.admin(t, "user:alice", c.session)
			res, err := e.s.Launch(ctx, "user:alice", "work", "", admin)
			if err != nil {
				t.Fatal(err)
			}
			want := time.Time{}
			if c.want >= 0 {
				want = e.now.Add(-c.want)
			}
			r, ok := e.s.Redeem(ctx, "work", res.URL[strings.Index(res.URL, "t=")+2:], res.Binding)
			if !ok || !r.AuthenticatedAt.Equal(want) {
				t.Fatalf("redeem: %v %v, want %v", r.AuthenticatedAt, ok, want)
			}
			e.now = e.now.Add(time.Minute)
			v, ok := e.s.Revalidate(ctx, "work", r.Handle)
			if !ok || !v.AuthenticatedAt.Equal(want) {
				t.Fatalf("revalidate: %v %v, want %v", v.AuthenticatedAt, ok, want)
			}
		})
	}
}

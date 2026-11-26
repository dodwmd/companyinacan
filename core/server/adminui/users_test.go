package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/users"
)

type fakeMail struct {
	ready bool
	err   error
	sent  []string // "to|forUser|text"
}

func (f *fakeMail) Ready() bool { return f.ready }
func (f *fakeMail) Send(_ context.Context, to, forUser, _, text string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, to+"|"+forUser+"|"+text)
	return nil
}

type uui struct {
	*pui
	svc  *users.Service
	pats *pat.Service
	mail *fakeMail
}

func newUUI(t *testing.T) *uui {
	t.Helper()
	p := newPUI(t)
	ctx := context.Background()
	work := store.Module{ID: "work", Version: "1", Namespace: "mod-work", ServiceAccount: "work", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "work", Permissions: []string{"work.read", "work.write"}, Roles: map[string][]string{"viewer": {"work.read"}, "editor": {"work.read", "work.write"}}}}
	must(t, p.db.UpsertModule(ctx, work, nil, nil))
	clock := func() time.Time { return *p.now }
	pats := pat.New(p.db, p.audit)
	pats.Now = clock
	pats.Holds = func(ctx context.Context, subject, perm string) (bool, error) {
		return p.rb.Check(ctx, subject, perm, "")
	}
	u := &uui{pui: p, pats: pats, mail: &fakeMail{}}
	u.svc = &users.Service{DB: p.db, Audit: p.audit, Auth: p.ui.Auth, RBAC: p.rb, PATs: pats, Mail: u.mail, BaseURL: "https://admin.acme.example", Now: clock}
	p.ui.PATs, p.ui.Users = pats, u.svc
	return u
}

// post sends a form with the session's CSRF token.
func (u *uui) post(s session, path string, kv ...string) resp {
	u.t.Helper()
	form := url.Values{"csrf": {s.csrf}}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Add(kv[i], kv[i+1])
	}
	return u.do("POST", path, form, s.with()...)
}

func (u *uui) want(r resp, status int) resp {
	u.t.Helper()
	if r.StatusCode != status {
		u.t.Fatalf("%s %s = %d, want %d\n%s", r.Request.Method, r.Request.URL.Path, r.StatusCode, status, r.body)
	}
	return r
}

// manager gives alice rbac.manage through the work:viewer role: a manager who is not an owner.
func (u *uui) manager() session {
	u.t.Helper()
	ctx := context.Background()
	must(u.t, u.rb.Override(ctx, "root", "work:viewer", rbac.PermManage, rbac.EffectGrant))
	must(u.t, u.rb.Grant(ctx, "root", "user:alice", "work:viewer", ""))
	return u.loginAs("alice")
}

var linkRe = regexp.MustCompile(`/set-password\?token=([A-Za-z0-9_-]{43})`)
var secretShownRe = regexp.MustCompile(`<dd><code>([A-Z2-7]{32})</code></dd>`)

func tokenIn(t *testing.T, body string) string {
	t.Helper()
	m := linkRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no link in:\n%s", body)
	}
	return m[1]
}

// useLink opens the link and submits the form the way a person would.
func (u *uui) useLink(token, password string) resp {
	u.t.Helper()
	page := u.want(u.do("GET", "/set-password?token="+token, nil), 200)
	if probs := a11yProblems(page.body); len(probs) > 0 {
		u.t.Errorf("set-password: %v", probs)
	}
	m := secretShownRe.FindStringSubmatch(page.body)
	if m == nil {
		u.t.Fatalf("no secret:\n%s", page.body)
	}
	key, _ := auth.DecodeTOTPSecret(m[1])
	c := cookieNamed(page.Response, "__Host-can_login_csrf")
	form := url.Values{"csrf": {csrfOf(u.t, page.body)}, "token": {token}, "password": {password}, "totp": {auth.Code(key, auth.SHA1, 6, 30, *u.now)}}
	if strings.Contains(page.body, `name="confirm"`) {
		form.Set("confirm", password)
	}
	r := u.do("POST", "/set-password", form, c)
	if r.StatusCode == http.StatusSeeOther {
		u.secrets["bob"] = m[1]
	}
	return r
}

func (u *uui) signIn(user, password string) resp {
	u.t.Helper()
	*u.now = u.now.Add(31 * time.Second)
	c, tok := u.loginForm()
	key, _ := auth.DecodeTOTPSecret(u.secrets[user])
	return u.do("POST", "/login", url.Values{"username": {user}, "password": {password}, "totp": {auth.Code(key, auth.SHA1, 6, 30, *u.now)}, "csrf": {tok}}, c)
}

func (u *uui) actions() []string {
	u.t.Helper()
	recs, err := u.audit.List(context.Background(), 0, 500)
	must(u.t, err)
	var out []string
	for _, r := range recs {
		out = append(out, r.Action)
	}
	return out
}

const bobPassword = "a fresh long password"

func TestInviteSetPasswordAndSignIn(t *testing.T) {
	u := newUUI(t)
	root := u.loginAs("root")
	r := u.want(u.post(root, "/users", "username", "bob", "email", "bob@acme.example"), 200)
	token := tokenIn(t, r.body)
	if !strings.Contains(r.body, "https://admin.acme.example/set-password?token=") || len(u.mail.sent) != 0 {
		t.Fatalf("not configured to send: the link is shown with the configured address\n%s", r.body)
	}
	if probs := a11yProblems(r.body); len(probs) > 0 {
		t.Errorf("link page: %v", probs)
	}
	// Before the link is used the account cannot be told from an unknown one at sign-in.
	c, tok := u.loginForm()
	// An invited account has no code secret: the code computed from an empty one must not get further.
	empty := auth.Code(nil, auth.SHA1, 6, 30, *u.now)
	invited := u.do("POST", "/login", url.Values{"username": {"bob"}, "password": {bobPassword}, "totp": {empty}, "csrf": {tok}}, c)
	unknown := u.do("POST", "/login", url.Values{"username": {"nobody"}, "password": {bobPassword}, "totp": {"123456"}, "csrf": {tok}}, c)
	if invited.StatusCode != 401 || unknown.StatusCode != 401 {
		t.Fatalf("invited %d, unknown %d", invited.StatusCode, unknown.StatusCode)
	}
	// Only the hash is stored.
	var n int
	must(t, u.db.QueryRow(`SELECT COUNT(*) FROM user_invites WHERE token_hash=?`, token).Scan(&n))
	if n != 0 {
		t.Fatal("the token is stored in the clear")
	}
	// Opening twice consumes nothing; a short password and a wrong code are refused and keep the link.
	u.want(u.do("GET", "/set-password?token="+token, nil), 200)
	u.want(u.useLink(token, "short"), 400)
	u.want(u.useLink(token, bobPassword), 303)
	u.want(u.do("GET", "/set-password?token="+token, nil), 404)
	s := u.want(u.signIn("bob", bobPassword), 303)
	bob := session{cookie: cookieNamed(s.Response, "can_session")}
	u.want(u.do("GET", "/users", nil, bob.with()...), 403)
	u.want(u.do("GET", "/users/root", nil, bob.with()...), 403)
	u.want(u.do("GET", "/users/nobody", nil, bob.with()...), 403) // no enumeration for others
	got := strings.Join(u.actions(), " ")
	for _, a := range []string{"users.create", "users.setup"} {
		if !strings.Contains(got, a) {
			t.Errorf("no %s audit entry", a)
		}
	}
}

func TestInviteByEmailNeverShowsTheLink(t *testing.T) {
	u := newUUI(t)
	u.mail.ready = true
	root := u.loginAs("root")
	r := u.want(u.post(root, "/users", "username", "bob", "email", "bob@acme.example", "send", "1"), 200)
	if linkRe.MatchString(r.body) || len(u.mail.sent) != 1 || !strings.HasPrefix(u.mail.sent[0], "bob@acme.example|user:bob|") {
		t.Fatalf("sent=%d\n%s", len(u.mail.sent), r.body)
	}
	u.want(u.useLink(tokenIn(t, u.mail.sent[0]), bobPassword), 303)
	// A refused message falls back to showing the link, and says so.
	u.mail.err = errors.New("not on the allow-list")
	r = u.want(u.post(root, "/users", "username", "eve", "email", "eve@other.example", "send", "1"), 200)
	if !linkRe.MatchString(r.body) || !strings.Contains(r.body, "could not be sent") {
		t.Fatalf("fallback:\n%s", r.body)
	}
}

func TestLinkExpiresIsReplacedAndDiesWithItsMaker(t *testing.T) {
	u := newUUI(t)
	root := u.loginAs("root")
	first := tokenIn(t, u.want(u.post(root, "/users", "username", "bob"), 200).body)
	second := tokenIn(t, u.want(u.post(root, "/users/bob/link", "purpose", "invite"), 200).body)
	u.want(u.do("GET", "/set-password?token="+first, nil), 404)
	u.want(u.do("GET", "/set-password?token="+second, nil), 200)
	*u.now = u.now.Add(users.InviteTTL + time.Second)
	u.want(u.do("GET", "/set-password?token="+second, nil), 404)

	// A manager who is not an owner invites; once the account is given something the manager does
	// not hold, the manager's link stops working.
	alice := u.manager()
	token := tokenIn(t, u.want(u.post(alice, "/users", "username", "frank"), 200).body)
	u.want(u.do("GET", "/set-password?token="+token, nil), 200)
	must(t, u.rb.Grant(context.Background(), "root", "user:frank", "operator", ""))
	u.want(u.do("GET", "/set-password?token="+token, nil), 404)
	// And so does a link whose maker was disabled.
	token = tokenIn(t, u.want(u.post(alice, "/users", "username", "grace"), 200).body)
	root = u.loginAs("root")
	u.want(u.post(root, "/users/alice/disable"), 303)
	u.want(u.do("GET", "/set-password?token="+token, nil), 404)
}

func TestNobodyEscalates(t *testing.T) {
	u := newUUI(t)
	alice := u.manager()
	// What alice holds she can give; what she does not hold she cannot.
	u.want(u.post(alice, "/users/dave/roles/grant", "role", "work:viewer"), 303)
	for _, role := range []string{"work:editor", "operator", "owner"} {
		u.want(u.post(alice, "/users/carol/roles/grant", "role", role), 403)
		u.want(u.post(alice, "/access/carol/grant", "role", role), 403)
	}
	u.want(u.post(alice, "/users/alice/roles/grant", "role", "work:viewer"), 403)
	// The grant list offers only what she may give.
	page := u.want(u.do("GET", "/users/carol", nil, alice.with()...), 200).body
	if !strings.Contains(page, `<option value="work:viewer">`) || strings.Contains(page, `<option value="work:editor">`) || strings.Contains(page, `<option value="owner">`) {
		t.Fatalf("offered roles:\n%s", page)
	}
	// Owners are managed by owners only, on every path.
	rootPage := u.want(u.do("GET", "/users/root", nil, alice.with()...), 200).body
	if strings.Contains(rootPage, "/users/root/delete") || strings.Contains(rootPage, "/users/root/link") {
		t.Fatalf("offers to change an owner:\n%s", rootPage)
	}
	for _, c := range [][]string{
		{"/users/root/disable"}, {"/users/root/enable"}, {"/users/root/delete", "confirm", "root"},
		{"/users/root/link", "purpose", "mfa"}, {"/users/root/link", "purpose", "reset"},
		{"/users/root/sessions/revoke"}, {"/users/root/roles/revoke", "role", "owner"}, {"/users/root/roles/grant", "role", "work:viewer"},
		{"/access/root/revoke", "role", "owner"}, {"/access/roles/work:viewer/override", "permission", "work.write", "effect", "grant"},
	} {
		if r := u.post(alice, c[0], c[1:]...); r.StatusCode != 403 {
			t.Errorf("%v by a manager = %d, want 403", c, r.StatusCode)
		}
	}
	// Taking over an account that holds more than she does is escalation too.
	must(t, u.rb.Grant(context.Background(), "root", "user:carol", "work:editor", ""))
	u.want(u.post(alice, "/users/carol/link", "purpose", "mfa"), 403)
	u.want(u.post(alice, "/users/carol/link", "purpose", "reset"), 403)
	u.want(u.post(alice, "/users/dave/link", "purpose", "mfa"), 200)
}

func TestSensitiveChangesNeedARecentSignIn(t *testing.T) {
	u := newUUI(t)
	root := u.loginAs("root")
	u.want(u.post(root, "/users", "username", "bob"), 200)
	*u.now = u.now.Add(users.StepUpWindow + time.Second)
	for _, c := range [][]string{
		{"/users", "username", "mallory"}, {"/users/alice/disable"}, {"/users/alice/enable"}, {"/users/alice/delete", "confirm", "alice"},
		{"/users/alice/link", "purpose", "mfa"}, {"/users/alice/roles/grant", "role", "operator"}, {"/users/alice/roles/revoke", "role", "operator"},
		{"/access/alice/grant", "role", "operator"}, {"/access/alice/revoke", "role", "operator"}, {"/access/roles/work:viewer/override", "permission", "work.write", "effect", "grant"},
	} {
		r := u.post(root, c[0], c[1:]...)
		if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/reauth?next=") {
			t.Errorf("%v with an old sign-in = %d %q", c, r.StatusCode, r.Header.Get("Location"))
		}
	}
	if as, _ := u.rb.SubjectAssignments(context.Background(), "user:alice"); len(as) != 0 {
		t.Fatalf("changed without a step-up: %v", as)
	}
	page := u.want(u.do("GET", "/reauth?next=/users", nil, root.with()...), 200)
	if probs := a11yProblems(page.body); len(probs) > 0 {
		t.Errorf("reauth: %v", probs)
	}
	key, _ := auth.DecodeTOTPSecret(u.secrets["root"])
	code := func() string { return auth.Code(key, auth.SHA1, 6, 30, *u.now) }
	u.want(u.post(root, "/reauth", "password", "wrong password!!", "totp", code(), "next", "/users"), 401)
	r := u.want(u.post(root, "/reauth", "password", u.pw, "totp", code(), "next", "https://evil.example/"), 303)
	if loc := r.Header.Get("Location"); !strings.HasPrefix(loc, "/?") {
		t.Fatalf("open redirect: %q", loc)
	}
	u.want(u.post(root, "/users/alice/roles/grant", "role", "operator"), 303)
	// Confirming never uses up an allowance: many in a row all work.
	for range 8 {
		*u.now = u.now.Add(31 * time.Second)
		u.want(u.post(root, "/reauth", "password", u.pw, "totp", code(), "next", "/users"), 303)
	}
	// Guessing ends the session that guesses, and nothing else: signing in still works.
	for range 5 {
		u.want(u.post(root, "/reauth", "password", "wrong password!!", "totp", "000000", "next", "/"), 401)
	}
	r = u.want(u.post(root, "/reauth", "password", "wrong password!!", "totp", "000000", "next", "/"), 303)
	if r.Header.Get("Location") != "/login" {
		t.Fatalf("session must end: %q", r.Header.Get("Location"))
	}
	u.want(u.do("GET", "/settings", nil, root.with()...), 303)
	u.loginAs("root")
}

// holdings gives dave a role, a token and a live session.
func (u *uui) holdings() (dave session, token string) {
	u.t.Helper()
	ctx := context.Background()
	must(u.t, u.rb.Grant(ctx, "root", "user:dave", "work:editor", ""))
	dave = u.loginAs("dave")
	iss, err := u.pats.Issue(ctx, "user:dave", pat.IssueRequest{Name: "laptop", Module: "work", Permissions: []string{"work.read"}, TTL: 24 * time.Hour})
	must(u.t, err)
	if _, ok := u.pats.Introspect(ctx, "work", iss.Token); !ok {
		u.t.Fatal("token should work")
	}
	return dave, iss.Token
}

func (u *uui) assertCut(dave session, token string) {
	u.t.Helper()
	if _, ok := u.pats.Introspect(context.Background(), "work", token); ok {
		u.t.Error("the token still works")
	}
	if r := u.do("GET", "/settings", nil, dave.with()...); r.StatusCode != 303 {
		u.t.Errorf("the session still works: %d", r.StatusCode)
	}
}

func TestDisableRemoveRoleAndResetEndAccess(t *testing.T) {
	for name, act := range map[string][]string{
		"disable":     {"/users/dave/disable"},
		"role":        {"/users/dave/roles/revoke", "role", "work:editor"},
		"access page": {"/access/dave/revoke", "role", "work:editor"},
		"mfa reset":   {"/users/dave/link", "purpose", "mfa"},
		"delete":      {"/users/dave/delete", "confirm", "dave"},
		"override":    {"/access/roles/work:editor/override", "permission", "work.write", "effect", "revoke"},
		"sessions":    {"/users/dave/sessions/revoke"},
	} {
		t.Run(name, func(t *testing.T) {
			u := newUUI(t)
			dave, token := u.holdings()
			root := u.loginAs("root")
			if r := u.post(root, act[0], act[1:]...); r.StatusCode != 303 && r.StatusCode != 200 {
				t.Fatalf("%v = %d %s", act, r.StatusCode, r.body)
			}
			if name == "sessions" { // sessions only: tokens are revoked one by one
				if r := u.do("GET", "/settings", nil, dave.with()...); r.StatusCode != 303 {
					t.Fatal("session survived")
				}
				return
			}
			u.assertCut(dave, token)
		})
	}
}

func TestSessionsAndTokensPageActions(t *testing.T) {
	u := newUUI(t)
	dave, token := u.holdings()
	root := u.loginAs("root")
	page := u.want(u.do("GET", "/users/dave", nil, root.with()...), 200).body
	if probs := a11yProblems(page); len(probs) > 0 {
		t.Errorf("user page: %v", probs)
	}
	if strings.Contains(page, token) || strings.Contains(page, u.secrets["dave"]) {
		t.Fatal("the page leaks a credential")
	}
	for _, want := range []string{"work:editor", "<code>work.write</code>", "laptop"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, path := range []string{"/users", "/access"} {
		body := u.want(u.do("GET", path, nil, root.with()...), 200).body
		if probs := a11yProblems(body); len(probs) > 0 {
			t.Errorf("%s: %v", path, probs)
		}
	}
	tokID := regexp.MustCompile(`/users/dave/tokens/(pat_[0-9a-f]+)/revoke`).FindStringSubmatch(page)
	sid := regexp.MustCompile(`/users/dave/sessions/([0-9a-f]{16})/revoke`).FindStringSubmatch(page)
	if tokID == nil || sid == nil {
		t.Fatalf("no revoke forms:\n%s", page)
	}
	// Another user's token or session id does not work under this name.
	u.want(u.post(root, "/users/carol/tokens/"+tokID[1]+"/revoke"), 404)
	u.want(u.post(root, "/users/carol/sessions/"+sid[1]+"/revoke"), 404)
	u.want(u.post(root, "/users/dave/tokens/"+tokID[1]+"/revoke"), 303)
	u.want(u.post(root, "/users/dave/sessions/"+sid[1]+"/revoke"), 303)
	u.assertCut(dave, token)
}

func TestLastOwnerStaysOnEveryPath(t *testing.T) {
	u := newUUI(t)
	ctx := context.Background()
	root := u.loginAs("root")
	// A second owner who only has an invitation is not an owner who can sign in.
	u.want(u.post(root, "/users", "username", "bob"), 200)
	u.want(u.post(root, "/users/bob/roles/grant", "role", "owner"), 303)
	u.want(u.post(root, "/users/root/roles/revoke", "role", "owner"), 409)
	u.want(u.post(root, "/access/root/revoke", "role", "owner"), 409)
	u.want(u.post(root, "/users/root/disable"), 400)
	u.want(u.post(root, "/users/root/delete", "confirm", "root"), 400)
	u.want(u.post(root, "/users/root/link", "purpose", "reset"), 400)
	// The service refuses too when a stale actor tries after losing the other owner.
	must(t, u.rb.Grant(ctx, "root", "user:carol", "owner", ""))
	carol := u.loginAs("carol")
	u.want(u.post(carol, "/users/root/disable"), 303)
	if _, err := u.svc.Disable(ctx, users.Actor{User: store.AdminUser{Username: "root"}, Session: store.Session{CreatedAt: *u.now}}, "carol"); !errors.Is(err, users.ErrLastOwner) {
		t.Fatalf("disabling the last owner: %v", err)
	}
}

func TestMFAResetKeepsThePasswordAsAFactor(t *testing.T) {
	u := newUUI(t)
	root := u.loginAs("root")
	old := u.secrets["dave"]
	token := tokenIn(t, u.want(u.post(root, "/users/dave/link", "purpose", "mfa"), 200).body)
	u.secrets["dave"] = old
	if r := u.signIn("dave", u.pw); r.StatusCode != 401 {
		t.Fatalf("the old authenticator still works: %d", r.StatusCode)
	}
	for range 4 {
		u.want(u.useLink(token, "not the password at all"), 401)
	}
	u.want(u.useLink(token, "not the password at all"), 404) // burnt
	u.want(u.do("GET", "/set-password?token="+token, nil), 404)

	token = tokenIn(t, u.want(u.post(root, "/users/dave/link", "purpose", "mfa"), 200).body)
	u.want(u.useLink(token, u.pw), 303)
	u.secrets["dave"] = u.secrets["bob"]
	u.want(u.signIn("dave", u.pw), 303)
}

func TestSetPasswordPostNeedsItsCSRF(t *testing.T) {
	u := newUUI(t)
	root := u.loginAs("root")
	token := tokenIn(t, u.want(u.post(root, "/users", "username", "bob"), 200).body)
	u.want(u.do("POST", "/set-password", url.Values{"token": {token}, "password": {bobPassword}}), 403)
	u.want(u.do("POST", "/set-password", url.Values{"token": {token}, "password": {bobPassword}, "csrf": {"x"}}, &http.Cookie{Name: "__Host-can_login_csrf", Value: "y"}), 403)
	u.want(u.do("GET", "/set-password?token="+strings.Repeat("A", 43), nil), 404)
}

func TestDeletedNamesAreNeverReused(t *testing.T) {
	u := newUUI(t)
	ctx := context.Background()
	u.holdings()
	root := u.loginAs("root")
	u.want(u.post(root, "/users/dave/delete", "confirm", "wrong"), 400)
	u.want(u.post(root, "/users/dave/delete", "confirm", "dave"), 303)
	u.want(u.do("GET", "/users/dave", nil, root.with()...), 404)
	if as, _ := u.rb.SubjectAssignments(ctx, "user:dave"); len(as) != 0 {
		t.Fatalf("roles survive deletion: %v", as)
	}
	u.want(u.post(root, "/users", "username", "dave"), 400)
	u.want(u.post(root, "/users", "username", "Dave"), 400)
	u.want(u.post(root, "/users", "username", "ALICE"), 409)
	u.want(u.post(root, "/users", "username", "sso-x"), 400)
	u.want(u.post(root, "/users", "username", "../x"), 400)
	if _, err := u.db.CreateAdmin(ctx, "dave", "x", "y"); !errors.Is(err, store.ErrDeletedName) {
		t.Fatalf("bootstrap reuses a deleted name: %v", err)
	}
}

// A change that cannot be audited does not happen.
func TestChangesAreAuditedInTheSameTransaction(t *testing.T) {
	u := newUUI(t)
	u.holdings()
	root := u.loginAs("root")
	_, err := u.db.Exec(`CREATE TRIGGER no_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'no audit'); END`)
	must(t, err)
	for _, c := range [][]string{
		{"/users", "username", "bob"}, {"/users/dave/disable"}, {"/users/dave/delete", "confirm", "dave"},
		{"/users/dave/link", "purpose", "mfa"}, {"/users/dave/roles/revoke", "role", "work:editor"}, {"/users/dave/sessions/revoke"},
	} {
		if r := u.post(root, c[0], c[1:]...); r.StatusCode != 500 {
			t.Errorf("%v without audit = %d", c, r.StatusCode)
		}
	}
	var n int
	must(t, u.db.QueryRow(`SELECT (SELECT COUNT(*) FROM admin_users WHERE username='bob') + (SELECT COUNT(*) FROM admin_users WHERE username='dave' AND (disabled_at<>0 OR totp_secret=''))`).Scan(&n))
	var sessions int
	must(t, u.db.QueryRow(`SELECT COUNT(*) FROM sessions s JOIN admin_users a ON a.id=s.user_id WHERE a.username='dave'`).Scan(&sessions))
	as, _ := u.rb.SubjectAssignments(context.Background(), "user:dave")
	if n != 0 || sessions == 0 || len(as) != 1 {
		t.Fatalf("something changed without an audit entry: %d rows, %d sessions, %v", n, sessions, as)
	}
}

// One authorization path: the HTTP layers hold no SQL and call no role, identity or revocation
// change directly.
func TestHandlersDecideNothingAboutUsers(t *testing.T) {
	banned := regexp.MustCompile(`(RBAC\.(Grant|Revoke|Override)\(|SSO\.(Link|Unlink)\(|Identity\.(RevokeNow|Enable)\(|SELECT |INSERT |UPDATE |DELETE FROM)`)
	for _, dir := range []string{".", "../adminapi"} {
		files, _ := filepath.Glob(dir + "/*.go")
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			must(t, err)
			if m := banned.Find(b); m != nil {
				t.Errorf("%s: %q belongs in core/server/users or the store", f, m)
			}
		}
	}
}

// Two owners resetting each other at once must not leave nobody who can sign in: the second is
// refused although its request was authenticated before the first one ran.
func TestOwnersCannotResetEachOtherAway(t *testing.T) {
	u := newUUI(t)
	ctx := context.Background()
	must(t, u.rb.Grant(ctx, "root", "user:carol", "owner", ""))
	as := func(name string) users.Actor {
		a, err := u.db.AdminByUsername(ctx, name)
		must(t, err)
		return users.Actor{User: a, Session: store.Session{CreatedAt: *u.now}}
	}
	root, carol := as("root"), as("carol")
	if _, err := u.svc.SendLink(ctx, root, "carol", "reset", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := u.svc.SendLink(ctx, carol, "root", "reset", "", false); !errors.Is(err, users.ErrLastOwner) {
		t.Fatalf("the last owner who can sign in was reset: %v", err)
	}
}

// A single sign-on user steps up by authenticating again at the provider.
func TestSSOStepUpAsksTheProviderToAuthenticateAgain(t *testing.T) {
	s := newSSOHarness(t, nil)
	plain, _ := s.begin("/users")
	r := s.do("GET", "/auth/oidc/login?stepup=1&next=/users", nil)
	loc, err := url.Parse(r.Header.Get("Location"))
	must(t, err)
	if q := loc.Query(); q.Get("max_age") != "0" || q.Get("prompt") != "login" {
		t.Fatalf("step-up request: %s", loc)
	}
	if strings.Contains(plain, "prompt=login") {
		t.Fatalf("an ordinary sign-in forces none: %s", plain)
	}
}

// The older token page refuses a manager who is not an owner, instead of failing.
func TestOwnerTokensAreRevokedByOwnersOnly(t *testing.T) {
	u := newUUI(t)
	iss, err := u.pats.Issue(context.Background(), "user:root", pat.IssueRequest{Name: "ci", Module: "work", Permissions: []string{"work.read"}, TTL: 24 * time.Hour})
	must(t, err)
	alice := u.manager()
	u.want(u.post(alice, "/tokens/"+iss.ID+"/revoke"), 403)
	u.want(u.post(alice, "/users/root/tokens/"+iss.ID+"/revoke"), 403)
	if _, ok := u.pats.Introspect(context.Background(), "work", iss.Token); !ok {
		t.Fatal("the owner's token was revoked")
	}
}

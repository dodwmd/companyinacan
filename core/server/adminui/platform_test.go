package adminui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type pui struct {
	*harness
	rb      *rbac.Service
	eng     *approvals.Engine
	ntf     *notify.Service
	secrets map[string]string
}

func newPUI(t *testing.T) *pui {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	p := &pui{harness: h, secrets: map[string]string{"root": h.secret}}
	hash, _ := auth.HashPassword(h.pw)
	for _, u := range []string{"alice", "carol", "dave"} {
		p.secrets[u] = auth.NewTOTPSecret()
		if _, err := h.db.CreateAdmin(ctx, u, hash, p.secrets[u]); err != nil {
			t.Fatal(err)
		}
	}
	clock := func() time.Time { return *h.now }
	p.rb = rbac.New(h.db, h.audit, rbac.Options{Now: clock})
	p.ntf = notify.New(h.db, notify.Options{Now: clock, Mailer: &notify.FakeMailer{}, Roles: p.rb})
	p.eng = approvals.New(h.db, h.audit, approvals.Deps{Roles: p.rb, Perms: p.rb, Notifier: p.ntf, Now: clock})
	m := store.Module{ID: "expenses", Version: "1", Namespace: "mod-expenses", ServiceAccount: "expenses", State: store.StateEnabled, HMACSecret: "s", Manifest: config.Module{ID: "expenses"}}
	if err := h.db.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	pol := config.ApprovalPolicy{Steps: []config.ApprovalStep{{Name: "fin", Approvers: []config.Approver{{User: "user:carol"}}}}}
	if err := p.eng.SyncPolicies(ctx, map[string]config.ApprovalPolicy{"expense": pol}); err != nil {
		t.Fatal(err)
	}
	h.ui.RBAC, h.ui.Approvals, h.ui.Notify = p.rb, p.eng, p.ntf
	return p
}

func (p *pui) loginAs(user string) session {
	p.t.Helper()
	*p.now = p.now.Add(31 * time.Second)
	c, tok := p.loginForm()
	key, _ := authDecode(p.secrets[user])
	r := p.do("POST", "/login", url.Values{"username": {user}, "password": {p.pw}, "totp": {authCode(key, *p.now)}, "csrf": {tok}}, c)
	sc := cookieNamed(r.Response, "can_session")
	if sc == nil {
		p.t.Fatalf("login %s: %d %s", user, r.StatusCode, r.body)
	}
	s := session{cookie: &http.Cookie{Name: sc.Name, Value: sc.Value}}
	s.csrf = csrfOf(p.t, p.do("GET", "/settings", nil, s.cookie).body)
	return s
}

func (p *pui) request(requester, title string) string {
	p.t.Helper()
	r, _, err := p.eng.Create(context.Background(), approvals.CreateRequest{ModuleID: "expenses", Type: "expense", Requester: requester, Title: title, Amount: 100, Ref: "e-1",
		IdempotencyKey: title + requester + p.now.String()})
	if err != nil {
		p.t.Fatal(err)
	}
	return r.ID
}

func TestPlatformPagesNeedConfiguredServices(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	for _, path := range []string{"/approvals", "/notifications", "/access"} {
		if r := h.do("GET", path, nil, s.with()...); r.StatusCode != 503 {
			t.Errorf("%s without services = %d, want 503", path, r.StatusCode)
		}
	}
}

func TestApprovalInboxAndDetail(t *testing.T) {
	p := newPUI(t)
	id := p.request("user:alice", "<script>alert(1)</script> Laptop")
	carol, alice, dave := p.loginAs("carol"), p.loginAs("alice"), p.loginAs("dave")

	r := p.do("GET", "/approvals", nil, carol.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "/approvals/"+id) {
		t.Fatalf("carol inbox: %d\n%s", r.StatusCode, r.body)
	}
	if strings.Contains(r.body, "<script>alert") || !strings.Contains(r.body, "&lt;script&gt;") {
		t.Fatalf("title is not escaped:\n%s", r.body)
	}
	if r := p.do("GET", "/approvals", nil, alice.with()...); strings.Contains(r.body, `href="/approvals/`+id+`"`) && !strings.Contains(r.body, "My requests") {
		t.Fatal("requester sees the request only under their own requests")
	}
	d := p.do("GET", "/approvals/"+id, nil, carol.with()...)
	if d.StatusCode != 200 || !strings.Contains(d.body, `value="approve"`) || !strings.Contains(d.body, `value="reject"`) || !strings.Contains(d.body, `name="csrf"`) {
		t.Fatalf("approver detail lacks decision form: %d\n%s", d.StatusCode, d.body)
	}
	ad := p.do("GET", "/approvals/"+id, nil, alice.with()...)
	if ad.StatusCode != 200 || strings.Contains(ad.body, `value="approve"`) || !strings.Contains(ad.body, "/cancel") {
		t.Fatalf("requester detail must offer withdraw and no approve:\n%s", ad.body)
	}
	if r := p.do("GET", "/approvals/"+id, nil, dave.with()...); r.StatusCode != 404 {
		t.Fatalf("stranger = %d, want 404", r.StatusCode)
	}
	if r := p.do("GET", "/approvals/apr_nope", nil, carol.with()...); r.StatusCode != 404 {
		t.Fatalf("bad id = %d", r.StatusCode)
	}
	for _, body := range []string{r.body, d.body, ad.body} {
		if probs := a11yProblems(body); len(probs) > 0 {
			t.Errorf("accessibility problems: %v", probs)
		}
	}
}

func TestDecideThroughTheUI(t *testing.T) {
	p := newPUI(t)
	id := p.request("user:alice", "Laptop")
	carol, alice := p.loginAs("carol"), p.loginAs("alice")

	if r := p.do("POST", "/approvals/"+id+"/decide", url.Values{"decision": {"approve"}}, carol.with()...); r.StatusCode != 403 {
		t.Fatalf("no csrf token = %d, want 403", r.StatusCode)
	}
	if r := p.do("POST", "/approvals/"+id+"/decide", url.Values{"csrf": {alice.csrf}, "decision": {"approve"}}, alice.with()...); r.StatusCode != 403 {
		t.Fatalf("self approval = %d, want 403", r.StatusCode)
	}
	if r := p.do("POST", "/approvals/"+id+"/decide", url.Values{"csrf": {carol.csrf}, "decision": {"sideways"}}, carol.with()...); r.StatusCode != 400 {
		t.Fatalf("bad decision = %d", r.StatusCode)
	}
	if r := p.do("POST", "/approvals/"+id+"/comment", url.Values{"csrf": {carol.csrf}, "comment": {"checking the receipt"}}, carol.with()...); r.StatusCode != 303 {
		t.Fatalf("comment = %d", r.StatusCode)
	}
	r := p.do("POST", "/approvals/"+id+"/decide", url.Values{"csrf": {carol.csrf}, "decision": {"approve"}, "comment": {"fine <b>by</b> me"}}, carol.with()...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/approvals/"+id+"?flash=flash.decided" {
		t.Fatalf("approve = %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	d := p.do("GET", "/approvals/"+id, nil, carol.with()...)
	if !strings.Contains(d.body, "Approved") || !strings.Contains(d.body, "checking the receipt") || strings.Contains(d.body, "<b>by</b>") || !strings.Contains(d.body, "&lt;b&gt;by&lt;/b&gt;") {
		t.Fatalf("history missing or unescaped:\n%s", d.body)
	}
	if strings.Contains(d.body, `value="approve"`) {
		t.Fatal("a decided request offers no decision form")
	}
	if r := p.do("POST", "/approvals/"+id+"/decide", url.Values{"csrf": {carol.csrf}, "decision": {"approve"}}, carol.with()...); r.StatusCode != 409 {
		t.Fatalf("deciding twice = %d, want 409", r.StatusCode)
	}
	// the requester is told
	if n, _ := p.ntf.UnreadCount(context.Background(), "user:alice"); n != 1 {
		t.Fatalf("requester notifications = %d", n)
	}
}

func TestWithdrawThroughTheUI(t *testing.T) {
	p := newPUI(t)
	id := p.request("user:alice", "Laptop")
	alice, carol := p.loginAs("alice"), p.loginAs("carol")
	if r := p.do("POST", "/approvals/"+id+"/cancel", url.Values{"csrf": {carol.csrf}}, carol.with()...); r.StatusCode != 403 {
		t.Fatalf("approver withdrawing = %d", r.StatusCode)
	}
	if r := p.do("POST", "/approvals/"+id+"/cancel", url.Values{"csrf": {alice.csrf}, "reason": {"changed my mind"}}, alice.with()...); r.StatusCode != 303 {
		t.Fatalf("withdraw = %d", r.StatusCode)
	}
	if d := p.do("GET", "/approvals/"+id, nil, alice.with()...); !strings.Contains(d.body, "Cancelled") {
		t.Fatalf("status not shown:\n%s", d.body)
	}
}

func TestNotificationsPageAndBadge(t *testing.T) {
	p := newPUI(t)
	ctx := context.Background()
	alice := p.loginAs("alice")
	for i := 0; i < 2; i++ {
		_, _, _ = p.ntf.Notify(ctx, notify.Notice{Subject: "user:alice", Type: "lead.created", Data: map[string]string{"lead_id": "l1"}, Link: "/leads/l1"})
	}
	home := p.do("GET", "/approvals", nil, alice.with()...)
	if !strings.Contains(home.body, `class="count"`) || !strings.Contains(home.body, ">2<") {
		t.Fatalf("unread badge missing:\n%s", home.body)
	}
	n := p.do("GET", "/notifications", nil, alice.with()...)
	if n.StatusCode != 200 || !strings.Contains(n.body, "/leads/l1") || !strings.Contains(n.body, "/notifications/read") {
		t.Fatalf("notifications page: %d\n%s", n.StatusCode, n.body)
	}
	if probs := a11yProblems(n.body); len(probs) > 0 {
		t.Errorf("accessibility problems: %v", probs)
	}
	items, _ := p.ntf.List(ctx, "user:alice", notify.ListOptions{Limit: 5})
	one := p.do("POST", "/notifications/"+itoa(items[0].ID)+"/read", url.Values{"csrf": {alice.csrf}}, alice.with()...)
	if one.StatusCode != 303 {
		t.Fatalf("mark one = %d", one.StatusCode)
	}
	if c, _ := p.ntf.UnreadCount(ctx, "user:alice"); c != 1 {
		t.Fatalf("unread = %d", c)
	}
	if r := p.do("POST", "/notifications/read", url.Values{"csrf": {alice.csrf}}, alice.with()...); r.StatusCode != 303 {
		t.Fatal("mark all")
	}
	if c, _ := p.ntf.UnreadCount(ctx, "user:alice"); c != 0 {
		t.Fatal("unread after read-all")
	}
	if page := p.do("GET", "/approvals", nil, alice.with()...); strings.Contains(page.body, `class="count"`) {
		t.Fatal("no badge when nothing is unread")
	}
	// settings and preferences
	r := p.do("POST", "/notifications/settings", url.Values{"csrf": {alice.csrf}, "email": {"alice@example.com"}, "quiet_start": {"22:00"}, "quiet_end": {"07:00"}, "tz": {"Australia/Sydney"}, "digest": {"daily"}}, alice.with()...)
	if r.StatusCode != 303 {
		t.Fatalf("settings = %d\n%s", r.StatusCode, r.body)
	}
	if set, _ := p.ntf.Settings(ctx, "user:alice"); set.Email != "alice@example.com" || set.QuietStart != 22*60 || set.QuietEnd != 7*60 || set.Digest != "daily" {
		t.Fatalf("settings = %+v", set)
	}
	if r := p.do("POST", "/notifications/settings", url.Values{"csrf": {alice.csrf}, "email": {"nope"}, "quiet_start": {""}, "quiet_end": {""}, "tz": {"UTC"}, "digest": {"off"}}, alice.with()...); r.StatusCode != 400 {
		t.Fatalf("bad email = %d", r.StatusCode)
	}
	if r := p.do("POST", "/notifications/prefs", url.Values{"csrf": {alice.csrf}, "type": {"lead.created"}, "channel": {"email"}}, alice.with()...); r.StatusCode != 303 {
		t.Fatalf("pref = %d", r.StatusCode)
	}
	if ps, _ := p.ntf.Prefs(ctx, "user:alice"); len(ps) != 1 || ps[0].Enabled {
		t.Fatalf("an unchecked box means disabled: %+v", ps)
	}
	// cross-user mark-read
	dave := p.loginAs("dave")
	if r := p.do("POST", "/notifications/"+itoa(items[1].ID)+"/read", url.Values{"csrf": {dave.csrf}}, dave.with()...); r.StatusCode != 404 {
		t.Fatalf("cross-user mark-read = %d", r.StatusCode)
	}
}

func TestAccessPageAndLeastPrivilege(t *testing.T) {
	p := newPUI(t)
	alice, root := p.loginAs("alice"), p.loginAs("root")
	// without a role, the operational pages are closed and / sends the user to their inbox
	for _, path := range []string{"/modules", "/audit", "/events", "/leads", "/access"} {
		if r := p.do("GET", path, nil, alice.with()...); r.StatusCode != 403 {
			t.Errorf("alice GET %s = %d, want 403", path, r.StatusCode)
		}
	}
	if r := p.do("GET", "/", nil, alice.with()...); r.StatusCode != 303 || r.Header.Get("Location") != "/approvals" {
		t.Fatalf("dashboard for a user without platform.view = %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := p.do("GET", "/settings", nil, alice.with()...); r.StatusCode != 200 {
		t.Fatal("everyone can reach their own settings")
	}
	if r := p.do("POST", "/audit/verify", url.Values{"csrf": {alice.csrf}}, alice.with()...); r.StatusCode != 403 {
		t.Fatalf("verify without role = %d", r.StatusCode)
	}

	acc := p.do("GET", "/access", nil, root.with()...)
	if acc.StatusCode != 200 || !strings.Contains(acc.body, "alice") || !strings.Contains(acc.body, "owner") {
		t.Fatalf("access page: %d\n%s", acc.StatusCode, acc.body)
	}
	if probs := a11yProblems(acc.body); len(probs) > 0 {
		t.Errorf("accessibility problems: %v", probs)
	}
	if r := p.do("POST", "/access/alice/grant", url.Values{"csrf": {alice.csrf}, "role": {"owner"}}, alice.with()...); r.StatusCode != 403 {
		t.Fatalf("self-promotion = %d", r.StatusCode)
	}
	if r := p.do("POST", "/access/alice/grant", url.Values{"csrf": {root.csrf}, "role": {"operator"}}, root.with()...); r.StatusCode != 303 {
		t.Fatalf("grant = %d", r.StatusCode)
	}
	if r := p.do("GET", "/modules", nil, alice.with()...); r.StatusCode != 200 {
		t.Fatalf("operator can list modules = %d", r.StatusCode)
	}
	if r := p.do("GET", "/access", nil, alice.with()...); r.StatusCode != 403 {
		t.Fatal("operator cannot manage roles")
	}
	if r := p.do("POST", "/access/alice/grant", url.Values{"csrf": {root.csrf}, "role": {"nope"}}, root.with()...); r.StatusCode != 400 {
		t.Fatalf("unknown role = %d", r.StatusCode)
	}
	if r := p.do("POST", "/access/alice/revoke", url.Values{"csrf": {root.csrf}, "role": {"operator"}}, root.with()...); r.StatusCode != 303 {
		t.Fatal("revoke")
	}
	if r := p.do("GET", "/modules", nil, alice.with()...); r.StatusCode != 403 {
		t.Fatal("revoked")
	}
	if r := p.do("POST", "/access/root/revoke", url.Values{"csrf": {root.csrf}, "role": {"owner"}}, root.with()...); r.StatusCode != 409 {
		t.Fatalf("last owner = %d, want 409", r.StatusCode)
	}
	if r := p.do("POST", "/access/ghost/grant", url.Values{"csrf": {root.csrf}, "role": {"operator"}}, root.with()...); r.StatusCode != 404 {
		t.Fatalf("unknown user = %d", r.StatusCode)
	}
}

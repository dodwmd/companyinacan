package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
)

type pHarness struct {
	t       *testing.T
	srv     *httptest.Server
	db      *store.DB
	rb      *rbac.Service
	eng     *approvals.Engine
	ntf     *notify.Service
	pats    *pat.Service
	tickets *ticket.Service
	now     *time.Time
	secrets map[string]string
	pw      string
}

func newPHarness(t *testing.T) *pHarness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	h := &pHarness{t: t, db: db, now: &now, secrets: map[string]string{}, pw: "correct horse battery staple"}
	hash, _ := auth.HashPassword(h.pw)
	for _, u := range []string{"root", "alice", "carol", "dave"} { // root is the first admin and so the owner
		h.secrets[u] = auth.NewTOTPSecret()
		if _, err := db.CreateAdmin(ctx, u, hash, h.secrets[u]); err != nil {
			t.Fatal(err)
		}
	}
	clock := func() time.Time { return now }
	al := audit.New(db)
	h.rb = rbac.New(db, al, rbac.Options{Now: clock})
	h.ntf = notify.New(db, notify.Options{Now: clock, Mailer: &notify.FakeMailer{}, Roles: h.rb, PushSender: pushOK{}})
	h.eng = approvals.New(db, al, approvals.Deps{Roles: h.rb, Perms: h.rb, Notifier: h.ntf, Now: clock})
	m := store.Module{ID: "expenses", Version: "1", Namespace: "mod-expenses", ServiceAccount: "expenses", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "expenses", Permissions: []string{"expenses.approve", "expenses.pay"}, Roles: map[string][]string{"finance": {"expenses.approve"}}}}
	m.Manifest.Name = "Expenses <b>"
	m.Manifest.Runtime.Subdomain = "expenses"
	m.Manifest.Runtime.WebLogin = &config.WebLogin{ViaSubdomain: true}
	if err := db.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.SyncPolicies(ctx, map[string]config.ApprovalPolicy{"expense": {Steps: []config.ApprovalStep{{Name: "fin", Approvers: []config.Approver{{User: "user:carol"}}}}}}); err != nil {
		t.Fatal(err)
	}
	h.tickets = &ticket.Service{DB: db, Audit: al, Now: clock, Launcher: ticket.Launcher{Domain: "example.org"},
		Holds: func(ctx context.Context, subject, perm string) (bool, error) {
			return h.rb.Check(ctx, subject, perm, "")
		}}
	h.pats = pat.New(db, al)
	h.pats.Now = clock
	h.pats.Holds = func(ctx context.Context, subject, perm string) (bool, error) {
		return h.rb.Check(ctx, subject, perm, "")
	}
	s := &Server{
		Auth: auth.NewService(db, al, auth.Options{Now: clock, MinLoginTime: time.Nanosecond, Sleep: func(context.Context, time.Duration) error { return nil }, Policy: auth.Policy{HardLock: true, MaxUserFailures: 5, MaxIPFailures: 20, AttemptsPerMin: 1000, Window: 15 * time.Minute, LockFor: 15 * time.Minute}}),
		DB:   db, Audit: al, Registry: registry.New(db, "http://can-core"), Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Secure: true, Now: clock,
		RBAC: h.rb, Approvals: h.eng, Notify: h.ntf, PATs: h.pats, Tickets: h.tickets,
	}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

type sess struct {
	cookie *http.Cookie
	csrf   string
}

func (h *pHarness) login(user string) sess {
	h.t.Helper()
	*h.now = h.now.Add(31 * time.Second)
	k, _ := auth.DecodeTOTPSecret(h.secrets[user])
	b, _ := json.Marshal(map[string]string{"username": user, "password": h.pw, "totp": auth.Code(k, auth.SHA1, 6, 30, *h.now)})
	resp, err := http.Post(h.srv.URL+"/api/v1/login", "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var v sessionView
	_ = json.NewDecoder(resp.Body).Decode(&v)
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			return sess{c, v.CSRFToken}
		}
	}
	h.t.Fatalf("login %s: %d", user, resp.StatusCode)
	return sess{}
}

func (h *pHarness) call(s sess, method, path string, body any) (int, string) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	req.Host = "admin.example.org"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(s.cookie)
	if method != "GET" {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *pHarness) request(requester string) string {
	h.t.Helper()
	r, _, err := h.eng.Create(context.Background(), approvals.CreateRequest{ModuleID: "expenses", Type: "expense", Requester: requester, Title: "Laptop", Amount: 100, IdempotencyKey: "k-" + requester + h.t.Name() + time.Now().String()})
	if err != nil {
		h.t.Fatal(err)
	}
	return r.ID
}

func TestLeastPrivilegeGatesOperationalRoutes(t *testing.T) {
	h := newPHarness(t)
	alice, root := h.login("alice"), h.login("root")
	for _, p := range []string{"/api/v1/modules", "/api/v1/audit", "/api/v1/deliveries", "/api/v1/capabilities", "/api/v1/roles", "/api/v1/users"} {
		if code, _ := h.call(alice, "GET", p, nil); code != 403 {
			t.Errorf("alice GET %s = %d, want 403 (no role)", p, code)
		}
		if code, _ := h.call(root, "GET", p, nil); code != 200 {
			t.Errorf("owner GET %s = %d", p, code)
		}
	}
	if code, _ := h.call(alice, "GET", "/api/v1/session", nil); code != 200 {
		t.Fatal("every signed-in user can read their own session")
	}
}

func TestRoleAssignmentTakesEffectAtOnceAndIsAudited(t *testing.T) {
	h := newPHarness(t)
	alice, root := h.login("alice"), h.login("root")
	if code, _ := h.call(alice, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "owner"}); code != 403 {
		t.Fatalf("self-promotion = %d, want 403", code)
	}
	if code, body := h.call(root, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "operator"}); code != 204 {
		t.Fatalf("grant = %d %s", code, body)
	}
	if code, _ := h.call(alice, "GET", "/api/v1/modules", nil); code != 200 {
		t.Fatal("operator may list modules right after the grant")
	}
	if code, _ := h.call(root, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "nonexistent"}); code != 400 {
		t.Fatalf("unknown role = %d", code)
	}
	if code, _ := h.call(root, "POST", "/api/v1/users/nobody/roles", map[string]string{"role": "operator"}); code != 404 {
		t.Fatalf("unknown user = %d", code)
	}
	if code, body := h.call(root, "GET", "/api/v1/users", nil); code != 200 || !strings.Contains(body, `"username":"alice"`) || !strings.Contains(body, `"role":"operator"`) {
		t.Fatalf("users = %d %s", code, body)
	}
	if code, _ := h.call(root, "POST", "/api/v1/users/alice/roles/revoke", map[string]string{"role": "operator"}); code != 204 {
		t.Fatal("revoke")
	}
	if code, _ := h.call(alice, "GET", "/api/v1/modules", nil); code != 403 {
		t.Fatal("revoked role must stop working at once")
	}
	if code, _ := h.call(root, "POST", "/api/v1/users/root/roles/revoke", map[string]string{"role": "owner"}); code != 409 {
		t.Fatalf("revoking the last owner = %d, want 409", code)
	}
	recs, _ := audit.New(h.db).List(context.Background(), 0, 200)
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Action] = true
	}
	if !seen["rbac.grant"] || !seen["rbac.revoke"] {
		t.Fatalf("audit lacks role changes: %v", seen)
	}
}

func TestRoleOverrideEndpoint(t *testing.T) {
	h := newPHarness(t)
	alice, root := h.login("alice"), h.login("root")
	if code, _ := h.call(alice, "POST", "/api/v1/roles/expenses:finance/overrides", map[string]string{"permission": "expenses.pay", "effect": "grant"}); code != 403 {
		t.Fatal("rbac.manage required")
	}
	if code, body := h.call(root, "POST", "/api/v1/roles/expenses:finance/overrides", map[string]string{"permission": "expenses.pay", "effect": "grant"}); code != 204 {
		t.Fatalf("override = %d %s", code, body)
	}
	if _, body := h.call(root, "GET", "/api/v1/roles", nil); !strings.Contains(body, "expenses.pay") {
		t.Fatalf("roles = %s", body)
	}
	if code, _ := h.call(root, "POST", "/api/v1/roles/owner/overrides", map[string]string{"permission": "x.y", "effect": "revoke"}); code != 400 {
		t.Fatal("the owner role cannot be edited")
	}
}

func TestApprovalFlowOverTheAdminAPI(t *testing.T) {
	h := newPHarness(t)
	alice, carol, dave := h.login("alice"), h.login("carol"), h.login("dave")
	id := h.request("user:alice")

	if code, body := h.call(carol, "GET", "/api/v1/approvals?view=waiting", nil); code != 200 || !strings.Contains(body, id) {
		t.Fatalf("carol's inbox = %d %s", code, body)
	}
	if code, body := h.call(alice, "GET", "/api/v1/approvals?view=waiting", nil); code != 200 || strings.Contains(body, id) {
		t.Fatalf("the requester must not see their own request waiting: %s", body)
	}
	if code, body := h.call(alice, "GET", "/api/v1/approvals?view=requested", nil); code != 200 || !strings.Contains(body, id) {
		t.Fatalf("requested = %d %s", code, body)
	}
	if code, _ := h.call(alice, "GET", "/api/v1/approvals?view=all", nil); code != 403 {
		t.Fatal("view=all needs approvals.admin")
	}
	if code, _ := h.call(dave, "GET", "/api/v1/approvals/"+id, nil); code != 404 {
		t.Fatalf("a stranger gets 404 (no existence oracle), got %d", code)
	}
	if code, body := h.call(alice, "GET", "/api/v1/approvals/"+id, nil); code != 200 || !strings.Contains(body, `"steps"`) {
		t.Fatalf("detail = %d %s", code, body)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/approvals/"+id+"/decision", map[string]string{"decision": "approve"}); code != 403 {
		t.Fatalf("self approval = %d, want 403", code)
	}
	if code, _ := h.call(dave, "POST", "/api/v1/approvals/"+id+"/decision", map[string]string{"decision": "approve"}); code != 404 {
		t.Fatal("a stranger deciding gets 404")
	}
	if code, _ := h.call(carol, "POST", "/api/v1/approvals/"+id+"/decision", map[string]string{"decision": "maybe"}); code != 400 {
		t.Fatal("bad decision")
	}
	if code, body := h.call(carol, "POST", "/api/v1/approvals/"+id+"/comments", map[string]string{"comment": "looks fine"}); code != 204 {
		t.Fatalf("comment = %d %s", code, body)
	}
	if code, body := h.call(carol, "POST", "/api/v1/approvals/"+id+"/decision", map[string]string{"decision": "approve", "comment": "ok"}); code != 200 || !strings.Contains(body, `"status":"approved"`) {
		t.Fatalf("decision = %d %s", code, body)
	}
	if code, _ := h.call(carol, "POST", "/api/v1/approvals/"+id+"/decision", map[string]string{"decision": "approve"}); code != 409 {
		t.Fatal("deciding a decided request is 409")
	}
	if code, body := h.call(alice, "GET", "/api/v1/notifications?unread=true", nil); code != 200 || !strings.Contains(body, "approval.decided") {
		t.Fatalf("requester should be notified of the outcome: %d %s", code, body)
	}
}

func TestCancelAndDelegationOverTheAdminAPI(t *testing.T) {
	h := newPHarness(t)
	alice, carol := h.login("alice"), h.login("carol")
	id := h.request("user:alice")
	if code, _ := h.call(carol, "POST", "/api/v1/approvals/"+id+"/cancel", map[string]string{}); code != 403 {
		t.Fatalf("an approver cannot withdraw another person's request: %d", code)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/approvals/"+id+"/cancel", map[string]string{"reason": "oops"}); code != 204 {
		t.Fatal("requester can withdraw")
	}
	from := h.now.Format(time.RFC3339)
	to := h.now.Add(48 * time.Hour).Format(time.RFC3339)
	code, body := h.call(carol, "POST", "/api/v1/delegations", map[string]string{"delegate": "user:dave", "from": from, "to": to})
	if code != 201 {
		t.Fatalf("delegate = %d %s", code, body)
	}
	var d struct{ ID int64 }
	_ = json.Unmarshal([]byte(body), &d)
	id2 := h.request("user:alice")
	dave := h.login("dave")
	if code, body := h.call(dave, "GET", "/api/v1/approvals?view=waiting", nil); code != 200 || !strings.Contains(body, id2) {
		t.Fatalf("delegate inbox = %d %s", code, body)
	}
	if code, body := h.call(carol, "GET", "/api/v1/delegations", nil); code != 200 || !strings.Contains(body, "user:dave") {
		t.Fatalf("list = %d %s", code, body)
	}
	if code, _ := h.call(dave, "POST", "/api/v1/delegations/"+itoa(d.ID)+"/revoke", map[string]string{}); code != 404 {
		t.Fatal("only the delegator revokes")
	}
	if code, _ := h.call(carol, "POST", "/api/v1/delegations/"+itoa(d.ID)+"/revoke", map[string]string{}); code != 204 {
		t.Fatal("revoke")
	}
	if code, _ := h.call(carol, "POST", "/api/v1/delegations", map[string]string{"delegate": "carol", "from": from, "to": to}); code != 400 {
		t.Fatal("malformed delegate subject")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestNotificationEndpoints(t *testing.T) {
	h := newPHarness(t)
	ctx := context.Background()
	alice, dave := h.login("alice"), h.login("dave")
	for i := 0; i < 3; i++ {
		_, _, _ = h.ntf.Notify(ctx, notify.Notice{Subject: "user:alice", Type: "lead.created", Data: map[string]string{"lead_id": "l1"}, Link: "/leads/l1"})
	}
	if code, body := h.call(alice, "GET", "/api/v1/notifications/unread-count", nil); code != 200 || !strings.Contains(body, `"unread":3`) {
		t.Fatalf("count = %d %s", code, body)
	}
	_, body := h.call(alice, "GET", "/api/v1/notifications?limit=2", nil)
	var page struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
		NextBefore int64 `json:"next_before"`
	}
	_ = json.Unmarshal([]byte(body), &page)
	if len(page.Items) != 2 || page.NextBefore == 0 {
		t.Fatalf("page = %s", body)
	}
	if code, _ := h.call(dave, "POST", "/api/v1/notifications/"+itoa(page.Items[0].ID)+"/read", map[string]string{}); code != 404 {
		t.Fatal("another user's notification is 404")
	}
	if code, _ := h.call(alice, "POST", "/api/v1/notifications/"+itoa(page.Items[0].ID)+"/read", map[string]string{}); code != 204 {
		t.Fatal("mark read")
	}
	if _, body := h.call(alice, "GET", "/api/v1/notifications/unread-count", nil); !strings.Contains(body, `"unread":2`) {
		t.Fatalf("count after read = %s", body)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/notifications/read-all", map[string]string{}); code != 204 {
		t.Fatal("read-all")
	}
	if _, body := h.call(alice, "GET", "/api/v1/notifications/unread-count", nil); !strings.Contains(body, `"unread":0`) {
		t.Fatalf("count after read-all = %s", body)
	}
	// settings and preferences
	if code, body := h.call(alice, "POST", "/api/v1/notification-settings", map[string]any{"email": "alice@example.com", "quiet_start": 1320, "quiet_end": 420, "tz": "Australia/Sydney", "digest": "daily"}); code != 204 {
		t.Fatalf("settings = %d %s", code, body)
	}
	if _, body := h.call(alice, "GET", "/api/v1/notification-settings", nil); !strings.Contains(body, "Australia/Sydney") {
		t.Fatalf("settings readback = %s", body)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/notification-settings", map[string]any{"email": "bad", "quiet_start": -1, "quiet_end": -1, "tz": "UTC", "digest": "off"}); code != 400 {
		t.Fatal("bad email")
	}
	if code, _ := h.call(alice, "POST", "/api/v1/notification-preferences", map[string]any{"type": "lead.created", "channel": "email", "enabled": false}); code != 204 {
		t.Fatal("preference")
	}
	if code, body := h.call(alice, "GET", "/api/v1/notification-preferences", nil); code != 200 || !strings.Contains(body, "lead.created") {
		t.Fatalf("prefs = %d %s", code, body)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/notification-preferences", map[string]any{"type": "x", "channel": "pigeon", "enabled": true}); code != 400 {
		t.Fatal("bad channel")
	}
	// delivery log is an admin view
	if code, _ := h.call(alice, "GET", "/api/v1/notification-deliveries", nil); code != 403 {
		t.Fatal("delivery log needs notifications.admin")
	}
	root := h.login("root")
	if code, body := h.call(root, "GET", "/api/v1/notification-deliveries", nil); code != 200 || !strings.Contains(body, `"channel":"inapp"`) {
		t.Fatalf("deliveries = %d %s", code, body)
	}
}

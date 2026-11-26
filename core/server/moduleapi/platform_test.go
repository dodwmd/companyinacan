package moduleapi

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
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

type lines map[string]string

func (l lines) ManagerOf(_ context.Context, s string) (string, error) {
	if m, ok := l[s]; ok {
		return m, nil
	}
	return "", approvals.ErrNoManager
}

type pf struct {
	t   *testing.T
	srv *httptest.Server
	rb  *rbac.Service
	eng *approvals.Engine
	ntf *notify.Service
	db  *store.DB
}

func newPlatform(t *testing.T, wire bool) *pf {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	tr := tokenreview.NewFake()
	reg := registry.New(db, "http://can-core")
	bus := events.NewBus(db)
	al := audit.New(db)
	rb := rbac.New(db, al, rbac.Options{Now: func() time.Time { return now }, BreakGlass: true})
	ntf := notify.New(db, notify.Options{Now: func() time.Time { return now }, Mailer: &notify.FakeMailer{}, Bus: bus, Roles: rb})
	eng := approvals.New(db, al, approvals.Deps{Roles: rb, Perms: rb, Lines: lines{"user:emp": "user:boss"}, Notifier: ntf, Bus: bus, Now: func() time.Time { return now }})
	s := &Server{Auth: modauth.New(tr, db), Registry: reg, Bus: bus, Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	if wire {
		s.RBAC, s.Approvals, s.Notify = rb, eng, ntf
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	exp := config.Module{ID: "expenses", Name: "expenses", Version: "1", Permissions: []string{"expenses.read", "expenses.approve"},
		Roles:     map[string][]string{"finance": {"expenses.approve"}},
		Approvals: []config.ManifestApproval{{Type: "expense", DefaultChain: []string{"manager"}}}}
	exp.Requires.Capabilities = []string{"notify"}
	other := config.Module{ID: "other", Name: "other", Version: "1"}
	for _, m := range []config.Module{exp, other} {
		if err := reg.Register(ctx, store.Module{ID: m.ID, Version: m.Version, Namespace: "mod-" + m.ID, ServiceAccount: m.ID, State: store.StateEnabled, HMACSecret: "s", Manifest: m}); err != nil {
			t.Fatal(err)
		}
		tr.Add("tok-"+m.ID, "mod-"+m.ID, m.ID)
	}
	if err := rb.Grant(ctx, rbac.CLIActor, "user:fin", "expenses:finance", ""); err != nil {
		t.Fatal(err)
	}
	return &pf{t: t, srv: srv, rb: rb, eng: eng, ntf: ntf, db: db}
}

func (p *pf) call(method, path, token string, hdr map[string]string, body any) (*http.Response, string) {
	p.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestCheckPermission(t *testing.T) {
	p := newPlatform(t, true)
	type q = map[string]string
	cases := []struct {
		name  string
		token string
		body  q
		code  int
		want  string
	}{
		{"granted", "tok-expenses", q{"subject": "user:fin", "permission": "expenses.approve"}, 200, `"allowed":true`},
		{"not granted", "tok-expenses", q{"subject": "user:nobody", "permission": "expenses.approve"}, 200, `"allowed":false`},
		{"role lacks it", "tok-expenses", q{"subject": "user:fin", "permission": "expenses.read"}, 200, `"allowed":false`},
		{"scoped question", "tok-expenses", q{"subject": "user:fin", "permission": "expenses.approve", "resource": "dept/a"}, 200, `"allowed":true`},
		{"permission of another module", "tok-other", q{"subject": "user:fin", "permission": "expenses.approve"}, 403, ""},
		{"bad subject", "tok-expenses", q{"subject": "fin", "permission": "expenses.approve"}, 400, ""},
		{"no token", "", q{"subject": "user:fin", "permission": "expenses.approve"}, 401, ""},
	}
	for _, c := range cases {
		resp, body := p.call("POST", "/v1/check", c.token, nil, c.body)
		if resp.StatusCode != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, body)
		}
	}
}

func TestPlatformServicesUnavailableWhenNotWired(t *testing.T) {
	p := newPlatform(t, false)
	resp, _ := p.call("POST", "/v1/check", "tok-expenses", nil, map[string]string{"subject": "user:a", "permission": "expenses.read"})
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestCreateApprovalIsIdempotentAndScoped(t *testing.T) {
	p := newPlatform(t, true)
	body := map[string]any{"type": "expense", "requester": "user:emp", "ref": "e-1", "title": "Laptop", "amount": 1200}
	idem := map[string]string{"Idempotency-Key": "k-1"}

	if resp, _ := p.call("POST", "/v1/approvals", "tok-expenses", nil, body); resp.StatusCode != 400 {
		t.Fatalf("missing Idempotency-Key: %d", resp.StatusCode)
	}
	resp, out := p.call("POST", "/v1/approvals", "tok-expenses", idem, body)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	var created struct{ ID, Status string }
	_ = json.Unmarshal([]byte(out), &created)
	if !strings.HasPrefix(created.ID, "apr_") || created.Status != "pending" {
		t.Fatalf("created = %s", out)
	}
	resp, out = p.call("POST", "/v1/approvals", "tok-expenses", idem, body)
	if resp.StatusCode != 200 || !strings.Contains(out, created.ID) {
		t.Fatalf("replay: %d %s", resp.StatusCode, out)
	}
	body["amount"] = 5
	if resp, _ = p.call("POST", "/v1/approvals", "tok-expenses", idem, body); resp.StatusCode != 409 {
		t.Fatalf("conflicting replay: %d", resp.StatusCode)
	}
	body["amount"] = 1200
	// a module can only use the approval types it declares
	body["type"] = "purchase"
	if resp, _ = p.call("POST", "/v1/approvals", "tok-expenses", map[string]string{"Idempotency-Key": "k-2"}, body); resp.StatusCode != 403 {
		t.Fatalf("undeclared type: %d", resp.StatusCode)
	}
	body["type"] = "expense"
	if resp, _ = p.call("POST", "/v1/approvals", "tok-other", map[string]string{"Idempotency-Key": "k-3"}, body); resp.StatusCode != 403 {
		t.Fatalf("other module: %d", resp.StatusCode)
	}
	// requester without a manager: the default chain cannot be staffed
	body["requester"] = "user:orphan"
	if resp, _ = p.call("POST", "/v1/approvals", "tok-expenses", map[string]string{"Idempotency-Key": "k-4"}, body); resp.StatusCode != 422 {
		t.Fatalf("no approver: %d", resp.StatusCode)
	}

	// read: the owner sees it, another module gets 404 (not 403: no oracle for ids)
	if resp, out = p.call("GET", "/v1/approvals/"+created.ID, "tok-expenses", nil, nil); resp.StatusCode != 200 || !strings.Contains(out, `"status":"pending"`) {
		t.Fatalf("get: %d %s", resp.StatusCode, out)
	}
	if resp, _ = p.call("GET", "/v1/approvals/"+created.ID, "tok-other", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("other module get: %d", resp.StatusCode)
	}
	if resp, _ = p.call("GET", "/v1/approvals/nope", "tok-expenses", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("bad id: %d", resp.StatusCode)
	}
	// cancel
	if resp, _ = p.call("POST", "/v1/approvals/"+created.ID+"/cancel", "tok-other", nil, map[string]string{}); resp.StatusCode != 404 {
		t.Fatalf("other module cancel: %d", resp.StatusCode)
	}
	if resp, out = p.call("POST", "/v1/approvals/"+created.ID+"/cancel", "tok-expenses", nil, map[string]string{"reason": "duplicate"}); resp.StatusCode != 204 {
		t.Fatalf("cancel: %d %s", resp.StatusCode, out)
	}
	if resp, _ = p.call("POST", "/v1/approvals/"+created.ID+"/cancel", "tok-expenses", nil, map[string]string{}); resp.StatusCode != 409 {
		t.Fatalf("second cancel: %d", resp.StatusCode)
	}
}

func TestModuleNotifications(t *testing.T) {
	p := newPlatform(t, true)
	good := map[string]any{"subject": "user:fin", "type": "expenses.reminder", "dedupe_key": "r1", "data": map[string]string{"title": "Receipts due"}, "link": "/approvals"}
	resp, out := p.call("POST", "/v1/notifications", "tok-expenses", nil, good)
	if resp.StatusCode != 202 || !strings.Contains(out, `"created":true`) {
		t.Fatalf("notify: %d %s", resp.StatusCode, out)
	}
	resp, out = p.call("POST", "/v1/notifications", "tok-expenses", nil, good)
	if resp.StatusCode != 202 || !strings.Contains(out, `"created":false`) {
		t.Fatalf("duplicate: %d %s", resp.StatusCode, out)
	}
	if n, _ := p.ntf.UnreadCount(context.Background(), "user:fin"); n != 1 {
		t.Fatalf("unread = %d", n)
	}
	spoof := map[string]any{"subject": "user:fin", "type": "approval.requested", "data": map[string]string{"request_id": "apr_x"}}
	if resp, _ = p.call("POST", "/v1/notifications", "tok-expenses", nil, spoof); resp.StatusCode != 400 {
		t.Fatalf("a module must not send platform notification types: %d", resp.StatusCode)
	}
	if resp, _ = p.call("POST", "/v1/notifications", "tok-other", nil, map[string]any{"subject": "user:fin", "type": "other.x"}); resp.StatusCode != 403 {
		t.Fatalf("module without the notify capability: %d", resp.StatusCode)
	}
	bad := map[string]any{"subject": "user:fin", "type": "expenses.x", "link": "https://evil.example/"}
	if resp, _ = p.call("POST", "/v1/notifications", "tok-expenses", nil, bad); resp.StatusCode != 400 {
		t.Fatalf("absolute link: %d", resp.StatusCode)
	}
}

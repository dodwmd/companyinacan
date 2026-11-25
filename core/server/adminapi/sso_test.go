package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/sso/ssotest"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type ssoEnv struct {
	*harness
	idp *ssotest.IdP
	svc *sso.Service
	al  *audit.Log
}

func newSSOEnv(t *testing.T, withSSO bool) *ssoEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().Truncate(time.Second)
	clock := func() time.Time { return now }
	db.Now = clock
	h := &harness{t: t, db: db, pw: "correct horse battery staple", secret: auth.NewTOTPSecret(), now: &now, logs: &bytes.Buffer{}}
	hash, _ := auth.HashPassword(h.pw)
	if _, err := db.CreateAdmin(ctx, "root", hash, h.secret); err != nil {
		t.Fatal(err)
	}
	al := audit.New(db)
	rb := rbac.New(db, al, rbac.Options{Now: clock})
	au := auth.NewService(db, al, auth.Options{Now: clock, MinLoginTime: time.Nanosecond, Sleep: func(context.Context, time.Duration) error { return nil }})
	e := &ssoEnv{harness: h, al: al, idp: ssotest.New(t, "/realms/acme")}
	e.idp.Now = clock
	s := &Server{Auth: au, DB: db, Audit: al, Registry: registry.New(db, "http://can-core"), RBAC: rb, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Secure: true, Now: clock}
	if withSSO {
		cfg := config.SSO{Enabled: true, Issuer: e.idp.Issuer, ClientID: e.idp.ClientID, ClientSecretRef: "acme-sso", RedirectBaseURL: "http://admin.test", AllowInsecureDev: true,
			GroupRoles: map[string][]string{"ops": {"operator"}}}
		e.svc = &sso.Service{DB: db, Auth: au, RBAC: rb, Audit: al, Now: clock, Secure: true, Secrets: sso.StaticSecrets{"acme-sso": e.idp.ClientSecret},
			Source: func(context.Context) (*config.SSO, string, error) { c := cfg; return &c, "acme.example", nil }}
		s.SSO = e.svc
	}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return e
}

func (e *ssoEnv) provision() store.SSOIdentity {
	e.t.Helper()
	loc, ck, err := e.svc.Begin(context.Background(), "")
	if err != nil {
		e.t.Fatal(err)
	}
	cb := e.idp.Approve(loc, ssotest.User{Sub: "u-1", Email: "o@acme.example", EmailVerified: true, Groups: []string{"ops"}})
	u, _ := url.Parse(cb)
	if _, err := e.svc.Complete(context.Background(), sso.CompleteRequest{Query: u.Query(), StateCookie: ck.Value, IP: "203.0.113.1"}); err != nil {
		e.t.Fatal(err)
	}
	ids, _ := e.db.ListSSOIdentities(context.Background(), 10)
	return ids[0]
}

func TestSSOEndpoints(t *testing.T) {
	e := newSSOEnv(t, true)
	id := e.provision()
	l := e.login()

	resp, b := e.do("GET", "/api/v1/sso", nil, l.with(false))
	if resp.StatusCode != 200 || strings.Contains(string(b), e.idp.ClientSecret) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	var st sso.Status
	_ = json.Unmarshal(b, &st)
	if !st.Enabled || st.ClientSecretRef != "acme-sso" || st.Identities != 1 || len(st.Mappings) != 1 {
		t.Fatalf("%+v", st)
	}

	// The discovery test is an unsafe method: CSRF is required.
	if resp, _ = e.do("POST", "/api/v1/sso/test", map[string]string{}, l.with(false)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf: %d", resp.StatusCode)
	}
	resp, b = e.do("POST", "/api/v1/sso/test", map[string]string{}, l.with(true))
	var tr sso.TestResult
	_ = json.Unmarshal(b, &tr)
	if resp.StatusCode != 200 || !tr.OK || tr.Keys != 2 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}

	resp, b = e.do("GET", "/api/v1/sso/identities", nil, l.with(false))
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"email":"o@acme.example"`) || !strings.Contains(string(b), `"linked":false`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}

	path := "/api/v1/sso/identities/" + strconv.FormatInt(id.ID, 10)
	if resp, _ = e.do("POST", path+"/link", map[string]string{"username": "root"}, l.with(false)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("link without csrf: %d", resp.StatusCode)
	}
	if resp, _ = e.do("POST", path+"/link", map[string]string{"username": "nobody"}, l.with(true)); resp.StatusCode != http.StatusConflict {
		t.Fatalf("unknown user: %d", resp.StatusCode)
	}
	if resp, _ = e.do("POST", path+"/link", map[string]string{"username": "root"}, l.with(true)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("link: %d", resp.StatusCode)
	}
	if resp, _ = e.do("POST", path+"/unlink", nil, l.with(true)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unlink: %d", resp.StatusCode)
	}
	if resp, _ = e.do("POST", "/api/v1/sso/identities/notanumber/link", map[string]string{"username": "root"}, l.with(true)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad id: %d", resp.StatusCode)
	}
}

func TestSSOEndpointsWithoutService(t *testing.T) {
	e := newSSOEnv(t, false)
	l := e.login()
	for _, p := range []string{"/api/v1/sso", "/api/v1/sso/identities"} {
		if resp, _ := e.do("GET", p, nil, l.with(false)); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := e.do("POST", "/api/v1/sso/test", map[string]string{}, l.with(true)); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("test: %d", resp.StatusCode)
	}
}

func TestSSOEndpointsNeedRBACManage(t *testing.T) {
	e := newSSOEnv(t, true)
	e.provision()
	ctx := context.Background()
	hash, _ := auth.HashPassword("another long password")
	if _, err := e.db.CreateAdmin(ctx, "auditor", hash, auth.NewTOTPSecret()); err != nil {
		t.Fatal(err)
	}
	// An SSO-provisioned operator has no rbac.manage; use a session of that user.
	ids, _ := e.db.ListSSOIdentities(ctx, 10)
	login, err := e.svc.Auth.CreateSSOSession(ctx, ids[0].UserID, "203.0.113.1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: auth.CookieName, Value: login.Token}
	resp, _ := e.do("GET", "/api/v1/sso", nil, func(r *http.Request) { r.AddCookie(cookie) })
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an operator may not read SSO administration: %d", resp.StatusCode)
	}
	resp, b := e.do("GET", "/api/v1/session", nil, func(r *http.Request) { r.AddCookie(cookie) })
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"source":"sso"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

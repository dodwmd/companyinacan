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
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
	db  *store.DB
	tr  *tokenreview.Fake
	reg *registry.Registry
}

func manifest(id string) config.Module {
	m := config.Module{ID: id, Name: id, Version: "1.0.0"}
	return m
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	tr := tokenreview.NewFake()
	reg := registry.New(db, "http://can-core.can-system.svc:8081")
	s := &Server{Auth: modauth.New(tr, db), Registry: reg, Bus: events.NewBus(db), Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	h := &harness{t: t, db: db, tr: tr, reg: reg, srv: httptest.NewServer(s.Handler())}
	t.Cleanup(h.srv.Close)

	// forms: publishes leads. crm: consumes tickets, subscribes to leads. support: provides tickets.
	forms := manifest("forms")
	forms.Publishes = append(forms.Publishes, struct {
		Event  string `yaml:"event"`
		Schema string `yaml:"schema"`
	}{Event: "lead.created.v1"})
	crm := manifest("crm")
	crm.Consumes = []string{"tickets"}
	crm.Requires.Capabilities = []string{"email.send"}
	crm.Subscribes = append(crm.Subscribes, struct {
		Event   string `yaml:"event"`
		Service string `yaml:"service"`
		Port    int    `yaml:"port"`
		Webhook string `yaml:"webhook"`
		Status  string `yaml:"status"`
	}{Event: "lead.created.v1", Service: "crm", Port: 8000, Webhook: "/hooks/lead"})
	support := manifest("support")
	support.Provides = append(support.Provides, struct {
		Capability string `yaml:"capability"`
		Schema     string `yaml:"schema"`
		Service    string `yaml:"service"`
		Port       int    `yaml:"port"`
		Status     string `yaml:"status"`
	}{Capability: "tickets", Service: "support", Port: 8080})
	for _, m := range []config.Module{forms, crm, support} {
		if err := reg.Register(ctx, store.Module{ID: m.ID, Version: m.Version, Namespace: "mod-" + m.ID, ServiceAccount: m.ID, State: store.StateEnabled, HMACSecret: "s", Manifest: m}); err != nil {
			t.Fatal(err)
		}
		tr.Add("tok-"+m.ID, "mod-"+m.ID, m.ID)
	}
	return h
}

func (h *harness) call(method, path, token string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestHealthzIsOpenEverythingElseNeedsAToken(t *testing.T) {
	h := newHarness(t)
	if resp, _ := h.call("GET", "/healthz", "", nil); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	for _, p := range Routes() {
		method, path, _ := strings.Cut(p, " ")
		if path == "/healthz" {
			continue
		}
		path = strings.ReplaceAll(path, "{name}", "tickets")
		resp, _ := h.call(method, path, "", nil)
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s anonymous -> %d", p, resp.StatusCode)
		}
		if resp, _ := h.call(method, path, "forged", nil); resp.StatusCode != 401 {
			t.Errorf("%s forged token -> %d", p, resp.StatusCode)
		}
	}
}

func TestSelf(t *testing.T) {
	h := newHarness(t)
	resp, b := h.call("GET", "/v1/self", "tok-support", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"id":"support"`) || !strings.Contains(string(b), `"tickets"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestIntrospect(t *testing.T) {
	h := newHarness(t)
	resp, b := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": "tok-crm"})
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"active":true`) || !strings.Contains(string(b), `"module_id":"crm"`) || !strings.Contains(string(b), `"namespace":"mod-crm"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	for _, tok := range []string{"garbage", ""} {
		resp, b = h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": tok})
		if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != `{"active":false}` {
			t.Fatalf("token %q: %d %s", tok, resp.StatusCode, b)
		}
	}
	// a disabled module's token is inactive too, without saying why
	_ = h.db.SetModuleState(context.Background(), "crm", store.StateDisabled, "")
	_, b = h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": "tok-crm"})
	if strings.TrimSpace(string(b)) != `{"active":false}` {
		t.Fatalf("%s", b)
	}
}

func TestResolveCapability(t *testing.T) {
	h := newHarness(t)
	resp, b := h.call("GET", "/v1/capabilities/tickets", "tok-crm", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "http://support.mod-support.svc.cluster.local:8080") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// platform capability provided by can-core itself
	resp, b = h.call("GET", "/v1/capabilities/email.send", "tok-crm", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"module":"can-core"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// forms never declared tickets: least privilege
	if resp, _ := h.call("GET", "/v1/capabilities/tickets", "tok-forms", nil); resp.StatusCode != 403 {
		t.Fatalf("undeclared consumer: %d", resp.StatusCode)
	}
	// declared but nobody provides it
	crm, _ := h.db.GetModule(context.Background(), "crm")
	crm.Manifest.Consumes = append(crm.Manifest.Consumes, "files")
	_ = h.reg.Register(context.Background(), crm)
	if resp, _ := h.call("GET", "/v1/capabilities/files", "tok-crm", nil); resp.StatusCode != 404 {
		t.Fatalf("no provider: %d", resp.StatusCode)
	}
}

func TestPublishEventRequiresDeclarationAndFansOut(t *testing.T) {
	h := newHarness(t)
	resp, b := h.call("POST", "/v1/events", "tok-forms", map[string]any{"type": "lead.created.v1", "data": map[string]any{"email": "a@example.com"}})
	if resp.StatusCode != 202 || !strings.Contains(string(b), `"id":"evt_`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	ds, _ := h.db.ListDeliveries(context.Background(), 5)
	if len(ds) != 1 || ds[0].ModuleID != "crm" {
		t.Fatalf("fan-out: %+v", ds)
	}
	if ds[0].Event.Source != "forms" {
		t.Fatalf("source must be the authenticated module, got %q", ds[0].Event.Source)
	}
	// crm did not declare lead.created.v1 under publishes
	if resp, _ := h.call("POST", "/v1/events", "tok-crm", map[string]any{"type": "lead.created.v1", "data": map[string]any{}}); resp.StatusCode != 403 {
		t.Fatalf("undeclared publisher: %d", resp.StatusCode)
	}
	if resp, _ := h.call("POST", "/v1/events", "tok-forms", map[string]any{"type": "bad", "data": map[string]any{}}); resp.StatusCode != 400 {
		t.Fatalf("bad type: %d", resp.StatusCode)
	}
	if resp, _ := h.call("POST", "/v1/events", "tok-forms", map[string]any{"type": "lead.created.v1", "data": []int{1}}); resp.StatusCode != 400 {
		t.Fatalf("non-object data: %d", resp.StatusCode)
	}
	if resp, _ := h.call("POST", "/v1/events", "tok-forms", map[string]any{"type": "lead.created.v1", "source": "crm"}); resp.StatusCode != 400 {
		t.Fatalf("client-supplied source must be rejected: %d", resp.StatusCode)
	}
}

func TestDisabledModuleIsForbidden(t *testing.T) {
	h := newHarness(t)
	_ = h.db.SetModuleState(context.Background(), "crm", store.StateDisabled, "")
	if resp, _ := h.call("GET", "/v1/self", "tok-crm", nil); resp.StatusCode != 403 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

type failingReviewer struct{}

func (failingReviewer) Review(context.Context, string, string) (tokenreview.Identity, error) {
	return tokenreview.Identity{}, io.ErrUnexpectedEOF
}

func TestTokenReviewOutageIsA502NotA401(t *testing.T) {
	h := newHarness(t)
	s := &Server{Auth: modauth.New(failingReviewer{}, h.db), Registry: h.reg, Bus: events.NewBus(h.db), Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/self", nil)
	req.Header.Set("Authorization", "Bearer x")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("%d", resp.StatusCode)
	}
}

package server_test

import (
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
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

// TestStubModuleLifecycle is the E03 "done when", minus the cluster: a stub module is enabled,
// receives injected config (including its webhook signing secret), calls the module API with its
// projected token, receives a signed event, and is disabled again, after which its token stops working.
// Kubernetes is replaced by the fake installer and fake TokenReview; the k3d version of this test
// belongs to the Helm-installer follow-up.
func TestStubModuleLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }

	reviewer := tokenreview.NewFake()
	reg := registry.New(db, "http://can-core.can-system.svc:8081")
	al := audit.New(db)
	bus := events.NewBus(db)
	installer := reconcile.NewFakeInstaller()
	rec := reconcile.New(db, reg, al, installer)

	api := httptest.NewServer((&moduleapi.Server{Auth: modauth.New(reviewer, db), Registry: reg, Bus: bus, Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}).Handler())
	defer api.Close()

	// The stub module's manifest: publishes lead events, subscribes to them, consumes tickets, provides contacts.
	stub := &config.Module{ID: "stub", Name: "Stub", Version: "0.1.0"}
	stub.Requires.Capabilities = []string{"email.send"}
	stub.Publishes = append(stub.Publishes, struct {
		Event  string `yaml:"event"`
		Schema string `yaml:"schema"`
	}{Event: "lead.created.v1"})
	stub.Subscribes = append(stub.Subscribes, struct {
		Event   string `yaml:"event"`
		Service string `yaml:"service"`
		Port    int    `yaml:"port"`
		Webhook string `yaml:"webhook"`
	}{Event: "lead.created.v1", Service: "stub", Port: 8000, Webhook: "/hooks/lead"})
	stub.Provides = append(stub.Provides, struct {
		Capability string `yaml:"capability"`
		Schema     string `yaml:"schema"`
		Service    string `yaml:"service"`
		Port       int    `yaml:"port"`
	}{Capability: "contacts", Service: "stub", Port: 8000})

	on := true
	desired := reconcile.Desired{
		Company:   &config.Company{Name: "T", Modules: map[string]config.ModuleEntry{"stub": {Enabled: &on, Settings: map[string]any{"mode": "test"}}}},
		Manifests: map[string]*config.Module{"stub": stub},
	}

	// 1. enable
	if _, err := rec.Apply(ctx, desired, reconcile.Options{}); err != nil {
		t.Fatal(err)
	}
	injected := installer.Enabled["stub"]
	if injected.Settings["mode"] != "test" || injected.HMACSecret == "" || injected.Namespace != "mod-stub" {
		t.Fatalf("injected config: %+v", injected)
	}
	// the cluster would now project a token for mod-stub/stub
	reviewer.Add("projected-token", injected.Namespace, injected.ServiceAccount)

	call := func(method, path string, body string) (int, string) {
		req, _ := http.NewRequest(method, api.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer projected-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 2. call the module API with the projected token
	if code, body := call("GET", "/v1/self", ""); code != 200 || !strings.Contains(body, `"id":"stub"`) {
		t.Fatalf("self: %d %s", code, body)
	}
	if code, body := call("GET", "/v1/capabilities/email.send", ""); code != 200 || !strings.Contains(body, "can-core") {
		t.Fatalf("capability: %d %s", code, body)
	}

	// 3. publish an event; the stub's own webhook receives it signed with the injected secret
	var verified bool
	var gotType string
	recv := &recvDoer{fn: func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		verified = webhook.Verify(injected.HMACSecret, r.Header, b, now, webhook.DefaultReplayWindow) == nil
		gotType = r.Header.Get(webhook.HeaderEventType)
	}}
	if code, body := call("POST", "/v1/events", `{"type":"lead.created.v1","data":{"email":"a@example.com"}}`); code != 202 {
		t.Fatalf("publish: %d %s", code, body)
	}
	disp := webhook.NewDispatcher(db, recv)
	disp.Now = func() time.Time { return now }
	if n, err := disp.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("dispatch n=%d err=%v", n, err)
	}
	if !verified || gotType != "lead.created.v1" {
		t.Fatalf("webhook verified=%v type=%q", verified, gotType)
	}

	// 4. disable: the module stops running, its token is refused, nothing is delivered to it
	off := false
	desired.Company.Modules["stub"] = config.ModuleEntry{Enabled: &off}
	delete(desired.Manifests, "stub")
	if _, err := rec.Apply(ctx, desired, reconcile.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, running := installer.Enabled["stub"]; running {
		t.Fatal("module still running")
	}
	if code, _ := call("GET", "/v1/self", ""); code != http.StatusForbidden {
		t.Fatalf("disabled module token: %d", code)
	}
	if _, err := bus.Publish(ctx, "can-core", "lead.created.v1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if due, _ := db.DueDeliveries(ctx, now, 10); len(due) != 0 {
		t.Fatalf("event queued for a disabled module: %+v", due)
	}

	// 5. everything above is in a verifiable audit trail
	res, err := al.Verify(ctx, audit.VerifyOptions{})
	if err != nil || !res.OK() || res.Entries != 2 {
		t.Fatalf("audit: %+v %v", res, err)
	}
}

type recvDoer struct{ fn func(*http.Request) }

func (r *recvDoer) Do(req *http.Request) (*http.Response, error) {
	r.fn(req)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
}

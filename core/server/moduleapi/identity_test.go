package moduleapi

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
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/identity"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

// The reference the people module puts in person.offboarded.v1 is the one can-core checks.
func TestSubjectRefAgreesWithPeople(t *testing.T) {
	for _, s := range []string{"user:sso-abc", "sso-x", ""} {
		if people.SubjectRef(s) != identity.SubjectRef(s) {
			t.Fatalf("SubjectRef(%q) differs between people and can-core", s)
		}
	}
}

func TestRevokeIdentityOverHTTP(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	db.Now = func() time.Time { return now }
	tr := tokenreview.NewFake()
	reg := registry.New(db, "http://can-core")
	bus := events.NewBus(db)
	idSvc := &identity.Service{DB: db, Audit: audit.New(db), Now: func() time.Time { return now }}
	bus.Subscribe("person.offboarded.v1", idSvc.HandleOffboarded)
	srv := httptest.NewServer((&Server{Auth: modauth.New(tr, db), Registry: reg, Bus: bus, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Identity: idSvc}).Handler())
	t.Cleanup(srv.Close)

	prov := config.Module{ID: "provisioning", Name: "p", Version: "1", Publishes: []struct {
		Event  string `yaml:"event"`
		Schema string `yaml:"schema"`
	}{{Event: "person.offboarded.v1"}}}
	prov.Provides = append(prov.Provides, struct {
		Capability string `yaml:"capability"`
		Schema     string `yaml:"schema"`
		Service    string `yaml:"service"`
		Port       int    `yaml:"port"`
		Status     string `yaml:"status"`
	}{Capability: "identity.provision", Service: "p", Port: 8080})
	ppl := config.Module{ID: "people", Name: "people", Version: "1"}
	for _, m := range []config.Module{prov, ppl} {
		if err := reg.Register(ctx, store.Module{ID: m.ID, Version: m.Version, Namespace: "mod-" + m.ID, ServiceAccount: m.ID, State: store.StateEnabled, HMACSecret: "s", Manifest: m}); err != nil {
			t.Fatal(err)
		}
		tr.Add("tok-"+m.ID, "mod-"+m.ID, m.ID)
	}
	const party, subj = "pers_0123456789abcdef01234567", "user:sso-abc"
	call := func(tok string, body any) (int, string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/identity/revoke", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	req := map[string]string{"party_id": party, "subject": subj}
	if c, _ := call("nope", req); c != 401 {
		t.Fatalf("unauthenticated = %d", c)
	}
	if c, _ := call("tok-provisioning", req); c != 409 {
		t.Fatalf("before the offboarding = %d, want 409", c)
	}
	// A module cannot make the offboarding true by publishing the event itself (source is the module).
	if _, err := bus.Publish(ctx, "provisioning", "person.offboarded.v1", json.RawMessage(`{"party_id":"`+party+`","kind":"employee","effective_date":"2026-06-01","subject_ref":"`+identity.SubjectRef(subj)+`"}`)); err != nil {
		t.Fatal(err)
	}
	if c, _ := call("tok-provisioning", req); c != 409 {
		t.Fatalf("forged offboarding = %d, want 409", c)
	}
	if _, err := bus.Publish(ctx, "people", "person.offboarded.v1", json.RawMessage(`{"party_id":"`+party+`","kind":"employee","effective_date":"2026-06-01","subject_ref":"`+identity.SubjectRef(subj)+`"}`)); err != nil {
		t.Fatal(err)
	}
	if c, _ := call("tok-people", req); c != 403 {
		t.Fatalf("a module without the capability = %d, want 403", c)
	}
	if c, _ := call("tok-provisioning", map[string]string{"party_id": party, "subject": "user:sso-other"}); c != 403 {
		t.Fatalf("another account = %d, want 403", c)
	}
	if c, body := call("tok-provisioning", req); c != 200 || !strings.Contains(body, `"tokens":0`) {
		t.Fatalf("revoke = %d %s", c, body)
	}
	if c, _ := call("tok-provisioning", map[string]any{"party_id": party, "subject": subj, "extra": 1}); c != 400 {
		t.Fatalf("unknown field = %d", c)
	}
}

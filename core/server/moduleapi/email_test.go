package moduleapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func emailHarness(t *testing.T, company string) *harness {
	t.Helper()
	h := newHarness(t)
	f := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(f, []byte(company), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc := email.New(h.db, audit.New(h.db), email.Source{CompanyFile: f}, nil, log)
	s := &Server{Auth: modauth.New(h.tr, h.db), Registry: h.reg, Bus: events.NewBus(h.db), Log: log, Email: svc}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	// A second module that may send, to prove messages are private to their sender.
	other := manifest("news")
	other.Requires.Capabilities = []string{"email.send"}
	if err := h.reg.Register(context.Background(), store.Module{ID: "news", Version: "1", Namespace: "mod-news", ServiceAccount: "news", State: store.StateEnabled, HMACSecret: "s", Manifest: other}); err != nil {
		t.Fatal(err)
	}
	h.tr.Add("tok-news", "mod-news", "news")
	return h
}

const emailCompany = `schema: 1
name: Acme
domain: acme.example
provider: {type: local}
email:
  relay: {host: smtp.relay.example}
  modules:
    crm: {per_hour: 3}
`

func TestEmailSendAPI(t *testing.T) {
	h := emailHarness(t, emailCompany)
	body := map[string]any{"to": []string{"sales@acme.example"}, "subject": "Hello", "text": "Body", "source_id": "lead:1"}

	if r, _ := h.call("POST", "/v1/email", "", body); r.StatusCode != 401 {
		t.Fatal("no token:", r.StatusCode)
	}
	if r, _ := h.call("POST", "/v1/email", "tok-support", body); r.StatusCode != 403 {
		t.Fatal("a module that does not declare email.send:", r.StatusCode)
	}
	r, b := h.call("POST", "/v1/email", "tok-crm", body)
	var out struct{ IDs []string }
	if r.StatusCode != 202 || json.Unmarshal(b, &out) != nil || len(out.IDs) != 1 {
		t.Fatal(r.StatusCode, string(b))
	}
	if r, _ := h.call("POST", "/v1/email", "tok-crm", body); r.StatusCode != 200 {
		t.Fatal("retry with the same source_id:", r.StatusCode)
	}
	for name, tc := range map[string]struct {
		patch map[string]any
		code  int
	}{
		"outside the allow-list": {map[string]any{"to": []string{"victim@evil.example"}}, 403},
		"header injection":       {map[string]any{"subject": "x\r\nBcc: v@evil.example"}, 400},
		"sender is not settable": {map[string]any{"from": "ceo@acme.example"}, 400},
		"conflicting source_id":  {map[string]any{"text": "other"}, 409},
	} {
		req := map[string]any{}
		for k, v := range body {
			req[k] = v
		}
		for k, v := range tc.patch {
			req[k] = v
		}
		if r, b := h.call("POST", "/v1/email", "tok-crm", req); r.StatusCode != tc.code {
			t.Errorf("%s: %d %s", name, r.StatusCode, b)
		} else if strings.Contains(string(b), "evil.example") {
			t.Errorf("%s: the error echoes the address: %s", name, b)
		}
	}

	r, b = h.call("GET", "/v1/email/"+out.IDs[0], "tok-crm", nil)
	if r.StatusCode != 200 || !strings.Contains(string(b), `"status":"pending"`) || strings.Contains(string(b), "acme.example") {
		t.Fatal(r.StatusCode, string(b))
	}
	r1, b1 := h.call("GET", "/v1/email/"+out.IDs[0], "tok-news", nil)
	r2, b2 := h.call("GET", "/v1/email/eml_missing", "tok-news", nil)
	if r1.StatusCode != 404 || r2.StatusCode != 404 || string(b1) != string(b2) {
		t.Fatal("another module's message must look like a missing one:", r1.StatusCode, string(b1), string(b2))
	}

	for i, want := range []int{202, 202, 429} {
		req := map[string]any{"to": []string{"sales@acme.example"}, "subject": "Hello", "text": "Body"}
		r, b := h.call("POST", "/v1/email", "tok-crm", req)
		if r.StatusCode != want || (want == 429 && r.Header.Get("Retry-After") == "") {
			t.Fatal(i, r.StatusCode, string(b))
		}
	}
}

func TestEmailSendWithoutARelayIs503(t *testing.T) {
	h := emailHarness(t, "schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\n")
	r, b := h.call("POST", "/v1/email", "tok-crm", map[string]any{"to": []string{"sales@acme.example"}, "subject": "Hi", "text": "x"})
	if r.StatusCode != 503 {
		t.Fatal(r.StatusCode, string(b))
	}
	plain := newHarness(t) // no email service at all
	if r, _ := plain.call("POST", "/v1/email", "tok-crm", map[string]any{"to": []string{"a@b.example"}, "subject": "s", "text": "t"}); r.StatusCode != 503 {
		t.Fatal(r.StatusCode)
	}
}

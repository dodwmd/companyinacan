package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

const apiToken = "static-admin-token-0123456789"

type fakeIntro struct {
	ids map[string]modclient.Identity
	err error
}

func (f *fakeIntro) Introspect(_ context.Context, tok string) (modclient.Identity, error) {
	if f.err != nil {
		return modclient.Identity{}, f.err
	}
	return f.ids[tok], nil
}

type apiEnv struct {
	*harness
	srv   *httptest.Server
	intro *fakeIntro
}

const (
	patRead  = "canpat_readerreaderreaderreader"
	patWrite = "canpat_writerwriterwriterwriter"
	patAdmin = "canpat_adminadminadminadminad"
	patOther = "canpat_otherotherotherotherothe"
)

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	h := newHarness(t, nil)
	in := &fakeIntro{ids: map[string]modclient.Identity{
		patRead:  {Active: true, Kind: "user", Subject: "user:reader", Permissions: []string{"provision.read"}},
		patWrite: {Active: true, Kind: "user", Subject: "user:writer", Permissions: []string{"provision.write"}},
		patAdmin: {Active: true, Kind: "user", Subject: "user:boss", Permissions: []string{"provision.admin"}},
		patOther: {Active: true, Kind: "user", Subject: "user:other", Permissions: []string{"people.read"}},
	}}
	api := &API{Eng: h.eng, Auth: MultiAuth{StaticToken{Token: apiToken}, &CanAuth{In: &modclient.Introspector{API: in}}},
		SCIM: &SCIM{Store: h.st, Dir: h.dir, Now: h.clk.now},
		Can:  &modclient.Receiver{Secret: "webhook-secret-0123456789", Handle: h.eng.HandleEvent, Now: h.clk.now},
		ConnInfo: func() []ConnectorInfo {
			return []ConnectorInfo{{Name: "app-a", Type: "scim-client"}}
		}}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &apiEnv{harness: h, srv: srv, intro: in}
}

func (e *apiEnv) do(method, path, token string, body any, hdr ...string) (*http.Response, map[string]any, string) {
	e.t.Helper()
	var rd io.Reader
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp, m, string(raw)
}

func TestAPIAuthenticationAndPermissions(t *testing.T) {
	e := newAPIEnv(t)
	for _, tok := range []string{"", "nope", patOther + "x", "canpat_" + strings.Repeat("z", 20)} {
		if resp, _, _ := e.do("GET", "/v1/provision/runs", tok, nil); resp.StatusCode != 401 {
			t.Errorf("token %q: %d", tok, resp.StatusCode)
		}
	}
	if resp, _, _ := e.do("GET", "/v1/provision/runs", patOther, nil); resp.StatusCode != 401 {
		t.Error("a token with no provision permission is not a provisioning principal")
	}
	body := map[string]any{"kind": "mover", "party_id": alice}
	cases := []struct {
		name, method, path, tok string
		want                    int
	}{
		{"read lists", "GET", "/v1/provision/runs", patRead, 200},
		{"read cannot start", "POST", "/v1/provision/runs", patRead, 403},
		{"write can read", "GET", "/v1/provision/runs", patWrite, 200},
		{"write cannot cancel", "POST", "/v1/provision/runs/run_000000000000000000000000/cancel", patWrite, 403},
		{"write cannot flag urgent", "POST", "/v1/provision/people/" + alice + "/urgent", patWrite, 403},
		{"write cannot make tokens", "POST", "/v1/provision/scim-tokens", patWrite, 403},
		{"write cannot read the audit", "GET", "/v1/provision/audit", patWrite, 403},
		{"read cannot read the audit", "GET", "/v1/provision/audit", patRead, 403},
		{"admin reads the audit", "GET", "/v1/provision/audit", patAdmin, 200},
		{"static token", "GET", "/v1/provision/connectors", apiToken, 200},
		{"unauthenticated health", "GET", "/healthz", "", 200},
	}
	for _, c := range cases {
		var b any
		if c.method == "POST" {
			b = body
		}
		if resp, _, raw := e.do(c.method, c.path, c.tok, b, "Idempotency-Key", "k"); resp.StatusCode != c.want {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, raw)
		}
	}
	// an introspection outage never lets a token through
	e.intro.err = modclient.ErrUnavailable
	e.intro.ids["canpat_freshfreshfreshfreshfresh"] = modclient.Identity{Active: true, Kind: "user", Subject: "user:fresh", Permissions: []string{"provision.admin"}}
	if resp, _, _ := e.do("GET", "/v1/provision/runs", "canpat_freshfreshfreshfreshfresh", nil); resp.StatusCode != 503 {
		t.Errorf("outage: %d", resp.StatusCode)
	}
	if resp, _, _ := e.do("GET", "/v1/provision/runs", apiToken, nil); resp.StatusCode != 200 {
		t.Errorf("the service token does not depend on introspection: %d", resp.StatusCode)
	}
}

func TestAPIStartRunIsIdempotentAndPlanHasNoSideEffects(t *testing.T) {
	e := newAPIEnv(t)
	e.dir.put(person(alice, "active", "alice@example.com"))
	e.linkAccounts(alice, "app-a")
	resp, plan, raw := e.do("POST", "/v1/provision/plans", patRead, map[string]any{"kind": "mover", "party_id": alice})
	if resp.StatusCode != 200 || len(plan["steps"].([]any)) == 0 {
		t.Fatalf("plan %d %s", resp.StatusCode, raw)
	}
	if len(e.runs()) != 0 {
		t.Fatal("a plan stored a run")
	}
	resp, _, _ = e.do("POST", "/v1/provision/runs", patWrite, map[string]any{"kind": "mover", "party_id": alice})
	if resp.StatusCode != 400 {
		t.Fatalf("missing Idempotency-Key: %d", resp.StatusCode)
	}
	resp, r1, raw := e.do("POST", "/v1/provision/runs", patWrite, map[string]any{"kind": "mover", "party_id": alice}, "Idempotency-Key", "m-1")
	if resp.StatusCode != 201 || r1["trigger"] != "api" || r1["initiator"] != "user:writer" {
		t.Fatalf("start %d %s", resp.StatusCode, raw)
	}
	resp, r2, _ := e.do("POST", "/v1/provision/runs", patWrite, map[string]any{"kind": "mover", "party_id": alice}, "Idempotency-Key", "m-1")
	if resp.StatusCode != 200 || r2["id"] != r1["id"] {
		t.Fatalf("repeat %d %v", resp.StatusCode, r2["id"])
	}
	for name, body := range map[string]any{
		"bad kind":       map[string]any{"kind": "nuke", "party_id": alice},
		"bad party":      map[string]any{"kind": "mover", "party_id": "x"},
		"bad date":       map[string]any{"kind": "leaver", "party_id": alice, "effective_date": "2026-13-45"},
		"unknown field":  map[string]any{"kind": "mover", "party_id": alice, "admin": true},
		"trailing":       `{"kind":"mover","party_id":"` + alice + `"} {}`,
		"not json":       `nope`,
		"person missing": map[string]any{"kind": "mover", "party_id": bob},
	} {
		resp, _, _ := e.do("POST", "/v1/provision/runs", patWrite, body, "Idempotency-Key", "k-"+name)
		if resp.StatusCode != 400 && resp.StatusCode != 404 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	// a leaver for an active person is a conflict
	if resp, _, _ := e.do("POST", "/v1/provision/runs", patWrite, map[string]any{"kind": "leaver", "party_id": alice}, "Idempotency-Key", "l-1"); resp.StatusCode != 409 {
		t.Errorf("leaver for an active person: %d", resp.StatusCode)
	}
	// content type is enforced
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/provision/runs", strings.NewReader(`{"kind":"mover","party_id":"`+alice+`"}`))
	req.Header.Set("Authorization", "Bearer "+patWrite)
	req.Header.Set("Idempotency-Key", "ct")
	req.Header.Set("Content-Type", "text/plain")
	r3, _ := http.DefaultClient.Do(req)
	_ = r3.Body.Close()
	if r3.StatusCode != 400 {
		t.Errorf("text/plain: %d", r3.StatusCode)
	}
	// oversized body
	if resp, _, _ := e.do("POST", "/v1/provision/runs", patWrite, `{"kind":"`+strings.Repeat("a", 70000)+`"}`, "Idempotency-Key", "big"); resp.StatusCode != 413 {
		t.Errorf("oversized: %d", resp.StatusCode)
	}
	if resp, _, _ := e.do("GET", "/v1/provision/runs/"+r1["id"].(string), patRead, nil); resp.StatusCode != 200 {
		t.Error("get run")
	}
	if resp, _, _ := e.do("GET", "/v1/provision/runs/notarun", patRead, nil); resp.StatusCode != 404 {
		t.Error("get bad run id")
	}
	_, l, _ := e.do("GET", "/v1/provision/runs?party_id="+alice+"&kind=mover&state=pending", patRead, nil)
	if len(l["items"].([]any)) != 1 {
		t.Errorf("list %v", l)
	}
	if resp, _, _ := e.do("GET", "/v1/provision/runs?party_id=bad", patRead, nil); resp.StatusCode != 400 {
		t.Error("bad filter")
	}
}

func TestAPIUrgentRetryCancelAndLink(t *testing.T) {
	e := newAPIEnv(t)
	p := person(alice, "offboarding", "alice@example.com")
	p.EndDate = "2026-06-30"
	e.dir.put(p)
	e.linkAccounts(alice, "app-a", "app-b")
	// a person cannot flag their own termination through their own token
	e.intro.ids["canpat_selfselfselfselfselfself"] = modclient.Identity{Active: true, Kind: "user", Subject: "user:sso-aaaa", Permissions: []string{"provision.admin"}}
	if resp, _, _ := e.do("POST", "/v1/provision/people/"+alice+"/urgent", "canpat_selfselfselfselfselfself", nil); resp.StatusCode != 403 {
		t.Errorf("self flag: %d", resp.StatusCode)
	}
	e.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-10"))
	if resp, _, _ := e.do("POST", "/v1/provision/people/"+alice+"/urgent", patAdmin, nil); resp.StatusCode != 202 {
		t.Errorf("flag: %d", resp.StatusCode)
	}
	e.settle()
	if e.a.count("disable_account") != 1 {
		t.Fatal("urgent flag did not release the leaver")
	}
	run := e.runs()[0]
	// the can-core kill switch cannot succeed until the person is former: it waits (it is not the
	// step's fault, so it is never dead-lettered and never uses up an attempt)
	for i := 0; i < 8; i++ {
		e.clk.add(5 * time.Minute)
		e.settle()
	}
	_, dl, _ := e.do("GET", "/v1/provision/dead-letters", patRead, nil)
	if len(dl["items"].([]any)) != 0 {
		t.Fatalf("dead letters %v", dl)
	}
	// the person becomes former: the waiting step completes by itself
	p.Status = "former"
	e.dir.put(p)
	e.clk.add(2 * time.Hour)
	e.settle()
	if got := e.runs()[0]; got.State != RunSucceeded || len(e.core.calls) != 1 {
		t.Fatalf("after the status change: %s, revocations %v", got.State, e.core.calls)
	}
	if resp, _, _ := e.do("POST", "/v1/provision/runs/"+run.ID+"/retry", patWrite, nil); resp.StatusCode != 409 {
		t.Errorf("retry of a finished run: %d", resp.StatusCode)
	}
	if resp, _, _ := e.do("POST", "/v1/provision/runs/"+run.ID+"/cancel", patAdmin, nil); resp.StatusCode != 409 {
		t.Errorf("cancel of a finished run: %d", resp.StatusCode)
	}
	// link an existing downstream account
	if resp, _, _ := e.do("PUT", "/v1/provision/people/"+alice+"/accounts/app-b", patAdmin, map[string]any{"external_id": "ext-77"}); resp.StatusCode != 204 {
		t.Errorf("link: %d", resp.StatusCode)
	}
	if ext, _, _ := e.st.accountFor(context.Background(), alice, "app-b"); ext != "ext-77" {
		t.Errorf("account %q", ext)
	}
	for name, c := range map[string]struct {
		path string
		body any
	}{
		"unknown connector": {"/v1/provision/people/" + alice + "/accounts/nope", map[string]any{"external_id": "x1"}},
		"hostile id":        {"/v1/provision/people/" + alice + "/accounts/app-b", map[string]any{"external_id": "../../x"}},
		"unknown person":    {"/v1/provision/people/" + bob + "/accounts/app-b", map[string]any{"external_id": "x1"}},
	} {
		if resp, _, _ := e.do("PUT", c.path, patAdmin, c.body); resp.StatusCode < 400 {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAPISCIMTokens(t *testing.T) {
	e := newAPIEnv(t)
	resp, m, raw := e.do("POST", "/v1/provision/scim-tokens", patAdmin, map[string]any{"name": "okta", "scopes": []string{"scim.read", "scim.write"}, "expires_in": "720h"})
	if resp.StatusCode != 201 || !strings.HasPrefix(m["token"].(string), "scim_") {
		t.Fatalf("create %d %s", resp.StatusCode, raw)
	}
	tok := m["token"].(string)
	// the token works against the SCIM server mounted on the same service
	e.dir.put(person(alice, "active", "alice@example.com"))
	if resp, _, _ := e.do("GET", "/scim/v2/Users/"+alice, tok, nil); resp.StatusCode != 200 {
		t.Fatalf("scim with the new token: %d", resp.StatusCode)
	}
	// a provisioning PAT is not a SCIM token and the SCIM token is not an API token
	if resp, _, _ := e.do("GET", "/scim/v2/Users", patAdmin, nil); resp.StatusCode != 401 {
		t.Error("an API token worked on the SCIM server")
	}
	if resp, _, _ := e.do("GET", "/v1/provision/runs", tok, nil); resp.StatusCode != 401 {
		t.Error("a SCIM token worked on the provisioning API")
	}
	_, l, raw := e.do("GET", "/v1/provision/scim-tokens", patAdmin, nil)
	if strings.Contains(raw, tok) || strings.Contains(raw, hashToken(tok)) || len(l["items"].([]any)) != 1 {
		t.Fatalf("listing %s", raw)
	}
	for name, body := range map[string]any{
		"no scopes":    map[string]any{"name": "x", "scopes": []string{}, "expires_in": "24h"},
		"never":        map[string]any{"name": "x", "scopes": []string{"scim.read"}, "expires_in": "99999h"},
		"bad duration": map[string]any{"name": "x", "scopes": []string{"scim.read"}, "expires_in": "soon"},
		"no name":      map[string]any{"name": "", "scopes": []string{"scim.read"}, "expires_in": "24h"},
	} {
		if resp, _, _ := e.do("POST", "/v1/provision/scim-tokens", patAdmin, body); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	id := m["id"].(string)
	if resp, _, _ := e.do("DELETE", "/v1/provision/scim-tokens/"+id, patAdmin, nil); resp.StatusCode != 204 {
		t.Errorf("revoke: %d", resp.StatusCode)
	}
	if resp, _, _ := e.do("GET", "/scim/v2/Users/"+alice, tok, nil); resp.StatusCode != 401 {
		t.Error("a revoked token still works")
	}
	if resp, _, _ := e.do("DELETE", "/v1/provision/scim-tokens/scimtok_nope", patAdmin, nil); resp.StatusCode != 404 {
		t.Error("revoking an unknown token")
	}
	for i := 0; i < maxSCIMTokens+1; i++ {
		resp, _, _ := e.do("POST", "/v1/provision/scim-tokens", patAdmin, map[string]any{"name": "n", "scopes": []string{"scim.read"}, "expires_in": "24h"})
		if i >= maxSCIMTokens && resp.StatusCode != 409 {
			t.Errorf("token cap not enforced: %d", resp.StatusCode)
		}
	}
	// the audit trail shows the actor and ids, never the token
	_, a, raw := e.do("GET", "/v1/provision/audit?limit=500", patAdmin, nil)
	if strings.Contains(raw, tok) || len(a["items"].([]any)) == 0 {
		t.Fatalf("audit %s", raw)
	}
}

func TestAPIReceivesSignedCanEvents(t *testing.T) {
	e := newAPIEnv(t)
	e.dir.put(person(alice, "active", "alice@example.com"))
	data, _ := json.Marshal(map[string]any{"id": "evt_0000000000000000000000ff", "type": EventPersonCreated, "time": e.clk.now(), "source": "people", "data": created(alice, "active")})
	post := func(sig string, ts time.Time) int {
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/can/events", bytes.NewReader(data))
		req.Header.Set("X-Can-Timestamp", itoa(ts.Unix()))
		req.Header.Set("X-Can-Signature", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("v1=00", e.clk.now()); c != 401 {
		t.Fatalf("bad signature: %d", c)
	}
	if len(e.runs()) != 0 {
		t.Fatal("an unsigned event started a run")
	}
	if c := post(modclient.Sign("webhook-secret-0123456789", e.clk.now().Add(-time.Hour), data), e.clk.now().Add(-time.Hour)); c != 401 {
		t.Fatalf("replayed: %d", c)
	}
	if c := post(modclient.Sign("webhook-secret-0123456789", e.clk.now(), data), e.clk.now()); c != 204 {
		t.Fatalf("signed: %d", c)
	}
	if len(e.runs()) != 1 {
		t.Fatal("the signed event did not start a run")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAPIConnectorsListHidesCredentials(t *testing.T) {
	e := newAPIEnv(t)
	_, _, raw := e.do("GET", "/v1/provision/connectors", patRead, nil)
	if !strings.Contains(raw, `"app-a"`) || strings.Contains(strings.ToLower(raw), "token") || strings.Contains(raw, "http") {
		t.Fatalf("connectors %s", raw)
	}
}

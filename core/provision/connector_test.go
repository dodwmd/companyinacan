package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSCIMServer is a tiny downstream SCIM 2.0 server: just enough of RFC 7644 for the client.
type fakeSCIMServer struct {
	mu      sync.Mutex
	users   map[string]map[string]any // id -> resource
	groups  map[string]map[string]bool
	names   map[string]string // group id -> displayName
	log     []string
	auth    []string
	cts     []string
	status  int // when non-zero every request answers this
	big     bool
	nextID  int
	conflic bool
}

func newFakeSCIMServer() *fakeSCIMServer {
	return &fakeSCIMServer{users: map[string]map[string]any{}, groups: map[string]map[string]bool{"g1": {}}, names: map[string]string{"g1": "Staff"}}
}

func (f *fakeSCIMServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, r.Method+" "+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.cts = append(f.cts, r.Header.Get("Content-Type"))
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`secret-token leaked in an error page`))
		return
	}
	if f.big {
		_, _ = w.Write([]byte(`{"Resources":[{"id":"` + strings.Repeat("a", 5<<20) + `"}]}`))
		return
	}
	w.Header().Set("Content-Type", scimContentType)
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == "GET" && r.URL.Path == "/scim/v2/Users":
		var res []map[string]any
		for id, u := range f.users {
			// userName is not case sensitive in SCIM (RFC 7643).
			if strings.Contains(strings.ToLower(r.URL.Query().Get("filter")), `"`+strings.ToLower(fmt.Sprint(u["userName"]))+`"`) {
				res = append(res, map[string]any{"id": id, "userName": u["userName"], "externalId": u["externalId"]})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"totalResults": len(res), "Resources": res})
	case r.Method == "POST" && r.URL.Path == "/scim/v2/Users":
		if f.conflic {
			w.WriteHeader(409)
			return
		}
		var u map[string]any
		_ = json.Unmarshal(body, &u)
		f.nextID++
		id := fmt.Sprintf("u%d", f.nextID)
		f.users[id] = u
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/scim/v2/Users/"):
		id := strings.TrimPrefix(r.URL.Path, "/scim/v2/Users/")
		u, ok := f.users[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		var p struct{ Operations []map[string]any }
		_ = json.Unmarshal(body, &p)
		for _, op := range p.Operations {
			if op["path"] == "active" {
				u["active"] = op["value"]
			}
		}
		_, _ = w.Write([]byte(`{}`))
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/scim/v2/Users/"):
		id := strings.TrimPrefix(r.URL.Path, "/scim/v2/Users/")
		if _, ok := f.users[id]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(f.users, id)
		w.WriteHeader(204)
	case r.Method == "GET" && r.URL.Path == "/scim/v2/Groups":
		var res []map[string]any
		for id, n := range f.names {
			if strings.Contains(r.URL.Query().Get("filter"), `"`+n+`"`) {
				res = append(res, map[string]any{"id": id})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"totalResults": len(res), "Resources": res})
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/scim/v2/Groups/"):
		id := strings.TrimPrefix(r.URL.Path, "/scim/v2/Groups/")
		var p struct{ Operations []map[string]any }
		_ = json.Unmarshal(body, &p)
		for _, op := range p.Operations {
			switch op["op"] {
			case "add":
				for _, m := range op["value"].([]any) {
					f.groups[id][m.(map[string]any)["value"].(string)] = true
				}
			case "remove":
				path := op["path"].(string)
				for m := range f.groups[id] {
					if strings.Contains(path, `"`+m+`"`) {
						delete(f.groups[id], m)
					}
				}
			}
		}
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(404)
	}
}

func newSCIMClient(t *testing.T, f *fakeSCIMServer) (*SCIMClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &SCIMClient{ConnName: "idp", Base: srv.URL + "/scim/v2", Token: "downstream-secret-token", HTTP: NewOutboundClient(true, 5*time.Second)}, srv
}

func ident(email string) Identity {
	return Identity{PartyID: alice, UserName: email, GivenName: "Alice", FamilyName: "Ng", DisplayName: "Alice Ng", Active: true}
}

func TestSCIMClientLifecycleAgainstAFakeServer(t *testing.T) {
	f := newFakeSCIMServer()
	c, _ := newSCIMClient(t, f)
	ctx := context.Background()
	ext, err := c.CreateAccount(ctx, "k1", ident("alice@example.com"))
	if err != nil || ext == "" {
		t.Fatalf("create: %q %v", ext, err)
	}
	// a retry adopts the account instead of duplicating it
	again, err := c.CreateAccount(ctx, "k1", ident("alice@example.com"))
	if err != nil || again != ext || len(f.users) != 1 {
		t.Fatalf("retry: %q %v users=%d", again, err, len(f.users))
	}
	// the answer to a create was lost and the server now says 409: the client finds the account
	f.conflic = true
	f.users["uX"] = map[string]any{"userName": "bob@example.com"}
	if got, err := c.CreateAccount(ctx, "k2", ident("bob@example.com")); err != nil || got != "uX" {
		t.Fatalf("conflict: %q %v", got, err)
	}
	f.conflic = false
	if err := c.UpdateAccount(ctx, "k3", ext, ident("alice@example.com")); err != nil {
		t.Fatal(err)
	}
	if err := c.AddToGroup(ctx, "k4", ext, "Staff"); err != nil || !f.groups["g1"][ext] {
		t.Fatalf("add: %v %v", err, f.groups)
	}
	if err := c.RemoveFromGroup(ctx, "k5", ext, "Staff"); err != nil || f.groups["g1"][ext] {
		t.Fatalf("remove: %v %v", err, f.groups)
	}
	if err := c.RemoveFromGroup(ctx, "k5", ext, "Missing group"); err != nil {
		t.Fatalf("removing from a group that is gone must succeed: %v", err)
	}
	if err := c.AddToGroup(ctx, "k6", ext, "Missing group"); err == nil || !isPerm(err, "group_not_found") {
		t.Fatalf("a missing group is created or ignored: %v", err)
	}
	if err := c.DisableAccount(ctx, "k7", ext); err != nil || f.users[ext]["active"] != false {
		t.Fatalf("disable: %v %v", err, f.users[ext])
	}
	if err := c.DisableAccount(ctx, "k7", "u404"); err != nil {
		t.Fatalf("disabling a vanished account must succeed: %v", err)
	}
	if err := c.DeleteAccount(ctx, "k8", ext); err != nil || len(f.users) != 1 {
		t.Fatalf("delete: %v", err)
	}
	if err := c.DeleteAccount(ctx, "k8", ext); err != nil {
		t.Fatalf("deleting twice must succeed: %v", err)
	}
	for i, a := range f.auth {
		if a != "Bearer downstream-secret-token" || (f.cts[i] != "" && f.cts[i] != scimContentType) {
			t.Fatalf("request %d: auth %q content-type %q", i, a, f.cts[i])
		}
	}
	if _, err := c.CreateAccount(ctx, "k9", ident("")); !isPerm(err, "no_login") {
		t.Fatalf("an account without a login: %v", err)
	}
}

func isPerm(err error, code string) bool {
	ce, ok := err.(*ConnError)
	return ok && ce.Permanent && ce.Code == code
}

func TestSCIMClientClassifiesFailuresWithoutLeakingBodies(t *testing.T) {
	f := newFakeSCIMServer()
	c, _ := newSCIMClient(t, f)
	for status, wantPerm := range map[int]bool{500: false, 502: false, 429: false, 408: false, 400: true, 401: true, 403: true, 422: true} {
		f.status = status
		_, err := c.CreateAccount(context.Background(), "k", ident("a@example.com"))
		ce, ok := err.(*ConnError)
		if !ok || ce.Permanent != wantPerm {
			t.Errorf("status %d: %v", status, err)
		}
		if err != nil && (strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "leaked")) {
			t.Errorf("status %d: error text leaks the response: %v", status, err)
		}
	}
}

func TestSCIMClientBoundsResponsesAndRefusesHostileValues(t *testing.T) {
	f := newFakeSCIMServer()
	c, _ := newSCIMClient(t, f)
	f.big = true
	if _, err := c.CreateAccount(context.Background(), "k", ident("a@example.com")); err == nil {
		t.Fatal("an oversized answer was accepted")
	}
	f.big = false
	// an external id from a hostile server must not be able to steer later requests
	f.users["../../admin"] = map[string]any{"userName": "evil@example.com"}
	if _, err := c.CreateAccount(context.Background(), "k", ident("evil@example.com")); !isPerm(err, "bad_response") {
		t.Fatalf("hostile id: %v", err)
	}
	if err := c.DisableAccount(context.Background(), "k", "../../etc/passwd"); !isPerm(err, "bad_account") {
		t.Fatalf("hostile id: %v", err)
	}
}

func TestSCIMClientFilterValuesAreQuoted(t *testing.T) {
	if got := scimQuote(`a"b\c`); got != `"a\"b\\c"` {
		t.Fatalf("quote %s", got)
	}
}

func TestOutboundClientRefusesPrivateDestinationsAndRedirects(t *testing.T) {
	hit := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- r.URL.Path; w.WriteHeader(200) }))
	defer srv.Close()
	// default: loopback is not a public address, so the connection is never made
	cl := NewOutboundClient(false, 3*time.Second)
	_, _, err := doJSON(context.Background(), cl, "GET", srv.URL+"/x", "", nil, nil)
	if !isPerm(err, "destination_denied") {
		t.Fatalf("loopback was reachable: %v", err)
	}
	select {
	case p := <-hit:
		t.Fatalf("the server was contacted: %s", p)
	default:
	}
	// redirects are not followed, even with private targets allowed
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/secret", http.StatusFound)
	}))
	defer redir.Close()
	st, _, err := doJSON(context.Background(), NewOutboundClient(true, 3*time.Second), "GET", redir.URL, "", nil, nil)
	if err != nil || st != 302 {
		t.Fatalf("redirect: %d %v", st, err)
	}
	select {
	case p := <-hit:
		t.Fatalf("a redirect was followed to %s", p)
	default:
	}
}

func TestValidateTarget(t *testing.T) {
	bad := []struct {
		url             string
		private, plain  bool
		wantErrContains string
	}{
		{"http://app.example.com/scim", false, false, "plain http"},
		{"https://user:pw@app.example.com/scim", false, false, "credentials"},
		{"https://app.example.com/scim?token=1", false, false, "query"},
		{"https://app.example.com/scim#x", false, false, "fragment"},
		{"ftp://app.example.com", false, false, "http"},
		{"https://127.0.0.1/scim", false, false, "public"},
		{"https://10.1.2.3/scim", false, false, "public"},
		{"https://169.254.169.254/latest", false, false, "public"},
		{"https://[::1]/scim", false, false, "public"},
		{"https://[fd00::1]/scim", false, false, "public"},
		{"", false, false, "absolute"},
		{"/relative", false, false, "absolute"},
	}
	for _, c := range bad {
		_, err := validateTarget(c.url, c.private, c.plain)
		if err == nil || !strings.Contains(err.Error(), c.wantErrContains) {
			t.Errorf("%q: %v", c.url, err)
		}
	}
	for _, c := range []struct {
		url            string
		private, plain bool
	}{{"https://app.example.com/scim/v2", false, false}, {"https://8.8.8.8/x", false, false}, {"http://people-api.mod-people:8081", true, true}, {"https://10.0.0.1/x", true, false}} {
		if _, err := validateTarget(c.url, c.private, c.plain); err != nil {
			t.Errorf("%q: %v", c.url, err)
		}
	}
}

func TestWebhookConnectorSignsAndClassifies(t *testing.T) {
	var gotSig, gotTS, gotKey string
	var gotBody []byte
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig, gotTS, gotKey = r.Header.Get("X-Provision-Signature"), r.Header.Get("X-Provision-Timestamp"), r.Header.Get("Idempotency-Key")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		if status == 200 {
			_, _ = w.Write([]byte(`{"external_id":"acct-77"}`))
		}
	}))
	defer srv.Close()
	now := time.Unix(1_800_000_000, 0)
	c := &WebhookConnector{ConnName: "hooks", URL: srv.URL, Secret: "0123456789abcdef-secret", HTTP: NewOutboundClient(true, 3*time.Second), Now: func() time.Time { return now }}
	ext, err := c.CreateAccount(context.Background(), "run_x:1", ident("a@example.com"))
	if err != nil || ext != "acct-77" {
		t.Fatalf("%q %v", ext, err)
	}
	if gotSig != WebhookSignature(c.Secret, now.Unix(), gotBody) || gotTS != "1800000000" || gotKey != "run_x:1" {
		t.Fatalf("headers sig=%q ts=%q key=%q", gotSig, gotTS, gotKey)
	}
	var env map[string]any
	_ = json.Unmarshal(gotBody, &env)
	if env["operation"] != "create_account" || env["idempotency_key"] != "run_x:1" {
		t.Fatalf("body %s", gotBody)
	}
	if strings.Contains(string(gotBody), c.Secret) {
		t.Fatal("the secret is in the body")
	}
	if err := c.DisableAccount(context.Background(), "run_x:2", "acct-77"); err != nil {
		t.Fatal(err)
	}
	status = 503
	if err := c.DisableAccount(context.Background(), "run_x:2", "acct-77"); err == nil || isPerm(err, "http_503") {
		t.Fatalf("503 should be transient: %v", err)
	}
	status = 410
	if err := c.AddToGroup(context.Background(), "k", "acct-77", "g"); !isPerm(err, "http_410") {
		t.Fatalf("410: %v", err)
	}
}

func TestStubsAreHonestAndOffline(t *testing.T) {
	for _, kind := range StubKinds {
		s := &Stub{ConnName: kind, Kind: kind}
		if s.Verified() {
			t.Errorf("%s claims to be verified", kind)
		}
		ctx := context.Background()
		for name, err := range map[string]error{
			"disable": s.DisableAccount(ctx, "k", "x"), "delete": s.DeleteAccount(ctx, "k", "x"), "add": s.AddToGroup(ctx, "k", "x", "g"),
			"remove": s.RemoveFromGroup(ctx, "k", "x", "g"), "update": s.UpdateAccount(ctx, "k", "x", Identity{}),
		} {
			if !isPerm(err, "unimplemented") {
				t.Errorf("%s %s: %v", kind, name, err)
			}
		}
		if _, err := s.CreateAccount(ctx, "k", Identity{}); !isPerm(err, "unimplemented") {
			t.Errorf("%s create: %v", kind, err)
		}
	}
}

func TestConnectorSetRejectsDuplicates(t *testing.T) {
	if _, err := NewConnectorSet(newFakeConn("a"), newFakeConn("a")); err == nil {
		t.Fatal("duplicate names accepted")
	}
}

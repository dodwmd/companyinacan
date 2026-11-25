package provision

import (
	"bytes"
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

type scimEnv struct {
	t     *testing.T
	st    *Store
	dir   *fakeDir
	srv   *httptest.Server
	scim  *SCIM
	read  string
	write string
	clk   *clock
	mu    sync.Mutex
	moved []string
}

func newSCIMEnv(t *testing.T, cfg SCIMConfig) *scimEnv {
	t.Helper()
	if cfg.RequireIfMatch == nil {
		cfg.RequireIfMatch = boolPtr(false) // most tests write without a version; the default is tested below
	}
	e := &scimEnv{t: t, dir: newFakeDir(), clk: &clock{t: time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)}}
	st, err := OpenStore(":memory:", e.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	e.scim = &SCIM{Store: st, Dir: e.dir, Now: e.clk.now, Cfg: cfg, OnMembership: func(_ context.Context, ids []string) {
		e.mu.Lock()
		e.moved = append(e.moved, ids...)
		e.mu.Unlock()
	}}
	e.srv = httptest.NewServer(e.scim.Handler())
	t.Cleanup(e.srv.Close)
	_, e.read, err = st.CreateSCIMToken(context.Background(), "user:admin", "reader", []string{ScopeSCIMRead}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, e.write, err = st.CreateSCIMToken(context.Background(), "user:admin", "writer", []string{ScopeSCIMWrite}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.dir.put(Person{ID: alice, Kind: "employee", Status: "active", GivenName: "Alice", FamilyName: "Ng", WorkEmail: "alice@example.com", WorkPhone: "+61 2 5550 0001", ManagerID: bob, SSOSubject: "user:sso-alice"})
	e.dir.put(Person{ID: bob, Kind: "employee", Status: "leave", GivenName: "Bob", FamilyName: "Ito", Preferred: "Bobby", WorkEmail: "bob@example.com"})
	e.dir.put(Person{ID: carol, Kind: "contractor", Status: "candidate", GivenName: "Carol", FamilyName: "Oh", WorkEmail: "carol@example.com"})
	return e
}

func (e *scimEnv) do(method, path, token, ct string, body any, hdr ...string) (*http.Response, map[string]any, string) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		if ct == "" {
			ct = scimContentType
		}
		req.Header.Set("Content-Type", ct)
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

func patchBody(ops ...map[string]any) map[string]any {
	return map[string]any{"schemas": []string{schemaPatch}, "Operations": ops}
}

func (e *scimEnv) wantError(resp *http.Response, m map[string]any, status int, scimType string) {
	e.t.Helper()
	if resp.StatusCode != status {
		e.t.Fatalf("status %d, want %d: %v", resp.StatusCode, status, m)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), scimContentType) {
		e.t.Errorf("error Content-Type %q", resp.Header.Get("Content-Type"))
	}
	sch, _ := m["schemas"].([]any)
	if len(sch) != 1 || sch[0] != schemaError || m["status"] != fmt.Sprint(status) || m["detail"] == "" {
		e.t.Errorf("not an RFC 7644 error: %v", m)
	}
	if scimType != "" && m["scimType"] != scimType {
		e.t.Errorf("scimType %v, want %s", m["scimType"], scimType)
	}
}

func TestSCIMAuthentication(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	for name, tok := range map[string]string{"none": "", "garbage": "nope", "wrong prefix": "canpat_xxxxxxxxxxxxxxxxxxxxxxxxxxxx", "unknown": "scim_" + strings.Repeat("A", 43)} {
		resp, m, _ := e.do("GET", "/scim/v2/Users", tok, "", nil)
		e.wantError(resp, m, 401, "")
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate", name)
		}
	}
	// expired and revoked tokens stop working
	rec, tok, _ := e.st.CreateSCIMToken(context.Background(), "user:admin", "short", []string{ScopeSCIMRead}, time.Hour)
	if resp, _, _ := e.do("GET", "/scim/v2/Users", tok, "", nil); resp.StatusCode != 200 {
		t.Fatal("fresh token refused")
	}
	if err := e.st.RevokeSCIMToken(context.Background(), "user:admin", rec.ID); err != nil {
		t.Fatal(err)
	}
	if resp, _, _ := e.do("GET", "/scim/v2/Users", tok, "", nil); resp.StatusCode != 401 {
		t.Fatal("revoked token accepted")
	}
	_, tok2, _ := e.st.CreateSCIMToken(context.Background(), "user:admin", "short2", []string{ScopeSCIMRead}, time.Hour)
	e.clk.add(2 * time.Hour)
	if resp, _, _ := e.do("GET", "/scim/v2/Users", tok2, "", nil); resp.StatusCode != 401 {
		t.Fatal("expired token accepted")
	}
	// scopes: a read token cannot write; a write token also reads
	resp, m, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.read, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "X"}))
	e.wantError(resp, m, 403, "")
	// the tokens created in setup expired too: reissue
	_, rd, _ := e.st.CreateSCIMToken(context.Background(), "user:admin", "r", []string{ScopeSCIMRead}, time.Hour)
	_, wr, _ := e.st.CreateSCIMToken(context.Background(), "user:admin", "w", []string{ScopeSCIMWrite}, time.Hour)
	resp, m, _ = e.do("PATCH", "/scim/v2/Users/"+alice, rd, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "X"}))
	e.wantError(resp, m, 403, "")
	if resp, _, _ = e.do("GET", "/scim/v2/Users/"+alice, wr, "", nil); resp.StatusCode != 200 {
		t.Fatalf("write scope does not read: %d", resp.StatusCode)
	}
	for _, bad := range [][]string{{}, {"scim.admin"}, {"scim.read", "root"}} {
		if _, _, err := e.st.CreateSCIMToken(context.Background(), "u", "n", bad, time.Hour); err == nil {
			t.Errorf("scopes %v accepted", bad)
		}
	}
	if _, _, err := e.st.CreateSCIMToken(context.Background(), "u", "n", []string{ScopeSCIMRead}, 1000*24*time.Hour); err == nil {
		t.Error("a token that never expires was issued")
	}
}

func TestSCIMValidCredentialsAreNeverThrottled(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	start := time.Now()
	for i := 0; i < 400; i++ {
		if resp, _, _ := e.do("GET", "/scim/v2/ServiceProviderConfig", e.read, "", nil); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("valid requests were slowed: %v", time.Since(start))
	}
	// the token store only stores a hash
	var n int
	_ = e.st.db.QueryRow(`SELECT count(*) FROM scim_tokens WHERE hash=?`, e.read).Scan(&n)
	if n != 0 {
		t.Fatal("a token is stored in clear")
	}
}

func TestSCIMStrictContentNegotiation(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	body := patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "Al"})
	resp, m, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "application/json", body)
	e.wantError(resp, m, 415, "")
	resp, m, _ = e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "text/plain", body)
	e.wantError(resp, m, 415, "")
	resp, m, _ = e.do("GET", "/scim/v2/Users", e.read, "", nil, "Accept", "text/html")
	e.wantError(resp, m, 406, "")
	for _, accept := range []string{"application/scim+json", "application/json", "*/*", "text/html, application/scim+json;q=0.9"} {
		if resp, _, _ := e.do("GET", "/scim/v2/Users", e.read, "", nil, "Accept", accept); resp.StatusCode != 200 {
			t.Errorf("Accept %q refused", accept)
		}
	}
	resp, _, _ = e.do("GET", "/scim/v2/Users", e.read, "", nil)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), scimContentType) || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", resp.Header)
	}
	// bodies are bounded and must be JSON
	resp, m, _ = e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": strings.Repeat("x", 70000)}))
	e.wantError(resp, m, 413, "")
	req, _ := http.NewRequest("PATCH", e.srv.URL+"/scim/v2/Users/"+alice, strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+e.write)
	req.Header.Set("Content-Type", scimContentType)
	r2, _ := http.DefaultClient.Do(req)
	var m2 map[string]any
	_ = json.NewDecoder(r2.Body).Decode(&m2)
	_ = r2.Body.Close()
	e.wantError(r2, m2, 400, "invalidSyntax")
}

func TestSCIMDiscovery(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	_, m, _ := e.do("GET", "/scim/v2/ServiceProviderConfig", e.read, "", nil)
	if m["patch"].(map[string]any)["supported"] != true || m["etag"].(map[string]any)["supported"] != true || m["bulk"].(map[string]any)["supported"] != false || m["sort"].(map[string]any)["supported"] != false {
		t.Errorf("spc %v", m)
	}
	if schemas := m["schemas"].([]any); schemas[0] != schemaSPC {
		t.Errorf("schemas %v", schemas)
	}
	_, m, _ = e.do("GET", "/scim/v2/ResourceTypes", e.read, "", nil)
	if m["totalResults"] != float64(2) {
		t.Errorf("resource types %v", m)
	}
	if resp, _, _ := e.do("GET", "/scim/v2/ResourceTypes/User", e.read, "", nil); resp.StatusCode != 200 {
		t.Error("ResourceTypes/User")
	}
	_, m, _ = e.do("GET", "/scim/v2/Schemas", e.read, "", nil)
	if m["totalResults"] != float64(3) {
		t.Errorf("schemas %v", m)
	}
	if resp, _, _ := e.do("GET", "/scim/v2/Schemas/"+schemaUser, e.read, "", nil); resp.StatusCode != 200 {
		t.Error("Schemas/User")
	}
	resp, m, _ := e.do("GET", "/scim/v2/Schemas/nope", e.read, "", nil)
	e.wantError(resp, m, 404, "")
	resp, m, _ = e.do("GET", "/scim/v2/Nothing", e.read, "", nil)
	e.wantError(resp, m, 404, "")
}

func TestSCIMUsersNeverExposeSensitiveFieldsOrLeave(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	_, m, raw := e.do("GET", "/scim/v2/Users", e.read, "", nil)
	if m["totalResults"] != float64(2) { // the candidate is not listed
		t.Fatalf("total %v: %s", m["totalResults"], raw)
	}
	for _, bad := range []string{"leave", "status", "birth", "salary", "national", "sso_subject", "sso-alice", "legal_hold", "carol", "candidate", "address", "compensation"} {
		if strings.Contains(strings.ToLower(raw), bad) {
			t.Errorf("the listing contains %q: %s", bad, raw)
		}
	}
	for _, r := range m["Resources"].([]any) {
		u := r.(map[string]any)
		if u["id"] == bob && u["active"] != true {
			t.Error("a person on leave must be shown as active")
		}
		if u["id"] == bob && u["displayName"] != "Bobby" {
			t.Errorf("display name %v", u["displayName"])
		}
		for k := range u {
			switch k {
			case "schemas", "id", "userName", "displayName", "name", "userType", "active", "meta", "emails", "phoneNumbers", schemaEnterprise:
			default:
				t.Errorf("unexpected attribute %q", k)
			}
		}
	}
	resp, m, _ := e.do("GET", "/scim/v2/Users/"+carol, e.read, "", nil)
	e.wantError(resp, m, 404, "")
}

func TestSCIMListPaginationAndFilter(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("pers_%024x", 0x100+i)
		e.dir.put(Person{ID: id, Kind: "employee", Status: "active", GivenName: fmt.Sprintf("P%d", i), FamilyName: "Test", WorkEmail: fmt.Sprintf("p%d@example.com", i)})
	}
	_, m, _ := e.do("GET", "/scim/v2/Users?startIndex=3&count=4", e.read, "", nil)
	if m["totalResults"] != float64(9) || m["startIndex"] != float64(3) || m["itemsPerPage"] != float64(4) || len(m["Resources"].([]any)) != 4 {
		t.Fatalf("page %v", m)
	}
	_, m, _ = e.do("GET", "/scim/v2/Users?startIndex=9&count=50", e.read, "", nil)
	if len(m["Resources"].([]any)) != 1 {
		t.Fatalf("last page %v", m)
	}
	_, m, _ = e.do("GET", "/scim/v2/Users?count=0", e.read, "", nil)
	if m["totalResults"] != float64(9) || len(m["Resources"].([]any)) != 0 {
		t.Fatalf("count=0 %v", m)
	}
	_, m, _ = e.do("GET", "/scim/v2/Users?count=100000", e.read, "", nil)
	if len(m["Resources"].([]any)) != 9 {
		t.Fatalf("clamped count %v", m["itemsPerPage"])
	}
	q := func(f string) map[string]any {
		_, m, _ := e.do("GET", "/scim/v2/Users?filter="+urlQ(f), e.read, "", nil)
		return m
	}
	if m := q(`userName eq "ALICE@example.com"`); m["totalResults"] != float64(1) {
		t.Errorf("userName filter %v", m)
	}
	if m := q(`userName eq "p3@example.com" and active eq true`); m["totalResults"] != float64(1) {
		t.Errorf("and filter %v", m)
	}
	if m := q(`name.familyName eq "Test"`); m["totalResults"] != float64(7) {
		t.Errorf("familyName filter %v", m)
	}
	if m := q(`id eq "` + alice + `"`); m["totalResults"] != float64(1) {
		t.Errorf("id filter %v", m)
	}
	if m := q(`active eq false`); m["totalResults"] != float64(0) {
		t.Errorf("active filter %v", m)
	}
	for _, bad := range []string{`userName co "a"`, `userName eq "a" or userName eq "b"`, `salary eq "5"`, `userName eq`} {
		resp, m, _ := e.do("GET", "/scim/v2/Users?filter="+urlQ(bad), e.read, "", nil)
		e.wantError(resp, m, 400, "invalidFilter")
	}
	resp, m, _ := e.do("GET", "/scim/v2/Users?sortBy=userName", e.read, "", nil)
	e.wantError(resp, m, 400, "invalidValue")
	resp, m, _ = e.do("GET", "/scim/v2/Users?startIndex=x", e.read, "", nil)
	e.wantError(resp, m, 400, "invalidValue")
}

func urlQ(s string) string { return strings.NewReplacer(" ", "%20", `"`, "%22", "+", "%2B").Replace(s) }

func TestSCIMListIsBounded(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{MaxScan: 150})
	for i := 0; i < 400; i++ {
		e.dir.put(Person{ID: fmt.Sprintf("pers_%024x", 0x1000+i), Kind: "employee", Status: "active", GivenName: "X", FamilyName: "Y", WorkEmail: fmt.Sprintf("x%d@example.com", i)})
	}
	resp, m, _ := e.do("GET", "/scim/v2/Users", e.read, "", nil)
	e.wantError(resp, m, 400, "tooMany")
	// a direct id lookup is not a scan
	if resp, _, _ := e.do("GET", "/scim/v2/Users?filter="+urlQ(`id eq "`+alice+`"`), e.read, "", nil); resp.StatusCode != 200 {
		t.Fatalf("id lookup %d", resp.StatusCode)
	}
}

func TestSCIMDirectoryOutageIs503(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	e.dir.down = true
	resp, m, _ := e.do("GET", "/scim/v2/Users/"+alice, e.read, "", nil)
	e.wantError(resp, m, 503, "")
	resp, m, _ = e.do("GET", "/scim/v2/Users", e.read, "", nil)
	e.wantError(resp, m, 503, "")
}

func TestSCIMUserETagAndPatch(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	resp, u, _ := e.do("GET", "/scim/v2/Users/"+alice, e.read, "", nil)
	etag := resp.Header.Get("ETag")
	if etag == "" || etag != u["meta"].(map[string]any)["version"] {
		t.Fatalf("etag %q meta %v", etag, u["meta"])
	}
	// replace displayName with a stale If-Match: 412 and nothing changes
	resp, m, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "Al"}), "If-Match", `W/"stale"`)
	e.wantError(resp, m, 412, "")
	if len(e.dir.updates) != 0 {
		t.Fatal("a stale write changed the directory")
	}
	resp, u2, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "Al"}), "If-Match", etag)
	if resp.StatusCode != 200 || u2["displayName"] != "Al" || resp.Header.Get("ETag") == etag {
		t.Fatalf("patch %d %v etag %q", resp.StatusCode, u2["displayName"], resp.Header.Get("ETag"))
	}
	if p, _ := e.dir.Get(context.Background(), alice); p.Preferred != "Al" {
		t.Fatalf("directory %+v", p)
	}
	// the phone, in the three shapes identity providers use
	for _, op := range []map[string]any{
		{"op": "replace", "path": `phoneNumbers[type eq "work"].value`, "value": "+61 400 111 222"},
		{"op": "add", "path": "phoneNumbers", "value": []map[string]any{{"value": "+61 400 333 444", "type": "work"}}},
		{"op": "replace", "value": map[string]any{"displayName": "Alice N", "phoneNumbers": []map[string]any{{"value": "+61 400 555 666", "type": "work"}}}},
	} {
		if resp, _, raw := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(op)); resp.StatusCode != 200 {
			t.Errorf("%v: %d %s", op, resp.StatusCode, raw)
		}
	}
	if p, _ := e.dir.Get(context.Background(), alice); p.WorkPhone != "+61 400 555 666" || p.Preferred != "Alice N" {
		t.Fatalf("directory %+v", p)
	}
	if resp, _, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "remove", "path": "displayName"})); resp.StatusCode != 200 {
		t.Error("remove displayName")
	}
	// everything else is managed by the directory
	for _, op := range []map[string]any{
		{"op": "replace", "path": "active", "value": false},
		{"op": "replace", "path": "userName", "value": "evil@example.com"},
		{"op": "replace", "path": "name.givenName", "value": "Z"},
		{"op": "replace", "value": map[string]any{"active": false}},
		{"op": "replace", "path": "emails", "value": []any{}},
	} {
		resp, m, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(op))
		e.wantError(resp, m, 400, "mutability")
	}
	for name, op := range map[string]map[string]any{
		"unknown path": {"op": "replace", "path": "bogus", "value": "x"},
		"bad op":       {"op": "move", "path": "displayName", "value": "x"},
	} {
		if resp, _, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(op)); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	for name, v := range map[string]string{"control": "A\nB", "too long": strings.Repeat("x", 101), "zero width": "A\u200bB"} {
		if resp, _, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": v})); resp.StatusCode != 400 {
			t.Errorf("%s accepted", name)
		}
	}
	// PatchOp envelope is validated
	resp, m, _ = e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", map[string]any{"schemas": []string{"x"}, "Operations": []any{}})
	e.wantError(resp, m, 400, "invalidSyntax")
	ops := make([]map[string]any, 25)
	for i := range ops {
		ops[i] = map[string]any{"op": "replace", "path": "displayName", "value": "a"}
	}
	resp, m, _ = e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(ops...))
	e.wantError(resp, m, 400, "invalidValue")
	// users cannot be created, replaced or deleted through SCIM
	for _, c := range [][2]string{{"POST", "/scim/v2/Users"}, {"PUT", "/scim/v2/Users/" + alice}, {"DELETE", "/scim/v2/Users/" + alice}} {
		resp, m, _ := e.do(c[0], c[1], e.write, "", map[string]any{"userName": "x"})
		e.wantError(resp, m, 403, "mutability")
	}
	// the audit trail has ids only
	recs, _ := e.st.AuditList(context.Background(), 0, 100)
	n := 0
	for _, r := range recs {
		if r.Action == "scim.user.patch" {
			n++
			if r.Target != alice || strings.Contains(fmt.Sprint(r.Detail), "Alice") || !strings.HasPrefix(r.Actor, "scim:scimtok_") {
				t.Errorf("audit %+v", r)
			}
		}
	}
	if n == 0 {
		t.Error("user patches are not audited")
	}
}

func TestSCIMRequireIfMatch(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{RequireIfMatch: boolPtr(true)})
	resp, m, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "A"}))
	e.wantError(resp, m, 428, "")
}

func TestSCIMUnknownUsers(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	for _, id := range []string{"pers_ffffffffffffffffffffffff", "x", "../etc/passwd", strings.Repeat("a", 500)} {
		resp, m, _ := e.do("GET", "/scim/v2/Users/"+id, e.read, "", nil)
		if resp.StatusCode != 404 {
			t.Errorf("%q: %d %v", id, resp.StatusCode, m)
		}
	}
}

func TestSCIMGroups(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	resp, g, raw := e.do("POST", "/scim/v2/Groups", e.write, "", map[string]any{"schemas": []string{schemaGroup}, "displayName": "Engineering", "members": []map[string]any{{"value": alice}}})
	if resp.StatusCode != 201 || resp.Header.Get("Location") == "" || resp.Header.Get("ETag") == "" {
		t.Fatalf("create %d %s", resp.StatusCode, raw)
	}
	gid := g["id"].(string)
	if len(e.moved) != 1 || e.moved[0] != alice {
		t.Fatalf("membership callback %v", e.moved)
	}
	resp, m, _ := e.do("POST", "/scim/v2/Groups", e.write, "", map[string]any{"displayName": "engineering"})
	e.wantError(resp, m, 409, "uniqueness")
	for name, body := range map[string]map[string]any{
		"unknown member":  {"displayName": "G1", "members": []map[string]any{{"value": "pers_ffffffffffffffffffffffff"}}},
		"candidate":       {"displayName": "G2", "members": []map[string]any{{"value": carol}}},
		"bad member":      {"displayName": "G3", "members": []map[string]any{{"value": "x"}}},
		"empty name":      {"displayName": ""},
		"control in name": {"displayName": "a\nb"},
	} {
		if resp, _, _ := e.do("POST", "/scim/v2/Groups", e.write, "", body); resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	// PATCH add, remove with a filter path, replace
	etag := resp.Header.Get("ETag")
	_ = etag
	resp, _, _ = e.do("GET", "/scim/v2/Groups/"+gid, e.read, "", nil)
	etag = resp.Header.Get("ETag")
	resp, g2, raw := e.do("PATCH", "/scim/v2/Groups/"+gid, e.write, "", patchBody(map[string]any{"op": "add", "path": "members", "value": []map[string]any{{"value": bob}}}), "If-Match", etag)
	if resp.StatusCode != 200 || len(g2["members"].([]any)) != 2 {
		t.Fatalf("add %d %s", resp.StatusCode, raw)
	}
	resp, m, _ = e.do("PATCH", "/scim/v2/Groups/"+gid, e.write, "", patchBody(map[string]any{"op": "add", "path": "members", "value": []map[string]any{{"value": alice}}}), "If-Match", etag)
	e.wantError(resp, m, 412, "")
	resp, g2, _ = e.do("PATCH", "/scim/v2/Groups/"+gid, e.write, "", patchBody(map[string]any{"op": "remove", "path": `members[value eq "` + alice + `"]`}))
	if resp.StatusCode != 200 || len(g2["members"].([]any)) != 1 {
		t.Fatalf("remove %v", g2)
	}
	resp, g2, _ = e.do("PATCH", "/scim/v2/Groups/"+gid, e.write, "", patchBody(map[string]any{"op": "replace", "path": "members", "value": []map[string]any{{"value": alice}}}))
	if resp.StatusCode != 200 || len(g2["members"].([]any)) != 1 || g2["members"].([]any)[0].(map[string]any)["value"] != alice {
		t.Fatalf("replace %v", g2)
	}
	resp, g2, _ = e.do("PATCH", "/scim/v2/Groups/"+gid, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "Platform"}))
	if resp.StatusCode != 200 || g2["displayName"] != "Platform" {
		t.Fatalf("rename %v", g2)
	}
	if resp, _, _ := e.do("PATCH", "/scim/v2/Groups/"+gid, e.write, "", patchBody(map[string]any{"op": "replace", "path": "id", "value": "x"})); resp.StatusCode != 400 {
		t.Error("id is not patchable")
	}
	// list and filter
	_, m, _ = e.do("GET", "/scim/v2/Groups?filter="+urlQ(`displayName eq "platform"`), e.read, "", nil)
	if m["totalResults"] != float64(1) {
		t.Fatalf("filter %v", m)
	}
	_, m, _ = e.do("GET", "/scim/v2/Groups?filter="+urlQ(`members.value eq "`+bob+`"`), e.read, "", nil)
	if m["totalResults"] != float64(0) {
		t.Fatalf("members filter %v", m)
	}
	// the module's own state: the person's groups drive the provisioning plan
	names, _ := e.st.personGroups(context.Background(), alice)
	if len(names) != 1 || names[0] != "Platform" {
		t.Fatalf("person groups %v", names)
	}
	resp, _, _ = e.do("DELETE", "/scim/v2/Groups/"+gid, e.write, "", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete %d", resp.StatusCode)
	}
	resp, m, _ = e.do("GET", "/scim/v2/Groups/"+gid, e.read, "", nil)
	e.wantError(resp, m, 404, "")
	// a read token cannot touch groups
	resp, m, _ = e.do("POST", "/scim/v2/Groups", e.read, "", map[string]any{"displayName": "Nope"})
	e.wantError(resp, m, 403, "")
	// audit
	recs, _ := e.st.AuditList(context.Background(), 0, 200)
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Action] = true
	}
	for _, a := range []string{"scim.group.create", "scim.group.patch", "scim.group.delete"} {
		if !seen[a] {
			t.Errorf("%s not audited", a)
		}
	}
	if err := e.st.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

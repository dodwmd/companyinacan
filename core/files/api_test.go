package files

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

const (
	apiToken = "static-token-0123456789abcdef0123456789"
	patAlice = "canpat_alice-0123456789"
	patBob   = "canpat_bob-0123456789abc"
	patRO    = "canpat_readonly-0123456789"
	patBad   = "canpat_unknown-0123456789"
)

type fakeIntrospect map[string]modclient.Identity

func (f fakeIntrospect) Introspect(_ context.Context, tok string) (modclient.Identity, error) {
	if id, ok := f[tok]; ok {
		return id, nil
	}
	return modclient.Identity{}, nil
}

type apiEnv struct {
	*env
	srv *httptest.Server
	api *API
}

func newAPIEnv(t *testing.T, mods ...func(*Config, *StoreOptions)) *apiEnv {
	t.Helper()
	e := newEnv(t, mods...)
	in := fakeIntrospect{
		patAlice:                    {Active: true, Kind: "user", Subject: "user:alice", Permissions: []string{PermWrite, "other.perm"}},
		patBob:                      {Active: true, Kind: "user", Subject: "user:bob", Permissions: []string{PermWrite}},
		patRO:                       {Active: true, Kind: "user", Subject: "user:ro", Permissions: []string{PermRead}},
		"canpat_svc-0123456789abcd": {Active: true, Kind: "service", Subject: "svc:x", Permissions: []string{PermAdmin}},
		"canpat_nosub-0123456789ab": {Active: true, Kind: "user", Subject: "alice", Permissions: []string{PermAdmin}},
		"canpat_noperm-0123456789a": {Active: true, Kind: "user", Subject: "user:np", Permissions: []string{"jobs.read"}},
	}
	a := &API{Store: e.s, Auth: MultiAuth{StaticToken{Token: apiToken}, &CanAuth{In: &modclient.Introspector{API: in}}}, TrustProxy: false}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &apiEnv{env: e, srv: srv, api: a}
}

func (a *apiEnv) do(method, path, token string, body io.Reader, hdr ...string) (*http.Response, []byte) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.srv.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := a.srv.Client().Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (a *apiEnv) json(method, path, token string, v any) (*http.Response, map[string]any) {
	a.t.Helper()
	var body io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	resp, b := a.do(method, path, token, body, "Content-Type", "application/json")
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp, m
}

func TestAPIAuthentication(t *testing.T) {
	a := newAPIEnv(t)
	resp, _ := a.do("GET", "/v1/spaces", "", nil)
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("no token: %d %v", resp.StatusCode, resp.Header)
	}
	for _, tok := range []string{"nope", patBad, "canpat_svc-0123456789abcd", "canpat_nosub-0123456789ab", "canpat_noperm-0123456789a", "Bearer x"} {
		if resp, _ := a.do("GET", "/v1/spaces", tok, nil); resp.StatusCode != 401 {
			t.Errorf("token %q: %d, want 401", tok, resp.StatusCode)
		}
	}
	for _, tok := range []string{apiToken, patAlice, patRO} {
		if resp, _ := a.do("GET", "/v1/spaces", tok, nil); resp.StatusCode != 200 {
			t.Errorf("token %q: %d", tok, resp.StatusCode)
		}
	}
	// the permission gate: read-only cannot create
	if resp, _ := a.json("POST", "/v1/spaces", patRO, map[string]string{"name": "x"}); resp.StatusCode != 403 {
		t.Errorf("read-only create: %d", resp.StatusCode)
	}
	// admin routes need files.admin
	if resp, _ := a.do("GET", "/v1/audit", patAlice, nil); resp.StatusCode != 403 {
		t.Errorf("audit as a writer: %d", resp.StatusCode)
	}
	if resp, _ := a.do("GET", "/v1/audit", apiToken, nil); resp.StatusCode != 200 {
		t.Errorf("audit as admin: %d", resp.StatusCode)
	}
	// valid credentials are never throttled, however many failures came before from the same address
	for i := 0; i < 100; i++ {
		a.do("GET", "/v1/spaces", "wrong-token-"+strings.Repeat("x", i%5), nil)
	}
	if resp, _ := a.do("GET", "/v1/spaces", patAlice, nil); resp.StatusCode != 200 {
		t.Errorf("a valid token after many failures: %d", resp.StatusCode)
	}
	resp, _ = a.do("GET", "/v1/spaces", "wrong-token", nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("repeated failures: %d", resp.StatusCode)
	}
}

func TestAPIFlowAndHeaders(t *testing.T) {
	a := newAPIEnv(t)
	resp, sp := a.json("POST", "/v1/spaces", patAlice, map[string]string{"name": "Contracts"})
	if resp.StatusCode != 201 || sp["role"] != "manager" {
		t.Fatalf("create space: %d %v", resp.StatusCode, sp)
	}
	sid := sp["id"].(string)
	content := pdf("hello download")
	resp, b := a.do("POST", "/v1/spaces/"+sid+"/files?name="+"..%2F..%2Ftrick%22%3B.pdf&description=a+note", patAlice, bytes.NewReader(content), "Content-Type", "application/x-msdownload")
	if resp.StatusCode != 201 {
		t.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	var f map[string]any
	_ = json.Unmarshal(b, &f)
	fid := f["id"].(string)
	if f["content_type"] != "application/pdf" || f["name"] != "trick .pdf" || f["description"] != "a note" {
		t.Errorf("file: %v", f)
	}
	// download: attachment only, never the stored type, hardened headers
	resp, b = a.do("GET", "/v1/files/"+fid+"/content", patAlice, nil)
	h := resp.Header
	if resp.StatusCode != 200 || !bytes.Equal(b, content) {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	if h.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment; filename=") ||
		h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
		!strings.Contains(h.Get("Cache-Control"), "no-store") || h.Get("Content-Length") == "" {
		t.Errorf("headers: %v", h)
	}
	// inline is not available unless the operator enabled it
	resp, _ = a.do("GET", "/v1/files/"+fid+"/content?inline=1", patAlice, nil)
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("inline leaked: %v", resp.Header)
	}
	// a stranger sees 404 with the same body as a missing id
	r1, b1 := a.do("GET", "/v1/files/"+fid, patBob, nil)
	r2, b2 := a.do("GET", "/v1/files/fil_"+strings.Repeat("0", 24), patBob, nil)
	r3, b3 := a.do("GET", "/v1/files/not-an-id", patBob, nil)
	if r1.StatusCode != 404 || r2.StatusCode != 404 || r3.StatusCode != 404 || !bytes.Equal(b1, b2) || !bytes.Equal(b1, b3) {
		t.Errorf("invisible vs missing: %d %q / %d %q / %d %q", r1.StatusCode, b1, r2.StatusCode, b2, r3.StatusCode, b3)
	}
	r1, b1 = a.do("GET", "/v1/files/"+fid+"/content", patBob, nil)
	r2, b2 = a.do("GET", "/v1/files/fil_"+strings.Repeat("0", 24)+"/content", patBob, nil)
	if r1.StatusCode != 404 || !bytes.Equal(b1, b2) || r2.StatusCode != 404 {
		t.Errorf("content: %d %q vs %q", r1.StatusCode, b1, b2)
	}
	// share, then the public route
	resp, sh := a.json("POST", "/v1/files/"+fid+"/shares", patAlice, map[string]any{"expires_in": "2h", "one_time": true, "label": "x"})
	if resp.StatusCode != 201 || sh["token"] == nil {
		t.Fatalf("share: %d %v", resp.StatusCode, sh)
	}
	tok := sh["token"].(string)
	if !strings.HasSuffix(sh["url"].(string), "/s/"+tok) {
		t.Errorf("url %v", sh["url"])
	}
	resp, b = a.do("GET", "/s/"+tok, "", nil)
	if resp.StatusCode != 200 || !bytes.Equal(b, content) || resp.Header.Get("Content-Type") != "application/octet-stream" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") ||
		resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("share download: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = a.do("GET", "/s/"+tok, "", nil)
	if resp.StatusCode != 404 {
		t.Errorf("a one-time link twice: %d", resp.StatusCode)
	}
	// the share list never shows the token
	resp, b = a.do("GET", "/v1/files/"+fid+"/shares", patAlice, nil)
	if resp.StatusCode != 200 || strings.Contains(string(b), tok) || strings.Contains(string(b), tok[strings.LastIndex(tok, ".")+1:]) {
		t.Errorf("share list: %d %s", resp.StatusCode, b)
	}
}

func TestAPIInlineOnlyWhenEnabledForImages(t *testing.T) {
	a := newAPIEnv(t, func(c *Config, _ *StoreOptions) {
		c.Download.Inline, c.Download.InlineTypes = true, []string{"png"}
	})
	sp := a.space(a.alice, "S")
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 50)...)
	pf := a.upload(a.alice, sp.ID, "a.png", png)
	pd := a.upload(a.alice, sp.ID, "a.pdf", pdf("x"))
	resp, _ := a.do("GET", "/v1/files/"+pf.ID+"/content?inline=1", apiToken, nil)
	_ = resp
	resp, _ = a.do("GET", "/v1/files/"+pf.ID+"/content?inline=1", patAlice, nil) // alice is "user:alice": same as the store's alice
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("inline png: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = a.do("GET", "/v1/files/"+pd.ID+"/content?inline=1", patAlice, nil)
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("a pdf was served inline: %v", resp.Header)
	}
}

func TestAPIUploadLimitsAndErrors(t *testing.T) {
	a := newAPIEnv(t, func(c *Config, _ *StoreOptions) { c.Limits.MaxFileBytes = 1 << 12 })
	sp := a.space(a.alice, "S")
	up := func(q string, body []byte, hdr ...string) (*http.Response, []byte) {
		return a.do("POST", "/v1/spaces/"+sp.ID+"/files?"+q, patAlice, bytes.NewReader(body), hdr...)
	}
	if resp, _ := up("name=a.pdf", append(pdf(""), bytes.Repeat([]byte("a"), 1<<13)...)); resp.StatusCode != 413 {
		t.Errorf("too large: %d", resp.StatusCode)
	}
	// declared length too large is refused before the body is read
	big := bytes.NewReader(append(pdf(""), bytes.Repeat([]byte("a"), 1<<13)...))
	req, _ := http.NewRequest("POST", a.srv.URL+"/v1/spaces/"+sp.ID+"/files?name=a.pdf", big)
	req.Header.Set("Authorization", "Bearer "+patAlice)
	if resp, err := a.srv.Client().Do(req); err != nil || resp.StatusCode != 413 {
		t.Errorf("declared too large: %v", err)
	}
	if resp, _ := up("name=a.pdf", []byte("MZ not a pdf")); resp.StatusCode != 415 {
		t.Errorf("wrong type: %d", resp.StatusCode)
	}
	if resp, _ := up("name=a.pdf&ref=nope", pdf("")); resp.StatusCode != 400 {
		t.Errorf("bad ref: %d", resp.StatusCode)
	}
	if resp, _ := up("", pdf("")); resp.StatusCode != 400 {
		t.Errorf("no name: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/v1/spaces/"+sp.ID+"/files?name=a.pdf", patBob, bytes.NewReader(pdf(""))); resp.StatusCode != 404 {
		t.Errorf("a stranger uploading: %d", resp.StatusCode)
	}
	if resp, _ := up("name=a.txt", []byte("hello EICAR-TEST")); resp.StatusCode != 422 {
		t.Errorf("infected: %d", resp.StatusCode)
	}
	// unknown query parameters are refused, not ignored
	if resp, _ := up("name=a.pdf&path=/etc", pdf("")); resp.StatusCode != 400 {
		t.Errorf("unknown parameter: %d", resp.StatusCode)
	}
	// JSON endpoints: type, size and unknown fields
	if resp, _ := a.do("POST", "/v1/spaces", patAlice, strings.NewReader(`{"name":"x"}`), "Content-Type", "text/plain"); resp.StatusCode != 400 {
		t.Errorf("wrong content type: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/v1/spaces", patAlice, strings.NewReader(`{"name":"x","owner":"user:bob"}`), "Content-Type", "application/json"); resp.StatusCode != 400 {
		t.Errorf("unknown field: %d", resp.StatusCode)
	}
	if resp, _ := a.do("POST", "/v1/spaces", patAlice, strings.NewReader(`{"name":"`+strings.Repeat("x", 100000)+`"}`), "Content-Type", "application/json"); resp.StatusCode != 413 {
		t.Errorf("a huge JSON body: %d", resp.StatusCode)
	}
}

func TestAPIPatchNeedsMatchingRevAndShareRouteFailuresAreLimited(t *testing.T) {
	a := newAPIEnv(t, func(c *Config, _ *StoreOptions) { c.Share.FailuresPerMinute = 3 })
	sp := a.space(a.alice, "S")
	f := a.upload(a.alice, sp.ID, "a.pdf", pdf(""))
	resp, v := a.json("PATCH", "/v1/files/"+f.ID, patAlice, map[string]any{"name": "b.pdf"})
	if resp.StatusCode != 200 || v["name"] != "b.pdf" {
		t.Fatalf("patch: %d %v", resp.StatusCode, v)
	}
	resp, _ = a.do("PATCH", "/v1/files/"+f.ID, patAlice, strings.NewReader(`{"name":"c.pdf"}`), "Content-Type", "application/json", "If-Match", `"1"`)
	if resp.StatusCode != 409 {
		t.Errorf("a stale If-Match: %d", resp.StatusCode)
	}
	for i := 0; i < 4; i++ {
		resp, _ = a.do("GET", "/s/shr_"+strings.Repeat("a", 24)+".1.AAAA", "", nil)
	}
	if resp.StatusCode != 429 {
		t.Errorf("failures on the public route: %d", resp.StatusCode)
	}
}

func TestAPIAdminRoutes(t *testing.T) {
	a := newAPIEnv(t)
	sp := a.space(a.alice, "S")
	const subject = "cand_0123456789abcdef"
	f, err := a.s.Upload(ctx, a.alice, UploadRequest{SpaceID: sp.ID, Name: "r.pdf", Subject: subject, Body: bytes.NewReader(pdf(""))})
	if err != nil {
		t.Fatal(err)
	}
	resp, m := a.json("PUT", "/v1/files/"+f.ID+"/hold", apiToken, map[string]any{"legal_hold": true})
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		t.Fatalf("hold: %d %v", resp.StatusCode, m)
	}
	if resp, _ := a.do("POST", "/v1/subjects/"+subject+"/erasure", apiToken, nil); resp.StatusCode != 409 {
		t.Errorf("erasure under hold: %d", resp.StatusCode)
	}
	a.json("PUT", "/v1/files/"+f.ID+"/hold", apiToken, map[string]any{"legal_hold": false})
	if resp, m := a.json("POST", "/v1/subjects/"+subject+"/erasure", apiToken, nil); resp.StatusCode != 200 || m["subject"] != subject {
		t.Errorf("erasure: %d %v", resp.StatusCode, m)
	}
	if resp, m := a.json("GET", "/v1/erasures", apiToken, nil); resp.StatusCode != 200 || len(m["items"].([]any)) != 2 {
		t.Errorf("erasures: %d %v", resp.StatusCode, m)
	}
	if resp, _ := a.json("POST", "/v1/purge", apiToken, map[string]string{"subject": subject, "class": ClassContent}); resp.StatusCode != 204 {
		t.Errorf("purge: %d", resp.StatusCode)
	}
	if resp, _ := a.json("POST", "/v1/retention/run", apiToken, nil); resp.StatusCode != 200 {
		t.Errorf("retention: %d", resp.StatusCode)
	}
	if resp, _ := a.do("GET", "/v1/admin/files?subject="+subject, patAlice, nil); resp.StatusCode != 403 {
		t.Errorf("admin files as a writer: %d", resp.StatusCode)
	}
}

func TestRoutesListMatchesTheMux(t *testing.T) {
	rs := Routes()
	if len(rs) < 25 {
		t.Errorf("routes: %v", rs)
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if seen[r] || !strings.Contains(r, " /") {
			t.Errorf("route %q", r)
		}
		seen[r] = true
	}
	for _, must := range []string{"GET /healthz", "GET /s/{token}", "POST /v1/spaces/{id}/files", "GET /v1/files/{id}/content"} {
		if !seen[must] {
			t.Errorf("missing %s", must)
		}
	}
}

func TestHeadDoesNotConsumeALinkAndTrashDaysZeroNeverPurges(t *testing.T) {
	a := newAPIEnv(t, func(c *Config, _ *StoreOptions) { z := 0; c.Retention.TrashDays = &z })
	sp := a.space(a.alice, "S")
	f := a.upload(a.alice, sp.ID, "a.pdf", pdf("a"))
	tok, _, err := a.s.CreateShare(ctx, a.alice, f.ID, ShareRequest{OneTime: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := a.do("HEAD", "/s/"+tok, "", nil); resp.StatusCode != 405 {
		t.Errorf("HEAD on a link: %d", resp.StatusCode)
	}
	if resp, _ := a.do("HEAD", "/v1/files/"+f.ID+"/content", patAlice, nil); resp.StatusCode != 405 {
		t.Errorf("HEAD on content: %d", resp.StatusCode)
	}
	if resp, _ := a.do("GET", "/s/"+tok, "", nil); resp.StatusCode != 200 {
		t.Errorf("the one-time link was used up by a HEAD: %d", resp.StatusCode)
	}
	_ = a.s.DeleteFile(ctx, a.alice, f.ID)
	a.advance(400 * 24 * time.Hour)
	if rep, err := a.s.RunRetention(ctx); err != nil || rep.Purged != 0 {
		t.Errorf("trash_days 0 purged: %+v %v", rep, err)
	}
}

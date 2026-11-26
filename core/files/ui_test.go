package files

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// All names, ids and content are fake. The UI is exercised through the combined handler the binary
// serves (API on /v1 and /s/, UI everywhere else) over the same Store the JSON API uses.

const (
	patRoot  = "canpat_root-0123456789abc"
	patCarol = "canpat_carol-0123456789ab"
	restore  = "restore-token-0123456789abcdef0123456789"
)

type uiEnv struct {
	*env
	h      http.Handler
	in     fakeIntrospect
	inMu   sync.Mutex
	logs   *syncBuffer
	sess   modclient.SessionAPI
	apiSrv *API
}

// Introspect implements the can-core introspection the UI and the API authenticate tokens with.
func (u *uiEnv) Introspect(ctx context.Context, tok string) (modclient.Identity, error) {
	u.inMu.Lock()
	defer u.inMu.Unlock()
	return u.in.Introspect(ctx, tok)
}

func (u *uiEnv) revoke(tok string) {
	u.inMu.Lock()
	defer u.inMu.Unlock()
	delete(u.in, tok)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type uiOpts struct {
	sessions modclient.SessionAPI
	mods     []func(*Config, *StoreOptions)
}

func newUIEnvWith(t *testing.T, o uiOpts) *uiEnv {
	t.Helper()
	mods := append([]func(*Config, *StoreOptions){func(c *Config, _ *StoreOptions) {
		c.UI.Enabled, c.UI.InsecureCookies = true, true
	}}, o.mods...)
	u := &uiEnv{env: newEnv(t, mods...), logs: &syncBuffer{}, sess: o.sessions, in: fakeIntrospect{
		patAlice: {Active: true, Kind: "user", Subject: "user:alice", Permissions: []string{PermWrite}},
		patBob:   {Active: true, Kind: "user", Subject: "user:bob", Permissions: []string{PermWrite}},
		patCarol: {Active: true, Kind: "user", Subject: "user:carol", Permissions: []string{PermWrite}},
		patRO:    {Active: true, Kind: "user", Subject: "user:ro", Permissions: []string{PermRead}},
		patRoot:  {Active: true, Kind: "user", Subject: "user:root", Permissions: []string{PermAdmin}},
	}}
	auth := MultiAuth{StaticToken{Token: apiToken}, StaticRestoreToken{Token: restore},
		&CanAuth{In: &modclient.Introspector{API: u, TTL: time.Nanosecond}}}
	u.apiSrv = &API{Store: u.s, Auth: auth, Log: slog.New(slog.NewTextHandler(u.logs, nil))}
	u.h = newHandler(u.apiSrv, o.sessions)
	return u
}

func newUIEnv(t *testing.T, mods ...func(*Config, *StoreOptions)) *uiEnv {
	t.Helper()
	return newUIEnvWith(t, uiOpts{mods: mods})
}

type client struct {
	t       *testing.T
	e       *uiEnv
	cookies map[string]string
	csrf    string
}

func (u *uiEnv) client(t *testing.T) *client {
	return &client{t: t, e: u, cookies: map[string]string{}}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (c *client) request(r *http.Request) *httptest.ResponseRecorder {
	c.t.Helper()
	r.RemoteAddr = "192.0.2.10:1234"
	for k, v := range c.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	c.e.h.ServeHTTP(w, r)
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	if m := csrfRe.FindStringSubmatch(w.Body.String()); m != nil && w.Code == 200 && !strings.Contains(r.URL.Path, "/login") {
		c.csrf = m[1]
	}
	return w
}

func (c *client) do(method, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return c.request(r)
}

func (c *client) get(path string) *httptest.ResponseRecorder { return c.do("GET", path, nil) }

// post adds the session's CSRF token (taken from the last page that carried one).
func (c *client) post(path string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if !form.Has("csrf") {
		form.Set("csrf", c.csrf)
	}
	return c.do("POST", path, form, "Sec-Fetch-Site", "same-origin")
}

// part is one field of a multipart body; filename non-nil makes it a file part.
type mpart struct {
	name, value string
	file        bool
	filename    string
	header      textproto.MIMEHeader
}

func multipartBody(t *testing.T, parts []mpart) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range parts {
		var pw io.Writer
		var err error
		switch {
		case p.header != nil:
			pw, err = w.CreatePart(p.header)
		case p.file:
			pw, err = w.CreateFormFile(p.name, p.filename)
		default:
			pw, err = w.CreateFormField(p.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(pw, p.value)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b, w.FormDataContentType()
}

func (c *client) multipart(path string, parts []mpart) *httptest.ResponseRecorder {
	c.t.Helper()
	b, ct := multipartBody(c.t, parts)
	r := httptest.NewRequest("POST", path, b)
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return c.request(r)
}

// upload posts a file the way the upload form does: csrf first, the file last.
func (c *client) upload(path, filename, content string, fields ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	parts := []mpart{{name: "csrf", value: c.csrf}}
	for i := 0; i+1 < len(fields); i += 2 {
		parts = append(parts, mpart{name: fields[i], value: fields[i+1]})
	}
	return c.multipart(path, append(parts, mpart{name: "file", file: true, filename: filename, value: content}))
}

func (c *client) loginWith(tok string) *httptest.ResponseRecorder {
	c.t.Helper()
	w := c.get("/login")
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		c.t.Fatalf("no csrf on the login page: %d %s", w.Code, w.Body.String())
	}
	return c.do("POST", "/login", url.Values{"token": {tok}, "csrf": {m[1]}}, "Sec-Fetch-Site", "same-origin")
}

// login signs in and loads a page so the session CSRF token is known.
func (u *uiEnv) login(t *testing.T, tok string) *client {
	t.Helper()
	c := u.client(t)
	if w := c.loginWith(tok); w.Code != 303 {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}
	if w := c.get("/"); w.Code != 200 {
		t.Fatalf("home = %d %s", w.Code, w.Body.String())
	}
	return c
}

var locRe = regexp.MustCompile(`^/(?:spaces|files)/((?:spc|fil)_[0-9a-f]{24})`)

// created is the id in the redirect of a successful post.
func created(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	m := locRe.FindStringSubmatch(w.Header().Get("Location"))
	if w.Code != 303 || m == nil {
		t.Fatalf("want a redirect to the new record, got %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	return m[1]
}

func want(t *testing.T, w *httptest.ResponseRecorder, code int, parts ...string) string {
	t.Helper()
	body := w.Body.String()
	if w.Code != code {
		t.Fatalf("status %d, want %d: %s", w.Code, code, body)
	}
	for _, p := range parts {
		if !strings.Contains(body, p) {
			t.Fatalf("missing %q in %s", p, body)
		}
	}
	return body
}

func folderID(t *testing.T, e *uiEnv, space, name string) string {
	t.Helper()
	fs, err := e.s.ListFolders(context.Background(), e.alice, space)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Name == name {
			return f.ID
		}
	}
	t.Fatalf("no folder %q", name)
	return ""
}

const doc = "%PDF-1.7\nui test body\n"

// ---- the whole journey -------------------------------------------------------------------------

func TestUIJourneySpaceUploadVersionMoveTrashRestoreShare(t *testing.T) {
	e := newUIEnv(t)
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"Contracts"}}))
	want(t, a.get("/"), 200, "Contracts", "Manager")

	if w := a.post("/spaces/"+sp+"/folders", url.Values{"name": {"2026"}}); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	fld := folderID(t, e, sp, "2026")
	want(t, a.get("/spaces/"+sp+"/upload?folder="+fld), 200, `enctype="multipart/form-data"`, "25.0 MiB", `value="`+fld+`" selected`)

	// Upload: CRLF in the description (as browsers send it), the scan verdict on the page.
	w := a.upload("/spaces/"+sp+"/files", `C:\docs\lease.pdf`, doc, "folder", fld, "description", "line one\r\nline two", "subject", "cand_ui1", "ref", "jobs:application-1")
	fid := created(t, w)
	if loc := w.Header().Get("Location"); loc != "/files/"+fid+"?ok=flash.uploaded" {
		t.Fatalf("location %q", loc)
	}
	page := want(t, a.get("/files/"+fid+"?ok=flash.uploaded"), 200, "lease.pdf", "File uploaded.", "<strong>Clean</strong>", "line one\nline two", "cand_ui1", "jobs:application-1")
	if strings.Contains(page, `C:\docs`) {
		t.Fatal("the path of the name was kept")
	}
	want(t, a.get("/spaces/"+sp), 200, "lease.pdf", "2026")
	want(t, a.get("/spaces/"+sp+"?folder="+fld), 200, "lease.pdf", "Files in this folder")

	// A new version; the old one stays downloadable.
	v2 := doc + "second\n"
	if w := a.upload("/files/"+fid+"/versions", "anything.pdf", v2); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	for path, body := range map[string]string{"/files/" + fid + "/content": v2, "/files/" + fid + "/content?version=1": doc} {
		d := a.get(path)
		if d.Code != 200 || d.Body.String() != body {
			t.Fatalf("%s: %d %q", path, d.Code, d.Body)
		}
	}

	// Rename, describe, move to the top. A form made before another change is refused.
	rev := regexp.MustCompile(`name="rev" value="(\d+)"`).FindStringSubmatch(want(t, a.get("/files/"+fid), 200))[1]
	edit := url.Values{"rev": {rev}, "name": {"lease-signed.pdf"}, "description": {"a\r\nb"}, "folder": {""}}
	if w := a.post("/files/"+fid+"/update", edit); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	got := want(t, a.get("/files/"+fid), 200, "lease-signed.pdf", "a\nb")
	if strings.Contains(got, "?folder="+fld+`">2026`) {
		t.Fatal("still shown in the folder")
	}
	want(t, a.post("/files/"+fid+"/update", edit), 409, "changed since it was read")
	edit.Del("rev")
	want(t, a.post("/files/"+fid+"/update", edit), 422)
	want(t, a.post("/files/"+fid+"/update", url.Values{"rev": {"3"}, "name": {"lease.docx"}, "folder": {""}}), 422, "extension cannot change")

	// The folder is empty now and can go.
	if w := a.post("/folders/"+fld+"/delete", url.Values{"space": {sp}}); w.Code != 303 || w.Header().Get("Location") != "/spaces/"+sp+"?ok=flash.folder_deleted" {
		t.Fatal(w.Code, w.Header())
	}

	// A share link: shown once, works without a session, listed without its token, revocable.
	created := want(t, a.post("/files/"+fid+"/shares", url.Values{"label": {"for Dana"}, "hours": {"2"}}), 200, "shown only this once")
	link := regexp.MustCompile(`<code class="link">(http://example\.com)(/s/[^<]+)</code>`).FindStringSubmatch(created)
	if link == nil {
		t.Fatalf("no link on the page: %s", created)
	}
	anon := e.client(t)
	if d := anon.get(link[2]); d.Code != 200 || d.Body.String() != v2 {
		t.Fatal("share download", d.Code, d.Body)
	}
	listed := want(t, a.get("/files/"+fid), 200, "for Dana", "Live")
	if strings.Contains(listed, strings.TrimPrefix(link[2], "/s/")) || strings.Contains(listed, "/s/shr_") {
		t.Fatal("the token is shown again")
	}
	shr := regexp.MustCompile(`action="/shares/(shr_[0-9a-f]{24})/revoke"`).FindStringSubmatch(listed)[1]
	if w := a.post("/shares/"+shr+"/revoke", url.Values{"file": {fid}}); w.Code != 303 || w.Header().Get("Location") != "/files/"+fid+"?ok=flash.revoked" {
		t.Fatal(w.Code, w.Header())
	}
	if d := anon.get(link[2]); d.Code != 404 {
		t.Fatal("revoked link still works", d.Code)
	}
	want(t, a.get("/files/"+fid), 200, "Revoked")
	want(t, a.post("/files/"+fid+"/shares", url.Values{"hours": {"0"}}), 422)
	want(t, a.post("/files/"+fid+"/shares", url.Values{"hours": {"99999999999"}}), 422)
	want(t, a.post("/files/"+fid+"/shares", url.Values{"hours": {"1000"}}), 422, "lifetime must be between")
	if strings.Contains(e.logs.String(), "/s/shr_") {
		t.Fatal("a share token reached the log")
	}

	// Trash and restore.
	if w := a.post("/files/"+fid+"/delete", nil); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	want(t, a.get("/spaces/"+sp+"/trash"), 200, "lease-signed.pdf", `action="/files/`+fid+`/restore"`)
	trashed := want(t, a.get("/files/"+fid), 200, "in the trash")
	if strings.Contains(trashed, "/content") {
		t.Fatal("a trashed file offers a download")
	}
	want(t, a.get("/files/"+fid+"/content"), 404)
	if w := a.post("/files/"+fid+"/restore", nil); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	want(t, a.get("/files/"+fid), 200, "Active")
}

// The download is the API's: same headers, same bytes, audited, never inline, no HEAD.
func TestUIDownloadMatchesTheAPI(t *testing.T) {
	e := newUIEnv(t, func(c *Config, _ *StoreOptions) { c.Download.Inline, c.Download.InlineTypes = true, []string{"png"} })
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"Pictures"}}))
	png := "\x89PNG\r\n\x1a\nfake"
	fid := created(t, a.upload("/spaces/"+sp+"/files", "logo \u2713.png", png))

	api := httptest.NewRequest("GET", "/v1/files/"+fid+"/content", nil)
	api.Header.Set("Authorization", "Bearer "+patAlice)
	aw := httptest.NewRecorder()
	e.h.ServeHTTP(aw, api)
	uw := a.get("/files/" + fid + "/content?inline=1") // the UI never serves inline
	if aw.Code != 200 || uw.Code != 200 || uw.Body.String() != png {
		t.Fatal(aw.Code, uw.Code)
	}
	for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length", "X-Content-Type-Options", "Content-Security-Policy", "Cache-Control", "Referrer-Policy"} {
		if aw.Header().Get(h) == "" || aw.Header().Get(h) != uw.Header().Get(h) {
			t.Errorf("%s: API %q, UI %q", h, aw.Header().Get(h), uw.Header().Get(h))
		}
	}
	if !strings.HasPrefix(uw.Header().Get("Content-Disposition"), "attachment;") || uw.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal(uw.Header())
	}
	count := func() int { return e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'file.download'`) }
	n := count()
	if w := a.do("HEAD", "/files/"+fid+"/content", nil); w.Code != 405 || count() != n {
		t.Fatal("HEAD", w.Code, count()-n)
	}
	a.get("/files/" + fid + "/content")
	if count() != n+1 {
		t.Fatal("the download was not audited")
	}
	for _, bad := range []string{"?version=9", "?version=x", "?version=-1"} {
		want(t, a.get("/files/"+fid+"/content"+bad), 404)
	}
}

// A corrupted object cuts the connection: the UI's panic handling must not turn it into a page
// that looks like a complete file.
func TestUIDownloadOfCorruptedContentAborts(t *testing.T) {
	e := newUIEnv(t)
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	body := "%PDF-1.7\n" + strings.Repeat("x", 100<<10)
	fid := created(t, a.upload("/spaces/"+sp+"/files", "big.pdf", body))
	blobs := e.blobs()
	if len(blobs) != 1 {
		t.Fatal(blobs)
	}
	if err := os.WriteFile(blobs[0], []byte(body[:len(body)-1]+"y"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/files/"+fid+"/content", nil)
	for k, v := range a.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		return // cut before the headers arrived: also a failed transfer
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err == nil || len(got) >= len(body) {
		t.Fatalf("the corrupted download ended normally: %d bytes, %v", len(got), err)
	}
}

// ---- uploads -----------------------------------------------------------------------------------

func smallLimit(c *Config, _ *StoreOptions) { c.Limits.MaxFileBytes = 32 << 10 }

func TestUIUploadRefusalsLeaveNothingBehind(t *testing.T) {
	e := newUIEnv(t, smallLimit)
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	path := "/spaces/" + sp + "/files"

	want(t, a.upload(path, "a.pdf", "MZ\x90 not a pdf"), 415, "does not match")
	want(t, a.upload(path, "a.svg", "<svg/>"), 415, "not allowed")
	want(t, a.upload(path, "a.pdf", "%PDF-1.7\nEICAR-TEST"), 422, "virus scanner found")
	want(t, a.upload(path, "big.pdf", "%PDF-"+strings.Repeat("x", 33<<10)), 413, "32.0 KiB") // over the limit, under the form cap
	want(t, a.upload(path, "huge.pdf", "%PDF-"+strings.Repeat("x", 200<<10)), 413)           // over the declared length cap
	want(t, a.upload(path, "", ""), 422, "Choose a file")
	want(t, a.upload(path, "empty.pdf", ""), 422)
	want(t, a.multipart(path, []mpart{{name: "csrf", value: a.csrf}}), 422, "Choose a file")
	want(t, a.upload(path, "a.pdf", doc, "unknown", "1"), 422)
	want(t, a.upload(path, "a.pdf", doc, "folder", "", "folder", ""), 422)
	want(t, a.upload(path, "a.pdf", doc, "description", strings.Repeat("d", 5000)), 422)
	want(t, a.upload(path, "a.pdf", doc, "subject", "Jane Doe"), 422)
	want(t, a.upload("/spaces/spc_000000000000000000000000/files", "a.pdf", doc), 404)
	want(t, a.upload("/files/fil_000000000000000000000000/versions", "a.pdf", doc), 404)

	// A body cut off in the middle of the file is not a short file.
	b, ct := multipartBody(t, []mpart{{name: "csrf", value: a.csrf}, {name: "file", file: true, filename: "cut.pdf", value: doc + strings.Repeat("z", 9000)}})
	for _, keep := range []int{b.Len() - 40, 600} {
		r := httptest.NewRequest("POST", path, bytes.NewReader(b.Bytes()[:keep]))
		r.Header.Set("Content-Type", ct)
		if w := a.request(r); w.Code == 303 {
			t.Fatalf("a truncated upload (%d bytes) was stored", keep)
		}
	}

	if n := e.count(`SELECT COUNT(*) FROM files`); n != 0 || len(e.blobs()) != 0 || e.staged() != 0 {
		t.Fatalf("files %d, blobs %d, staged %d", n, len(e.blobs()), e.staged())
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'file.infected'`) != 1 {
		t.Fatal("the detection was not audited")
	}
}

// A part's transfer encoding is never applied: the bytes stored are the bytes sent.
func TestUIUploadDoesNotDecodeTransferEncodings(t *testing.T) {
	e := newUIEnv(t)
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="a.txt"`)
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	fid := created(t, a.multipart("/spaces/"+sp+"/files", []mpart{{name: "csrf", value: a.csrf}, {header: h, value: "a=3Db"}}))
	if got := a.get("/files/" + fid + "/content").Body.String(); got != "a=3Db" {
		t.Fatalf("stored %q", got)
	}
}

type countedBody struct {
	r io.Reader
	n int
}

func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// The CSRF token is the first part: a forged form is refused before the file is read.
func TestUIUploadChecksCSRFBeforeReadingTheFile(t *testing.T) {
	e := newUIEnv(t)
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	big := "%PDF-" + strings.Repeat("x", 1<<20)
	file := mpart{name: "file", file: true, filename: "a.pdf", value: big}
	for name, parts := range map[string][]mpart{
		"wrong token":      {{name: "csrf", value: strings.Repeat("A", 43)}, file},
		"no token":         {file},
		"token after file": {file, {name: "csrf", value: a.csrf}},
		"token as a file":  {{name: "csrf", file: true, filename: "x", value: a.csrf}, file},
	} {
		b, ct := multipartBody(t, parts)
		body := &countedBody{r: b}
		r := httptest.NewRequest("POST", "/spaces/"+sp+"/files", body)
		r.ContentLength = int64(b.Len())
		r.Header.Set("Content-Type", ct)
		if w := a.request(r); w.Code != 403 {
			t.Fatalf("%s: %d", name, w.Code)
		}
		if body.n > 64<<10 {
			t.Fatalf("%s: %d bytes read", name, body.n)
		}
	}
	// A urlencoded post to an upload route, and a multipart post to an ordinary route.
	want(t, a.post("/spaces/"+sp+"/files", nil), 403)
	want(t, a.multipart("/spaces", []mpart{{name: "csrf", value: a.csrf}, {name: "name", value: "x"}}), 403)
	if e.count(`SELECT COUNT(*) FROM files`) != 0 || e.count(`SELECT COUNT(*) FROM spaces`) != 1 {
		t.Fatal("something was written")
	}
}

func TestUIUploadsAreClosedWithoutAScanner(t *testing.T) {
	e := newUIEnv(t, func(c *Config, o *StoreOptions) {
		yes := true
		c.Scanner.Required, o.Scanner = &yes, NoopScanner{}
	})
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	want(t, a.get("/spaces/"+sp+"/upload"), 200, "checked by the virus scanner")
	want(t, a.upload("/spaces/"+sp+"/files", "a.pdf", doc), 503, "no virus scanner is configured")
}

func TestUIUploadNotScannedIsLabelled(t *testing.T) {
	e := newUIEnv(t, func(_ *Config, o *StoreOptions) { o.Scanner = NoopScanner{} })
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	want(t, a.get("/spaces/"+sp+"/upload"), 200, "may be stored without being scanned")
	fid := created(t, a.upload("/spaces/"+sp+"/files", "a.pdf", doc))
	want(t, a.get("/files/"+fid), 200, "<strong>Not scanned</strong>")
}

func TestUIQuotaIsReported(t *testing.T) {
	e := newUIEnv(t, smallLimit, func(c *Config, _ *StoreOptions) { c.Limits.QuotaBytesPerOwner = 40 << 10 })
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	body := "%PDF-" + strings.Repeat("q", 30<<10)
	created(t, a.upload("/spaces/"+sp+"/files", "a.pdf", body))
	want(t, a.upload("/spaces/"+sp+"/files", "b.pdf", body), 413, "quota would be exceeded")
}

// Over a real connection (not a recorder): the upload deadlines and body limits work with the
// server's wrappers, and a refused large upload still shows its page.
func TestUIUploadOverARealServer(t *testing.T) {
	e := newUIEnv(t, func(c *Config, _ *StoreOptions) { c.Limits.MaxFileBytes = 4 << 20 })
	srv := httptest.NewUnstartedServer(nil)
	srv.Config = newServer(e.h)
	srv.Start()
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	read := func(resp *http.Response, err error) (int, string, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get("Location")
	}
	_, login, _ := read(hc.Get(srv.URL + "/login"))
	read(hc.PostForm(srv.URL+"/login", url.Values{"csrf": {csrfRe.FindStringSubmatch(login)[1]}, "token": {patAlice}}))
	_, home, _ := read(hc.Get(srv.URL + "/"))
	csrf := csrfRe.FindStringSubmatch(home)[1]
	_, _, loc := read(hc.PostForm(srv.URL+"/spaces", url.Values{"csrf": {csrf}, "name": {"Real"}}))
	sp := locRe.FindStringSubmatch(loc)[1]
	send := func(filename, content string) (int, string, string) {
		b, ct := multipartBody(t, []mpart{{name: "csrf", value: csrf}, {name: "file", file: true, filename: filename, value: content}})
		return read(hc.Post(srv.URL+"/spaces/"+sp+"/files", ct, b))
	}
	big := "%PDF-" + strings.Repeat("s", 3<<20)
	code, body, loc := send("a.pdf", big)
	if code != 303 {
		t.Fatal(code, body)
	}
	fid := locRe.FindStringSubmatch(loc)[1]
	if code, body, _ := read(hc.Get(srv.URL + "/files/" + fid + "/content")); code != 200 || body != big {
		t.Fatal(code, len(body))
	}
	if code, body, _ := send("b.exe", big); code != 415 || !strings.Contains(body, "not allowed") {
		t.Fatal(code, body)
	}
	if code, body, _ := send("c.pdf", "%PDF-"+strings.Repeat("s", 4<<20)); code != 413 || !strings.Contains(body, "4.0 MiB") {
		t.Fatal(code, body)
	}
}

// ---- who may see and do what -------------------------------------------------------------------

func TestUIRolesAndAntiEnumeration(t *testing.T) {
	e := newUIEnv(t)
	a, b := e.login(t, patAlice), e.login(t, patBob)
	sp := created(t, a.post("/spaces", url.Values{"name": {"Secret plans"}}))
	fid := created(t, a.upload("/spaces/"+sp+"/files", "plan.pdf", doc))

	// Not a member: the same page as for ids that do not exist, on every route.
	stable := func(s string) string { return reReq.ReplaceAllString(s, "") }
	ghost := stable(want(t, b.get("/files/fil_000000000000000000000000"), 404))
	for _, p := range []string{"/spaces/" + sp, "/spaces/" + sp + "/members", "/spaces/" + sp + "/trash", "/spaces/" + sp + "/upload",
		"/files/" + fid, "/files/" + fid + "/content", "/files/not-an-id", "/spaces/spc_000000000000000000000000"} {
		w := b.get(p)
		if w.Code != 404 || stable(w.Body.String()) != ghost {
			t.Errorf("%s: %d differs from a missing id", p, w.Code)
		}
	}
	want(t, b.get("/"), 200, "not in any space")
	want(t, b.upload("/spaces/"+sp+"/files", "x.pdf", doc), 404)
	want(t, b.post("/files/"+fid+"/delete", nil), 404)

	// Added as a viewer by a bare user name.
	if w := a.post("/spaces/"+sp+"/members", url.Values{"kind": {"user"}, "principal": {"bob"}, "role": {"viewer"}}); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	want(t, a.get("/spaces/"+sp+"/members"), 200, "user:bob", "Viewer")
	page := want(t, b.get("/files/"+fid), 200, "plan.pdf")
	for _, hidden := range []string{"SHA-256", "Virus scan", "Share links", "/delete", "/update", "multipart"} {
		if strings.Contains(page, hidden) {
			t.Errorf("a viewer is shown %q", hidden)
		}
	}
	spacePage := want(t, b.get("/spaces/"+sp), 200)
	for _, hidden := range []string{"/upload", "/members", "/trash", "/folders"} {
		if strings.Contains(spacePage, hidden) {
			t.Errorf("a viewer is offered %q", hidden)
		}
	}
	want(t, b.get("/spaces/"+sp+"/upload"), 403)
	want(t, b.get("/spaces/"+sp+"/members"), 403)
	want(t, b.get("/spaces/"+sp+"/trash"), 403)
	want(t, b.upload("/spaces/"+sp+"/files", "x.pdf", doc), 403)
	want(t, b.upload("/files/"+fid+"/versions", "x.pdf", doc), 403)
	want(t, b.post("/files/"+fid+"/delete", nil), 403)
	want(t, b.post("/files/"+fid+"/shares", url.Values{"hours": {"1"}}), 403)
	want(t, b.post("/spaces/"+sp+"/members", url.Values{"kind": {"user"}, "principal": {"bob"}, "role": {"manager"}}), 403)
	want(t, b.post("/spaces/"+sp+"/folders", url.Values{"name": {"x"}}), 403)

	// The last manager stays; demoting yourself sends you back to the space.
	want(t, a.post("/spaces/"+sp+"/members", url.Values{"kind": {"user"}, "principal": {"alice"}, "role": {"viewer"}}), 409, "at least one manager")
	a.post("/spaces/"+sp+"/members", url.Values{"kind": {"user"}, "principal": {"bob"}, "role": {"manager"}})
	if w := a.post("/spaces/"+sp+"/members/remove", url.Values{"kind": {"user"}, "principal": {"user:alice"}}); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/?") {
		t.Fatal(w.Code, w.Header())
	}
	want(t, a.get("/spaces/"+sp), 404)
}

func TestUIReadOnlyTokenCannotChange(t *testing.T) {
	e := newUIEnv(t)
	a := e.login(t, patAlice)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	fid := created(t, a.upload("/spaces/"+sp+"/files", "a.pdf", doc))
	e.member(e.alice, sp, "user", "user:ro", "manager")
	ro := e.login(t, patRO)
	page := want(t, ro.get("/files/"+fid), 200)
	for _, hidden := range []string{"/delete", "/update", "multipart", "/revoke"} {
		if strings.Contains(page, hidden) {
			t.Errorf("a read-only token is offered %q", hidden)
		}
	}
	want(t, ro.get("/spaces/"+sp+"/upload"), 403)
	want(t, ro.post("/spaces", url.Values{"name": {"mine"}}), 403)
	want(t, ro.upload("/spaces/"+sp+"/files", "a.pdf", doc), 403)
	want(t, ro.post("/files/"+fid+"/delete", nil), 403)
}

// ---- sign-in and sessions ----------------------------------------------------------------------

func TestUITokenSignInAndSessions(t *testing.T) {
	e := newUIEnv(t)
	for _, bad := range []string{"", "canpat_unknown-0123456789", restore, "two words"} {
		if w := e.client(t).loginWith(bad); w.Code != 401 && w.Code != 429 {
			t.Fatalf("%q signed in: %d", bad, w.Code)
		}
	}
	// The service token acts as svc:static, as on the API.
	svc := e.login(t, apiToken)
	want(t, svc.get("/"), 200, "Holds and retention")

	a := e.login(t, patAlice)
	page := a.get("/")
	if strings.Contains(page.Body.String(), "Holds and retention") || strings.Contains(page.Body.String(), patAlice) {
		t.Fatal("admin navigation or the token on the page")
	}
	for h, v := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY"} {
		if page.Header().Get(h) != v {
			t.Errorf("%s = %q", h, page.Header().Get(h))
		}
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'; script-src 'none'; style-src 'self'") || !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("csp %q", csp)
	}
	// Revoked: noticed at the next recheck.
	e.revoke(patAlice)
	e.advance(uiRecheckDefault + time.Second)
	if w := a.get("/"); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatal("a revoked token kept its session", w.Code)
	}
	// Idle and absolute timeouts.
	b := e.login(t, patBob)
	e.advance(uiDefaultIdle + time.Minute)
	if w := b.get("/"); w.Code != 303 {
		t.Fatal("idle", w.Code)
	}
	b = e.login(t, patBob)
	for i := 0; i < 20; i++ {
		e.advance(25 * time.Minute)
		b.get("/")
	}
	if w := b.get("/"); w.Code != 303 {
		t.Fatal("absolute", w.Code)
	}
	// Logout ends it, and a cross-site post is refused.
	b = e.login(t, patBob)
	if w := b.do("POST", "/spaces", url.Values{"csrf": {b.csrf}, "name": {"x"}}, "Sec-Fetch-Site", "cross-site"); w.Code != 403 {
		t.Fatal("cross-site", w.Code)
	}
	if w := b.post("/logout", nil); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if w := b.get("/"); w.Code != 303 {
		t.Fatal("after logout", w.Code)
	}
}

func TestUISecureCookies(t *testing.T) {
	e := newUIEnvWith(t, uiOpts{mods: []func(*Config, *StoreOptions){func(c *Config, _ *StoreOptions) { c.UI.InsecureCookies = false }}})
	r := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	ck := w.Result().Cookies()
	if len(ck) != 1 || !strings.HasPrefix(ck[0].Name, "__Host-") || !ck[0].Secure || !ck[0].HttpOnly || ck[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("%+v", ck)
	}
}

func TestUILoginFailuresAreThrottledButNeverAValidToken(t *testing.T) {
	e := newUIEnv(t)
	last := 0
	for i := 0; i < uiFailedLogins+2; i++ {
		last = e.client(t).loginWith("canpat_unknown-" + strconv.Itoa(1000000+i)).Code
	}
	if last != 429 {
		t.Fatal("not throttled", last)
	}
	if w := e.client(t).loginWith(patAlice); w.Code != 303 {
		t.Fatal("a valid token was throttled", w.Code)
	}
}

// ---- legal hold, retention, erasure ------------------------------------------------------------

func TestUIHoldRetentionAndErasure(t *testing.T) {
	e := newUIEnv(t)
	a, root := e.login(t, patAlice), e.login(t, patRoot)
	sp := created(t, a.post("/spaces", url.Values{"name": {"HR secrets"}}))
	fid := created(t, a.upload("/spaces/"+sp+"/files", "cv-of-someone.pdf", doc, "subject", "cand_ui2"))

	// Administrators only.
	for _, p := range []string{"/admin/files", "/admin/erasure", "/admin/files/" + fid + "/hold"} {
		want(t, a.get(p), 403)
	}
	want(t, a.post("/admin/files/"+fid+"/hold", url.Values{"hold": {"off"}}), 403)
	want(t, a.post("/admin/erasure", url.Values{"subject": {"cand_ui2"}, "confirm": {"ERASE"}, "token": {patAlice}}), 403)

	// The list shows ids, never names; the administrator is not in the space.
	list := want(t, root.get("/admin/files?subject=cand_ui2"), 200, fid, sp, "cand_ui2")
	if strings.Contains(list, "cv-of-someone") || strings.Contains(list, "HR secrets") {
		t.Fatal("the admin list shows names")
	}
	want(t, root.get("/files/"+fid), 404)
	want(t, root.get("/admin/files?subject=Jane+Doe"), 422)

	// A hold form that does not say changes nothing.
	hold := "/admin/files/" + fid + "/hold"
	page := want(t, root.get(hold), 200, `value="on"`, `value="off"`)
	if strings.Contains(page, "checked") {
		t.Fatal("the hold form preselects")
	}
	want(t, root.post(hold, url.Values{}), 422, "on or off")
	want(t, root.post(hold, url.Values{"hold": {"on"}, "retain_until": {"soon"}}), 422)
	want(t, root.post(hold, url.Values{"hold": {"on"}, "retain_until": {"2027-01-31"}, "clear_retention": {"1"}}), 422)
	if w := root.post(hold, url.Values{"hold": {"on"}, "retain_until": {"2027-01-31"}}); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}
	want(t, root.get("/admin/files"), 200, "2027-01-31", "Yes")
	want(t, a.get("/files/"+fid), 200, "legal hold keeps this file", "2027-01-31")
	want(t, a.post("/files/"+fid+"/delete", nil), 409, "legal hold")

	erase := url.Values{"subject": {"cand_ui2"}, "confirm": {"ERASE"}, "token": {patRoot}}
	want(t, root.get("/admin/erasure?subject=cand_ui2"), 200, "would be destroyed: 1.", "retention date: 1. The erasure will be refused", `name="token"`)
	want(t, root.post("/admin/erasure", erase), 409, "blocked")
	// Lift the hold and clear the date.
	if w := root.post(hold, url.Values{"hold": {"off"}, "clear_retention": {"1"}}); w.Code != 303 {
		t.Fatal(w.Code, w.Body)
	}

	// Confirmation and step-up.
	want(t, root.post("/admin/erasure", url.Values{"subject": {"cand_ui2"}, "token": {patRoot}}), 422, "Type ERASE")
	want(t, root.post("/admin/erasure", url.Values{"subject": {"cand_ui2"}, "confirm": {"erase"}, "token": {patRoot}}), 422, "Type ERASE")
	want(t, root.post("/admin/erasure", url.Values{"subject": {"cand_ui2"}, "confirm": {"ERASE"}}), 422, "Re-enter your sign-in token")
	want(t, root.post("/admin/erasure", url.Values{"subject": {"cand_ui2"}, "confirm": {"ERASE"}, "token": {patAlice}}), 422)
	if e.count(`SELECT COUNT(*) FROM files WHERE status = 'active'`) != 1 {
		t.Fatal("erased without the step-up")
	}
	want(t, root.post("/admin/erasure", erase), 200, "Files destroyed: 1.", "file-content")
	want(t, a.get("/files/"+fid), 404)
	if len(e.blobs()) != 0 {
		t.Fatal("the blob survived")
	}
}

// The step-up asks the Authenticator now: a token revoked a second ago erases nothing, although the
// session has not been rechecked yet.
func TestUIErasureNeedsATokenThatStillWorks(t *testing.T) {
	e := newUIEnv(t)
	a, root := e.login(t, patAlice), e.login(t, patRoot)
	sp := created(t, a.post("/spaces", url.Values{"name": {"S"}}))
	created(t, a.upload("/spaces/"+sp+"/files", "a.pdf", doc, "subject", "cand_ui3"))
	root.get("/admin/erasure")
	root.csrf = csrfRe.FindStringSubmatch(root.get("/admin/erasure").Body.String())[1]
	e.revoke(patRoot)
	time.Sleep(time.Millisecond) // past the introspector's (test) cache lifetime
	want(t, root.post("/admin/erasure", url.Values{"subject": {"cand_ui3"}, "confirm": {"ERASE"}, "token": {patRoot}}), 403)
	if e.count(`SELECT COUNT(*) FROM files WHERE status = 'active'`) != 1 {
		t.Fatal("a revoked token erased")
	}
}

// ---- what the listener serves ------------------------------------------------------------------

func TestUIOffByDefaultAndAPIPathsKept(t *testing.T) {
	get := func(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	off := newUIEnv(t, func(c *Config, _ *StoreOptions) { c.UI.Enabled = false })
	for _, p := range []string{"/", "/login", "/static/files.css", "/spaces"} {
		if w := get(off.h, p); w.Code != 404 || strings.Contains(w.Body.String(), "<html") {
			t.Errorf("ui off, %s: %d", p, w.Code)
		}
	}
	on := newUIEnv(t)
	if w := get(on.h, "/v1/spaces", "Authorization", "Bearer "+patAlice); w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Error("api", w.Code, w.Header())
	}
	if w := get(on.h, "/v1/nope"); w.Code != 404 || strings.Contains(w.Body.String(), "<html") {
		t.Error("an unknown API path must not be a UI page", w.Code)
	}
	for p, code := range map[string]int{"/healthz": 200, "/readyz": 200, "/s/nonsense": 404, "/login": 200, "/static/files.css": 200, "/static/icon.svg": 200, "/manifest.webmanifest": 200, "/static/..%2fui.go": 404, "/nope": 404} {
		if w := get(on.h, p); w.Code != code {
			t.Errorf("%s = %d, want %d", p, w.Code, code)
		}
	}
	// the session cookie is no API credential
	a := on.login(t, patAlice)
	if w := a.get("/v1/spaces"); w.Code != 401 {
		t.Error("the API accepted a session cookie", w.Code)
	}
}

func TestUIConfigValidation(t *testing.T) {
	for _, bad := range []string{
		"ui: {enabled: true, insecure_cookies: true, force_secure: true}",
		"ui: {insecure_cookies: true}\nshare: {base_url: 'https://files.example.org'}",
		"ui: {idle_minutes: -1}",
		"ui: {notakey: 1}",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	c, err := ParseConfig([]byte("ui: {enabled: true, token_login: false}"))
	if err != nil || !c.UI.Enabled || c.UI.TokenLogin == nil || *c.UI.TokenLogin {
		t.Fatal(c.UI, err)
	}
	if DefaultConfig().UI.Enabled {
		t.Fatal("the UI must be off by default")
	}
}

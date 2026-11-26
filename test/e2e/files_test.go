//go:build e2e

package e2e

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// (j) Files: through the edge, with the service token (one identity and no second person to be
// refused; the Apps launch of the web UI is in TestAppsLaunchSignsInToModuleUIs).
// A space, an upload with a hostile name, a download that is always an attachment, a version, a share
// link that is public and works once, a rejected upload, a legal hold that blocks erasure, and the
// erasure itself. Every run creates its own space.
func TestFilesUploadShareHoldAndErase(t *testing.T) {
	tok := tokenOf(t, "files", "CAN_FILES_API_TOKEN")
	api := harness.API{C: web, Host: "files.localhost", Token: tok}
	u := harness.Unique()
	subject := "cand_e2e" + u

	sp := api.Post(t, "/v1/spaces", map[string]any{"name": "E2E docs " + u}).Expect(t, "create a space", 201).Str("id")
	doc := []byte("%PDF-1.7\ne2e contract " + u + "\n")
	up := web.Do(t, harness.Req{Method: http.MethodPost, Host: "files.localhost", Token: tok, Body: doc,
		Path: "/v1/spaces/" + sp + "/files?name=..%2F..%2Fcontract-" + u + ".pdf&subject=" + subject + "&ref=e2e:doc-" + u}).Expect(t, "upload a pdf", 201)
	fid := up.Str("id")
	if !strings.HasPrefix(up.Str("name"), "contract-") || strings.Contains(up.Str("name"), "/") {
		t.Fatalf("the path part of the name was kept: %q", up.Str("name"))
	}

	// Download: always an attachment, never sniffed, and the bytes are the bytes.
	dl := web.Do(t, harness.Req{Host: "files.localhost", Token: tok, Path: "/v1/files/" + fid + "/content"}).Expect(t, "download", 200)
	if !bytes.Equal(dl.Body, doc) || dl.Header.Get("Content-Type") != "application/octet-stream" || dl.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(dl.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %q %v", dl.Body, dl.Header)
	}

	// Refusals leave nothing behind.
	web.Do(t, harness.Req{Method: http.MethodPost, Host: "files.localhost", Token: tok, Body: []byte("MZ\x90\x00 not a pdf"), Path: "/v1/spaces/" + sp + "/files?name=tool.pdf"}).Expect(t, "an executable named .pdf", 415)
	web.Do(t, harness.Req{Method: http.MethodPost, Host: "files.localhost", Token: tok, Body: doc, Path: "/v1/spaces/" + sp + "/files?name=a.svg"}).Expect(t, "an svg", 415)
	web.Do(t, harness.Req{Host: "files.localhost", Path: "/v1/files/" + fid}).Expect(t, "no token", 401)

	// A new version; the old one stays addressable.
	v2 := append(append([]byte(nil), doc...), []byte("second\n")...)
	web.Do(t, harness.Req{Method: http.MethodPost, Host: "files.localhost", Token: tok, Body: v2, Path: "/v1/files/" + fid + "/versions"}).Expect(t, "a new version", 201)
	if got := web.Do(t, harness.Req{Host: "files.localhost", Token: tok, Path: "/v1/files/" + fid + "/content?version=1"}).Expect(t, "version 1", 200); !bytes.Equal(got.Body, doc) {
		t.Fatal("version 1 changed")
	}

	// A one-time link works without a token, once.
	sh := api.Post(t, "/v1/files/"+fid+"/shares", map[string]any{"expires_in": "1h", "one_time": true}).Expect(t, "create a one-time link", 201)
	path := "/s/" + sh.Str("token")
	first := web.Do(t, harness.Req{Host: "files.localhost", Path: path}).Expect(t, "the link, first use", 200)
	if !bytes.Equal(first.Body, v2) || first.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("share download: %q %v", first.Body, first.Header)
	}
	web.Do(t, harness.Req{Host: "files.localhost", Path: path}).Expect(t, "the link, second use", 404)

	// A legal hold blocks the erasure; lifting it lets it through; the content is gone afterwards.
	api.Put(t, "/v1/files/"+fid+"/hold", map[string]any{"legal_hold": true}).Expect(t, "place a hold", 204)
	api.Post(t, "/v1/subjects/"+subject+"/erasure", nil).Expect(t, "erasure under a hold", 409)
	api.Put(t, "/v1/files/"+fid+"/hold", map[string]any{"legal_hold": false}).Expect(t, "lift the hold", 204)
	api.Post(t, "/v1/subjects/"+subject+"/erasure", nil).Expect(t, "erase the subject", 200)
	api.Get(t, "/v1/files/"+fid+"/content").Expect(t, "the content after erasure", 404)
	if list := api.Get(t, "/v1/erasures").Expect(t, "tombstones", 200); !strings.Contains(list.String(), subject) {
		t.Fatalf("no tombstone for the subject: %.300s", list.String())
	}
}

// The files web UI through the edge (specs/049-files-ui): sign in with the service token, make a
// space, upload a file in a multipart form, download it as an attachment and make a share link.
func TestFilesWebUI(t *testing.T) {
	const host = "files.localhost"
	s := harness.NewSession(env)
	login := s.Do(t, harness.Req{Host: host, Path: "/login"}).Expect(t, "sign-in page", 200)
	s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/login",
		Body: url.Values{"csrf": {harness.CSRF(login.String())}, "token": {tokenOf(t, "files", "CAN_FILES_API_TOKEN")}}}).Expect(t, "sign in", 303)
	u := harness.Unique()
	csrf := harness.CSRF(s.Do(t, harness.Req{Host: host, Path: "/"}).Expect(t, "spaces page", 200).String())
	made := s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/spaces", Body: url.Values{"csrf": {csrf}, "name": {"UI " + u}}}).Expect(t, "create a space", 303)
	space, _, _ := strings.Cut(made.Header.Get("Location"), "?")

	doc := []byte("%PDF-1.7\nui e2e " + u + "\n")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("csrf", csrf)
	_ = mw.WriteField("description", "from the e2e suite")
	part, err := mw.CreateFormFile("file", "report-"+u+".pdf")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(doc)
	_ = mw.Close()
	up := s.Do(t, harness.Req{Method: "POST", Host: host, Path: space + "/files", Body: body.Bytes(),
		Headers: map[string]string{"Content-Type": mw.FormDataContentType()}}).Expect(t, "upload", 303)
	file, _, _ := strings.Cut(up.Header.Get("Location"), "?")
	page := s.Do(t, harness.Req{Host: host, Path: file}).Expect(t, "file page", 200)
	if !strings.Contains(page.String(), "Not scanned") { // the stack runs no virus scanner
		t.Fatalf("no scan verdict: %.600s", page.String())
	}
	dl := s.Do(t, harness.Req{Host: host, Path: file + "/content"}).Expect(t, "download", 200)
	if !bytes.Equal(dl.Body, doc) || !strings.HasPrefix(dl.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %q %v", dl.Body, dl.Header)
	}
	link := s.Do(t, harness.Req{Method: "POST", Host: host, Path: file + "/shares", Body: url.Values{"csrf": {csrf}, "hours": {"1"}}}).Expect(t, "share link", 200)
	m := regexp.MustCompile(`<code class="link">http://files\.localhost:8080(/s/[^<]+)</code>`).FindStringSubmatch(link.String())
	if m == nil {
		t.Fatalf("no link: %.600s", link.String())
	}
	web.Do(t, harness.Req{Host: host, Path: m[1]}).Expect(t, "the link, without a session", 200)

	// A personal access token issued in the admin UI signs in too, as that person.
	p := harness.NewSession(env)
	pl := p.Do(t, harness.Req{Host: host, Path: "/login"}).Expect(t, "sign-in page", 200)
	p.Do(t, harness.Req{Method: "POST", Host: host, Path: "/login",
		Body: url.Values{"csrf": {harness.CSRF(pl.String())}, "token": {issuePAT(t, adminSession(t), "files", "files.read")}}}).Expect(t, "sign in with a personal access token", 303)
	p.Do(t, harness.Req{Host: host, Path: "/"}).Expect(t, "its spaces", 200)
	p.Do(t, harness.Req{Host: host, Path: file}).Expect(t, "a space it is not in does not exist for it", 404)
}

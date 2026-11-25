//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// (j) Files: through the edge, with the service token (the compose stack has no can-core to
// introspect personal access tokens, so there is one identity and no second person to be refused).
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

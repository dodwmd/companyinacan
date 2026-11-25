package files

import (
	"bytes"
	"testing"
)

// s3Env is an env whose blobs live in the fake S3, so that the Store is exercised with a backend
// whose reader is tied to the context it was opened with.
func s3Env(t *testing.T, mods ...func(*Config, *StoreOptions)) *env {
	t.Helper()
	f := newFakeS3(t)
	be, err := NewS3Backend(S3Options{Endpoint: f.srv.URL, Bucket: "files-bkt", Region: f.region, KeyID: f.keyID, Secret: f.secret, AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	return newEnv(t, append([]func(*Config, *StoreOptions){func(_ *Config, o *StoreOptions) { o.Backend = be }}, mods...)...)
}

// QA blocker 1: the backend was opened with a bounded context that was cancelled before the body
// was streamed, which cut every S3 download after the first buffered bytes.
func TestS3DownloadsAreNotCutShort(t *testing.T) {
	e := s3Env(t)
	sp := e.space(e.alice, "Docs")
	body := append(pdf(""), bytes.Repeat([]byte("0123456789abcdef"), 200_000)...) // 3.2 MB
	f := e.upload(e.alice, sp.ID, "big.pdf", body)
	c, err := e.s.OpenContent(ctx, e.alice, f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.readAll(c); !bytes.Equal(got, body) {
		t.Fatalf("download: %d of %d bytes", len(got), len(body))
	}
	tok, _, err := e.s.CreateShare(ctx, e.alice, f.ID, ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	c, err = e.s.OpenShare(ctx, tok, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.readAll(c); !bytes.Equal(got, body) {
		t.Fatalf("share download: %d of %d bytes", len(got), len(body))
	}
}

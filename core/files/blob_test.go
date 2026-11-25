package files

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// backendContract is run against every Backend implementation.
func backendContract(t *testing.T, b Backend) {
	t.Helper()
	ctx := context.Background()
	data := bytes.Repeat([]byte("hello "), 1000)
	key := newBlobKey()
	if err := b.Put(ctx, key, bytes.NewReader(data), int64(len(data)), sum(data)); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := b.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Error("content differs")
	}
	// a wrong checksum is refused and leaves nothing
	k2 := newBlobKey()
	if err := b.Put(ctx, k2, bytes.NewReader(data), int64(len(data)), sum([]byte("other"))); err == nil {
		t.Error("put with a wrong checksum succeeded")
	}
	if _, err := b.Open(ctx, k2); !errors.Is(err, ErrNotFound) {
		t.Errorf("open after a refused put = %v, want not found", err)
	}
	// a short body is refused
	if err := b.Put(ctx, newBlobKey(), bytes.NewReader(data[:10]), int64(len(data)), sum(data)); err == nil {
		t.Error("put of a short body succeeded")
	}
	// keys that are not ours never reach the storage
	for _, bad := range []string{"", "../etc/passwd", "a/b", strings.Repeat("a", 31), strings.Repeat("A", 32), key + "0"} {
		if _, err := b.Open(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("open(%q) = %v, want not found", bad, err)
		}
		if err := b.Delete(ctx, bad); err != nil {
			t.Errorf("delete(%q) = %v, want a quiet no-op", bad, err)
		}
	}
	if err := b.Ping(ctx); err != nil {
		t.Errorf("ping: %v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Errorf("delete is not idempotent: %v", err)
	}
	if _, err := b.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("open after delete = %v", err)
	}
}

func TestLocalBackend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	b, err := NewLocalBackend(dir)
	if err != nil {
		t.Fatal(err)
	}
	backendContract(t, b)
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("blob dir mode %o, want 0700", st.Mode().Perm())
	}
	// sharded by the key's prefix, files 0600, nothing left in the temp area
	key := newBlobKey()
	if err := b.Put(context.Background(), key, strings.NewReader("x"), 1, sum([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, key[:2], key[2:4], key)
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("blob %s: %v %v", p, st, err)
	}
	left, _ := os.ReadDir(filepath.Join(dir, ".tmp"))
	if len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestLocalBackendRefusesASymlinkedDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "l")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if _, err := NewLocalBackend(link); err == nil {
		t.Error("a symlinked blob dir was accepted")
	}
}

func TestLocalPutHonoursContext(t *testing.T) {
	b, _ := NewLocalBackend(filepath.Join(t.TempDir(), "b"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Put(ctx, newBlobKey(), strings.NewReader("abc"), 3, sum([]byte("abc"))); err == nil {
		t.Error("a cancelled put succeeded")
	}
}

// ---- S3 against a fake that verifies SigV4 independently ----------------------------------------

type fakeS3 struct {
	mu      sync.Mutex
	objs    map[string][]byte
	secret  string
	keyID   string
	region  string
	bad     int
	puts    int
	srv     *httptest.Server
	tamper  bool
	reqPath []string
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objs: map[string][]byte{}, secret: "s3cr3t", keyID: "AKIATEST", region: "us-test-1"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func hm(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqPath = append(f.reqPath, r.Method+" "+r.URL.Path)
	// recompute the signature from the request, the way S3 does
	auth := r.Header.Get("Authorization")
	date, payload := r.Header.Get("X-Amz-Date"), r.Header.Get("X-Amz-Content-Sha256")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+f.keyID+"/") || date == "" || payload == "" {
		f.bad++
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	day := date[:8]
	scope := day + "/" + f.region + "/s3/aws4_request"
	canon := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery,
		"host:" + r.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + date + "\n",
		"host;x-amz-content-sha256;x-amz-date", payload}, "\n")
	ch := sha256.Sum256([]byte(canon))
	sts := "AWS4-HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hm(hm(hm(hm([]byte("AWS4"+f.secret), day), f.region), "s3"), "aws4_request")
	want := "Signature=" + hex.EncodeToString(hm(k, sts))
	if !strings.HasSuffix(auth, want) {
		f.bad++
		http.Error(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if sum(body) != payload {
			http.Error(w, "<Error><Code>XAmzContentSHA256Mismatch</Code></Error>", 400)
			return
		}
		f.puts++
		f.objs[r.URL.Path] = body
		w.WriteHeader(200)
	case http.MethodGet:
		b, ok := f.objs[r.URL.Path]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", 404)
			return
		}
		if f.tamper {
			b = append([]byte("X"), b[1:]...)
		}
		_, _ = w.Write(b)
	case http.MethodDelete:
		delete(f.objs, r.URL.Path)
		w.WriteHeader(204)
	case http.MethodHead:
		w.WriteHeader(200)
	}
}

func TestS3Backend(t *testing.T) {
	f := newFakeS3(t)
	b, err := NewS3Backend(S3Options{Endpoint: f.srv.URL, Bucket: "files-bkt", Region: f.region, Prefix: "p/", KeyID: f.keyID, Secret: f.secret, AllowInsecure: true})
	if err != nil {
		t.Fatal(err)
	}
	backendContract(t, b)
	if f.bad != 0 {
		t.Errorf("%d requests carried a bad signature", f.bad)
	}
	for _, p := range f.reqPath {
		if !strings.Contains(p, "/files-bkt") {
			t.Errorf("request %q is not path-style", p)
		}
	}
	key := newBlobKey()
	if err := b.Put(context.Background(), key, strings.NewReader("zz"), 2, sum([]byte("zz"))); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objs["/files-bkt/p/"+key[:2]+"/"+key[2:4]+"/"+key]; !ok {
		t.Errorf("object not at the sharded, prefixed key: %v", f.reqPath)
	}
}

func TestS3RefusesPlainHTTPUnlessDevelopment(t *testing.T) {
	if _, err := NewS3Backend(S3Options{Endpoint: "http://s3.example.com", Bucket: "bkt-1", KeyID: "a", Secret: "b"}); err == nil {
		t.Error("http endpoint accepted without AllowInsecure")
	}
	if _, err := NewS3Backend(S3Options{Endpoint: "https://s3.example.com", Bucket: "bkt-1"}); err == nil {
		t.Error("missing credentials accepted")
	}
	if _, err := NewS3Backend(S3Options{Endpoint: "https://s3.example.com", Bucket: "bkt-1", KeyID: "a", Secret: "b"}); err != nil {
		t.Errorf("https endpoint refused: %v", err)
	}
}

func TestS3ErrorsDoNotLeakCredentials(t *testing.T) {
	f := newFakeS3(t)
	b, _ := NewS3Backend(S3Options{Endpoint: f.srv.URL, Bucket: "files-bkt", Region: f.region, KeyID: f.keyID, Secret: "wrong-secret", AllowInsecure: true})
	err := b.Put(context.Background(), newBlobKey(), strings.NewReader("a"), 1, sum([]byte("a")))
	if err == nil {
		t.Fatal("a bad signature was accepted")
	}
	if strings.Contains(err.Error(), "wrong-secret") || strings.Contains(err.Error(), f.keyID) {
		t.Errorf("error leaks credentials: %v", err)
	}
}

// ---- detection ----------------------------------------------------------------------------------

func TestDetectType(t *testing.T) {
	allow := DefaultTypes
	cases := []struct {
		name string
		head []byte
		ok   bool
		ct   string
	}{
		{"a.pdf", []byte("%PDF-1.7\n"), true, "application/pdf"},
		{"A.PDF", []byte("%PDF-1.7\n"), true, "application/pdf"},
		{"a.png", []byte("\x89PNG\r\n\x1a\n"), true, "image/png"},
		{"a.jpg", []byte("\xff\xd8\xff\xe0"), true, "image/jpeg"},
		{"a.gif", []byte("GIF89a"), true, "image/gif"},
		{"a.webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), true, "image/webp"},
		{"a.docx", []byte("PK\x03\x04"), true, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"a.txt", []byte("plain text, \u00e9\n"), true, "text/plain; charset=utf-8"},
		{"a.csv", []byte("a,b\n1,2\n"), true, "text/csv; charset=utf-8"},
		{"a.pdf", []byte("MZ\x90\x00"), false, ""},            // an executable renamed
		{"a.png", []byte("%PDF-1.7"), false, ""},              // right extension class, wrong content
		{"a.txt", []byte("abc\x00def"), false, ""},            // NUL: binary
		{"a.txt", []byte("\xff\xfe\xfd"), false, ""},          // not UTF-8
		{"a.exe", []byte("MZ"), false, ""},                    // not in the list
		{"a", []byte("%PDF-"), false, ""},                     // no extension
		{".pdf", []byte("%PDF-"), false, ""},                  // no name
		{"a.pdf.", []byte("%PDF-"), false, ""},                // trailing dot
		{"a.svg", []byte("<svg onload=alert(1)>"), false, ""}, // active content never accepted
		{"a.html", []byte("<script>"), false, ""},
		{"a.zip", []byte("PK\x03\x04"), false, ""},  // no archives: nothing is ever unpacked
		{"a.docx", []byte("PK\x05\x06"), false, ""}, // an empty zip
	}
	for _, c := range cases {
		ct, err := DetectType(c.name, c.head, allow)
		if (err == nil) != c.ok || ct != c.ct {
			t.Errorf("DetectType(%q, %q) = %q, %v; want ok=%v ct=%q", c.name, c.head, ct, err, c.ok, c.ct)
		}
		if err != nil && !errors.Is(err, ErrType) {
			t.Errorf("error %v is not ErrType", err)
		}
	}
	if _, err := DetectType("a.pdf", []byte("%PDF-"), []string{"png"}); err == nil {
		t.Error("a type outside the allow-list was accepted")
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":         "passwd",
		`C:\Users\x\evil.pdf`:      "evil.pdf",
		"a\r\nSet-Cookie: x=1.pdf": "a Set-Cookie: x=1.pdf",
		`say "hi";.pdf`:            "say hi .pdf",
		"report\u202Efdp.exe":      "reportfdp.exe", // right-to-left override removed
		"  spaced   out .txt ":     "spaced out .txt",
		"":                         "",
		"\x00\x01":                 "",
		"....":                     "",
		".hidden.txt":              "hidden.txt",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 300) + ".pdf"
	got := SafeFilename(long)
	if len(got) > 120 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name = %d bytes %q", len(got), got)
	}
}

func TestContentDispositionIsAttachmentAndSingleLine(t *testing.T) {
	for _, name := range []string{"a.pdf", "r\u00e9sum\u00e9 \"x\".pdf", "x;y\r\nLocation: evil.pdf", strings.Repeat("\u4e2d", 40) + ".pdf"} {
		h := ContentDisposition(SafeFilename(name), false)
		if !strings.HasPrefix(h, "attachment; filename=\"") || strings.ContainsAny(h, "\r\n") {
			t.Errorf("%q -> %q", name, h)
		}
		if strings.Count(h, `"`) != 2 {
			t.Errorf("unbalanced quotes in %q", h)
		}
	}
	if h := ContentDisposition("a.png", true); !strings.HasPrefix(h, "inline;") {
		t.Errorf("inline = %q", h)
	}
}

// ---- verified streaming -------------------------------------------------------------------------

func TestVerifiedCopyHoldsBackTheTailUntilTheChecksumMatches(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 20000) // 200 kB: several chunks
	var out bytes.Buffer
	if err := VerifiedCopy(&out, bytes.NewReader(data), int64(len(data)), sum(data)); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("good copy: %v len=%d", err, out.Len())
	}
	// same length, last byte flipped: the error is reported and the tail was never written
	bad := append([]byte(nil), data...)
	bad[len(bad)-1] ^= 1
	out.Reset()
	err := VerifiedCopy(&out, bytes.NewReader(bad), int64(len(bad)), sum(data))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("flipped byte: %v", err)
	}
	if out.Len() >= len(bad) {
		t.Errorf("all %d bytes were written before the mismatch was known", out.Len())
	}
	// a short stored object and an over-long one
	for _, n := range []int{len(data) - 5, len(data) + 5} {
		src := append(append([]byte(nil), data...), []byte("12345")...)[:n]
		out.Reset()
		if err := VerifiedCopy(&out, bytes.NewReader(src), int64(len(data)), sum(data)); !errors.Is(err, ErrIntegrity) {
			t.Errorf("length %d vs %d: %v", n, len(data), err)
		}
	}
}

func TestVerifiedCopyMemoryIsBounded(t *testing.T) {
	// 64 MiB through a discarding writer must not allocate anywhere near that
	n := int64(64 << 20)
	r := io.LimitReader(zeroSrc{}, n)
	h := sha256.New()
	_, _ = io.Copy(h, io.LimitReader(zeroSrc{}, n))
	want := hex.EncodeToString(h.Sum(nil))
	allocs := testing.AllocsPerRun(1, func() {
		if err := VerifiedCopy(io.Discard, io.LimitReader(zeroSrc{}, n), n, want); err != nil {
			t.Fatal(err)
		}
	})
	_ = r
	if allocs > 100 {
		t.Errorf("%v allocations for a 64 MiB copy: it buffers", allocs)
	}
}

type zeroSrc struct{}

func (zeroSrc) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestScannerFakeAndClamd(t *testing.T) {
	ctx := context.Background()
	fs := &FakeScanner{InfectedMarker: "EICAR"}
	if v, err := fs.Scan(ctx, strings.NewReader("hello")); err != nil || v != ScanClean {
		t.Errorf("clean: %v %v", v, err)
	}
	if v, _ := fs.Scan(ctx, strings.NewReader("xxEICARxx")); v != ScanInfected {
		t.Errorf("infected: %v", v)
	}
	// a clamd INSTREAM fake
	ln := newClamdFake(t, "stream: OK\x00")
	c := &ClamdScanner{Address: ln, Timeout: 2 * time.Second}
	if v, err := c.Scan(ctx, strings.NewReader("hello")); err != nil || v != ScanClean {
		t.Errorf("clamd clean: %v %v", v, err)
	}
	ln = newClamdFake(t, "stream: Win.Test.EICAR_HDB-1 FOUND\x00")
	c = &ClamdScanner{Address: ln, Timeout: 2 * time.Second}
	if v, err := c.Scan(ctx, strings.NewReader("hello")); err != nil || v != ScanInfected {
		t.Errorf("clamd infected: %v %v", v, err)
	}
	// anything else, including an error line and no answer, is "could not scan"
	for _, ans := range []string{"INSTREAM size limit exceeded. ERROR\x00", "garbage", ""} {
		ln = newClamdFake(t, ans)
		c = &ClamdScanner{Address: ln, Timeout: 500 * time.Millisecond}
		if v, err := c.Scan(ctx, strings.NewReader("hello")); err == nil {
			t.Errorf("answer %q: verdict %v without error", ans, v)
		}
	}
	c = &ClamdScanner{Address: "127.0.0.1:1", Timeout: 500 * time.Millisecond}
	if _, err := c.Scan(ctx, strings.NewReader("x")); err == nil {
		t.Error("an unreachable scanner must be an error, never clean")
	}
}

// newClamdFake listens once, reads an INSTREAM request up to its zero-length terminator and
// answers with reply.
func newClamdFake(t *testing.T, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		cmd := make([]byte, len("zINSTREAM\x00"))
		if _, err := io.ReadFull(c, cmd); err != nil || string(cmd) != "zINSTREAM\x00" {
			return
		}
		for {
			var n [4]byte
			if _, err := io.ReadFull(c, n[:]); err != nil {
				return
			}
			sz := binary.BigEndian.Uint32(n[:])
			if sz == 0 {
				break
			}
			if _, err := io.CopyN(io.Discard, c, int64(sz)); err != nil {
				return
			}
		}
		_, _ = c.Write([]byte(reply))
	}()
	return ln.Addr().String()
}

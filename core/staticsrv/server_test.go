package staticsrv

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secret = "TOPSECRET-outside-the-site-root"

// fixture builds a site directory next to a secret file that must never be served.
func fixture(t *testing.T) (siteDir string) {
	t.Helper()
	parent := t.TempDir()
	siteDir = filepath.Join(parent, "site")
	write := func(rel, content string) {
		p := filepath.Join(siteDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<h1>home</h1>")
	write("404.html", "<h1>custom not found</h1>")
	write("about.html", "<h1>about</h1>")
	write("docs/index.html", "<h1>docs</h1>")
	write("assets/site.0123abcd.css", "body{color:red}")
	write("assets/logo.svg", "<svg/>")
	write("robots.txt", "User-agent: *\n")
	write(".can-site", "marker")
	write(".git/config", "gitconfig")
	write(".well-known/security.txt", "Contact: x")
	write("data.gz", "should not be served directly")
	write("big.css", strings.Repeat("a{b:c}", 100))
	write("secret-in-parent-name.txt", "inside")
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	// precompressed variants
	gz := func(s string) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
		return b.Bytes()
	}
	if err := os.WriteFile(filepath.Join(siteDir, "big.css.gz"), gz(strings.Repeat("a{b:c}", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "big.css.br"), []byte("fake-brotli"), 0o644); err != nil {
		t.Fatal(err)
	}
	// symlinks leading outside the root
	_ = os.Symlink(filepath.Join(parent, "secret.txt"), filepath.Join(siteDir, "link.txt"))
	_ = os.Symlink(parent, filepath.Join(siteDir, "linkdir"))
	return siteDir
}

func get(h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServesIndexAndPages(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	cases := []struct {
		target, body string
		code         int
	}{
		{"/", "home", 200},
		{"/index.html", "home", 200},
		{"/about", "about", 200},
		{"/about.html", "about", 200},
		{"/docs/", "docs", 200},
		{"/robots.txt", "User-agent", 200},
		{"/assets/logo.svg", "<svg/>", 200},
		{"/nope", "custom not found", 404},
		{"/docs/missing", "custom not found", 404},
	}
	for _, tc := range cases {
		w := get(h, "GET", tc.target)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("%s: code=%d body=%q", tc.target, w.Code, w.Body.String())
		}
	}
}

func TestDirectoryRedirect(t *testing.T) {
	w := get(New(Config{Dir: fixture(t)}), "GET", "/docs?x=1")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/docs/?x=1" {
		t.Fatalf("code=%d location=%q", w.Code, w.Header().Get("Location"))
	}
}

func TestContentTypes(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	for target, want := range map[string]string{
		"/":                         "text/html; charset=utf-8",
		"/assets/site.0123abcd.css": "text/css; charset=utf-8",
		"/assets/logo.svg":          "image/svg+xml",
		"/robots.txt":               "text/plain; charset=utf-8",
	} {
		if got := get(h, "GET", target).Header().Get("Content-Type"); got != want {
			t.Errorf("%s: content-type %q, want %q", target, got, want)
		}
	}
}

func TestHeadHasNoBody(t *testing.T) {
	w := get(New(Config{Dir: fixture(t)}), "HEAD", "/")
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		w := get(h, m, "/")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
			t.Errorf("%s: code=%d allow=%q", m, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := get(h, "OPTIONS", "/"); w.Code != http.StatusNoContent || w.Header().Get("Allow") == "" {
		t.Errorf("OPTIONS: code=%d", w.Code)
	}
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	for _, target := range []string{"/", "/nope", "/healthz", "/docs", "/../x"} {
		w := get(h, "GET", target)
		for k, want := range map[string]string{
			"X-Content-Type-Options":     "nosniff",
			"X-Frame-Options":            "DENY",
			"Referrer-Policy":            "strict-origin-when-cross-origin",
			"Cross-Origin-Opener-Policy": "same-origin",
		} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s %s = %q, want %q", target, k, got, want)
			}
		}
		csp := w.Header().Get("Content-Security-Policy")
		for _, d := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, d) {
				t.Errorf("%s CSP %q lacks %q", target, csp, d)
			}
		}
		if strings.Contains(csp, "unsafe-") || strings.Contains(csp, "*") {
			t.Errorf("%s CSP too loose: %q", target, csp)
		}
		if w.Header().Get("Permissions-Policy") == "" {
			t.Errorf("%s: no Permissions-Policy", target)
		}
	}
}

func TestHealthz(t *testing.T) {
	// Healthy even if the site directory has not been published yet.
	h := New(Config{Dir: filepath.Join(t.TempDir(), "missing")})
	w := get(h, "GET", "/healthz")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "ok" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("code=%d body=%q cc=%q", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	if w := get(h, "GET", "/"); w.Code != http.StatusNotFound {
		t.Errorf("missing site: code=%d, want 404", w.Code)
	}
	if w := get(h, "HEAD", "/healthz"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD healthz: %d", w.Code)
	}
}

func TestPlain404WithoutCustomPage(t *testing.T) {
	dir := fixture(t)
	if err := os.Remove(filepath.Join(dir, "404.html")); err != nil {
		t.Fatal(err)
	}
	w := get(New(Config{Dir: dir}), "GET", "/nope")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "not found") || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("code=%d ct=%q body=%q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestCacheHeaders(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	for target, want := range map[string]string{
		"/":                         "no-cache",
		"/about":                    "no-cache",
		"/assets/site.0123abcd.css": "public, max-age=31536000, immutable",
		"/assets/logo.svg":          "public, max-age=3600",
		"/robots.txt":               "public, max-age=3600",
		"/nope":                     "no-cache",
	} {
		if got := get(h, "GET", target).Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", target, got, want)
		}
	}
}

func TestConditionalAndRange(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	first := get(h, "GET", "/")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if w := get(h, "GET", "/", "If-None-Match", etag); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Errorf("If-None-Match: code=%d", w.Code)
	}
	if w := get(h, "GET", "/robots.txt", "Range", "bytes=0-3"); w.Code != http.StatusPartialContent || w.Body.String() != "User" {
		t.Errorf("Range: code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestPrecompressed(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	plain := strings.Repeat("a{b:c}", 100)

	w := get(h, "GET", "/big.css", "Accept-Encoding", "gzip, deflate")
	if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Vary") != "Accept-Encoding" || w.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("headers: %v", w.Header())
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); string(b) != plain {
		t.Error("gzip body does not decompress to the original")
	}

	if w := get(h, "GET", "/big.css", "Accept-Encoding", "br, gzip"); w.Header().Get("Content-Encoding") != "br" || w.Body.String() != "fake-brotli" {
		t.Errorf("br preferred: %v", w.Header())
	}
	if w := get(h, "GET", "/big.css", "Accept-Encoding", "gzip;q=0, br;q=0"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != plain {
		t.Errorf("q=0 must disable encoding: %v", w.Header())
	}
	if w := get(h, "GET", "/big.css"); w.Header().Get("Content-Encoding") != "" || w.Body.String() != plain {
		t.Error("no Accept-Encoding must serve identity")
	}
	if w := get(h, "GET", "/about", "Accept-Encoding", "gzip"); w.Header().Get("Content-Encoding") != "" {
		t.Error("no .gz sibling: must serve identity")
	}
	if w := get(h, "GET", "/big.css", "Accept-Encoding", "gzip"); w.Header().Get("ETag") == get(h, "GET", "/big.css").Header().Get("ETag") {
		t.Error("encoded variant must have a distinct ETag")
	}
}

func TestHiddenAndInternalFilesAreNotServed(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	for _, target := range []string{"/.can-site", "/.git/config", "/data.gz", "/big.css.gz", "/big.css.br", "/assets/.hidden"} {
		if w := get(h, "GET", target); w.Code != 404 || strings.Contains(w.Body.String(), "gitconfig") {
			t.Errorf("%s: code=%d", target, w.Code)
		}
	}
	if w := get(h, "GET", "/.well-known/security.txt"); w.Code != 200 {
		t.Errorf(".well-known must be reachable, got %d", w.Code)
	}
}

// pathCases are hostile request paths (already URL-decoded, as r.URL.Path sees them).
var pathCases = []string{
	"/../secret.txt", "/..", "/../", "/a/../../secret.txt", "/assets/../../secret.txt",
	"/./../secret.txt", "//secret.txt", "///etc/passwd", "/..%2fsecret.txt", "/%2e%2e/secret.txt",
	"/..\\secret.txt", "/assets\\..\\..\\secret.txt", "\\..\\secret.txt", "/\x00", "/index.html\x00.png",
	"/link.txt", "/linkdir/secret.txt", "/linkdir/", "/linkdir", "/assets/../link.txt",
	"/.", "/./.can-site", "/assets/./../.can-site", "/%2e/.git/config", "/.git/config",
	"/etc/passwd", "/proc/self/environ", "/C:/Windows/win.ini", "/..;/secret.txt", "/\xff\xfe",
	strings.Repeat("/..", 200) + "/secret.txt", "/" + strings.Repeat("a/", 5000) + "x",
}

func TestPathTraversalTable(t *testing.T) {
	h := New(Config{Dir: fixture(t)})
	for _, p := range pathCases {
		r := httptest.NewRequest("GET", "/", nil)
		r.URL.Path, r.URL.RawPath = p, ""
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		body := w.Body.String()
		if strings.Contains(body, secret) || strings.Contains(body, "gitconfig") || strings.Contains(body, "marker") || strings.Contains(body, "root:") {
			t.Errorf("%q leaked content (code %d)", p, w.Code)
		}
		switch w.Code {
		case 400, 404, 301:
		case 200:
			// "/." and friends may legitimately normalise to the home page, never to anything else.
			if !strings.Contains(body, "home") {
				t.Errorf("%q: unexpected 200 with %q", p, body)
			}
		default:
			t.Errorf("%q: unexpected status %d", p, w.Code)
		}
	}
}

func FuzzServePath(f *testing.F) {
	for _, p := range pathCases {
		f.Add(p)
	}
	f.Add("/about")
	f.Add("/docs/")
	dir := f.TempDir()
	site := filepath.Join(dir, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		f.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(site, "index.html"), []byte("<h1>home</h1>"), 0o644)
	_ = os.WriteFile(filepath.Join(site, "about.html"), []byte("<h1>about</h1>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "secret.txt"), []byte(secret), 0o644)
	_ = os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(site, "link.txt"))
	_ = os.MkdirAll(filepath.Join(site, "docs"), 0o755)
	_ = os.WriteFile(filepath.Join(site, "docs", "index.html"), []byte("<h1>docs</h1>"), 0o644)
	h := New(Config{Dir: site})
	f.Fuzz(func(t *testing.T, p string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.URL.Path, r.URL.RawPath = p, ""
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("path %q leaked the secret", p)
		}
		if w.Code != 200 && w.Code != 301 && w.Code != 400 && w.Code != 404 {
			t.Fatalf("path %q: unexpected status %d", p, w.Code)
		}
	})
}

func TestResolveRejects(t *testing.T) {
	for _, p := range []string{"", "x", "/a/../b", "/..", "/a\\b", "/a\x00", "/.hidden", "/a/.hidden/b", "/x.gz", "/x.br", "/\xff"} {
		if _, status := resolve(p); status == 0 {
			t.Errorf("resolve(%q) accepted", p)
		}
	}
	for p, want := range map[string]string{"/": "", "/a": "a", "/a/": "a", "/a//b": "a/b", "/./a": "a", "/.well-known/x": ".well-known/x"} {
		if got, status := resolve(p); status != 0 || got != want {
			t.Errorf("resolve(%q) = %q, %d; want %q", p, got, status, want)
		}
	}
}

func TestServerTimeouts(t *testing.T) {
	s := NewServer(":0", New(Config{Dir: t.TempDir()}))
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 64<<10 {
		t.Fatalf("server lacks bounded timeouts/limits: %+v", s)
	}
	if s.ReadHeaderTimeout > 30*time.Second {
		t.Error("ReadHeaderTimeout too generous")
	}
}

func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	h := New(Config{Dir: fixture(t), AccessLog: &buf})
	get(h, "GET", "/nope")
	if !strings.Contains(buf.String(), "GET /nope 404") {
		t.Errorf("access log: %q", buf.String())
	}
	buf.Reset()
	get(h, "GET", "/healthz")
	if buf.Len() != 0 {
		t.Error("health checks must not be logged")
	}
}

func TestSiteSwapIsPickedUp(t *testing.T) {
	dir := fixture(t)
	h := New(Config{Dir: dir})
	if !strings.Contains(get(h, "GET", "/").Body.String(), "home") {
		t.Fatal("precondition")
	}
	// Replace the whole directory, as a rebuild on the PVC might.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>new</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(get(h, "GET", "/").Body.String(), "new") {
		t.Error("server kept serving a stale directory handle")
	}
}

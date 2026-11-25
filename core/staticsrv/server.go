// Package staticsrv is a small, read-only static file server for generated sites.
//
// It is stdlib-only and is used by the can-static binary (the in-cluster site pod) and by
// `can site serve`. Files are opened through os.Root, so neither ".." tricks nor symlinks
// inside the site directory can reach files outside it. The server never writes to disk.
package staticsrv

import (
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Config configures the handler.
type Config struct {
	// Dir is the site directory. It is re-opened per request, so replacing it while the
	// server runs (a rebuild on the PVC) is picked up immediately.
	Dir string
	// AccessLog, when set, receives one line per request (health checks excluded).
	AccessLog io.Writer
}

const (
	cspPolicy = "default-src 'none'; img-src 'self' data:; style-src 'self'; font-src 'self'; connect-src 'self'; " +
		"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	permissionsPolicy = "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()"
	allowedMethods    = "GET, HEAD, OPTIONS"
)

var hashedAsset = regexp.MustCompile(`^assets/.+\.[0-9a-f]{8}\.[a-z0-9]+$`)

// New returns the site handler.
func New(cfg Config) http.Handler {
	s := &server{dir: cfg.Dir, log: cfg.AccessLog}
	return s
}

// NewServer wraps a handler in an http.Server with bounded timeouts and header size, so a
// slow or hostile client cannot pin goroutines or memory.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

type server struct {
	dir string
	log io.Writer
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", cspPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", permissionsPolicy)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if s.log != nil && r.URL.Path != "/healthz" {
		rec := &recorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			fmt.Fprintf(s.log, "%s %s %d %d %s\n", r.Method, r.URL.Path, rec.status, rec.bytes, time.Since(start).Round(time.Microsecond))
		}()
		w = rec
	}
	if r.URL.Path == "/healthz" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", allowedMethods)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "ok\n")
		}
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions:
		w.Header().Set("Allow", allowedMethods)
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", allowedMethods)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel, status := resolve(r.URL.Path)
	if status != 0 {
		s.notFound(w, r, status)
		return
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		s.notFound(w, r, http.StatusNotFound)
		return
	}
	defer func() { _ = root.Close() }()

	target, ok := s.lookup(root, rel, strings.HasSuffix(r.URL.Path, "/"))
	switch {
	case target == redirectToDir:
		u := url.URL{Path: r.URL.Path + "/", RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
		return
	case !ok:
		s.notFoundIn(w, r, root, http.StatusNotFound)
		return
	}
	s.serveFile(w, r, root, target, http.StatusOK)
}

const redirectToDir = "\x00dir"

// lookup maps a cleaned request path to a regular file inside root.
func (s *server) lookup(root *os.Root, rel string, slash bool) (string, bool) {
	isFile := func(p string) bool {
		st, err := root.Stat(p)
		return err == nil && st.Mode().IsRegular()
	}
	if rel == "" {
		return "index.html", isFile("index.html")
	}
	st, err := root.Stat(rel)
	switch {
	case err == nil && st.IsDir():
		if !slash {
			return redirectToDir, true
		}
		idx := rel + "/index.html"
		return idx, isFile(idx)
	case err == nil && st.Mode().IsRegular():
		return rel, true
	}
	if !slash && path.Ext(rel) == "" && isFile(rel+".html") {
		return rel + ".html", true
	}
	return "", false
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request, status int) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		plain404(w, status)
		return
	}
	defer func() { _ = root.Close() }()
	s.notFoundIn(w, r, root, status)
}

func (s *server) notFoundIn(w http.ResponseWriter, r *http.Request, root *os.Root, status int) {
	if st, err := root.Stat("404.html"); err == nil && st.Mode().IsRegular() {
		s.serveFile(w, r, root, "404.html", status)
		return
	}
	plain404(w, status)
}

func plain404(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-cache")
	msg := "not found"
	if status == http.StatusBadRequest {
		msg = "bad request"
	}
	http.Error(w, msg, status)
}

// resolve validates a decoded URL path and returns the cleaned relative path ("" is the
// site root). status is non-zero (400 or 404) when the path must be refused.
func resolve(p string) (rel string, status int) {
	if !strings.HasPrefix(p, "/") || !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\\") {
		return "", http.StatusBadRequest
	}
	var segs []string
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "" || seg == ".":
		case seg == "..":
			return "", http.StatusBadRequest
		case strings.HasPrefix(seg, ".") && seg != ".well-known":
			return "", http.StatusNotFound
		default:
			segs = append(segs, seg)
		}
	}
	rel = strings.Join(segs, "/")
	if strings.HasSuffix(rel, ".gz") || strings.HasSuffix(rel, ".br") {
		// Precompressed siblings are only reachable through content negotiation.
		return "", http.StatusNotFound
	}
	return rel, 0
}

func contentType(name string) string {
	switch ext := strings.ToLower(path.Ext(name)); ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".xml":
		return "application/xml; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
		return "application/octet-stream"
	}
}

func cacheControl(rel string) string {
	switch {
	case hashedAsset.MatchString(rel):
		return "public, max-age=31536000, immutable"
	case strings.HasSuffix(rel, ".html"):
		return "no-cache"
	}
	return "public, max-age=3600"
}

// acceptedEncodings returns the content codings the client accepts, best first.
func acceptedEncodings(h string) []string {
	ok := map[string]bool{}
	for _, part := range strings.Split(h, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		q := 1.0
		if k, v, found := strings.Cut(strings.ReplaceAll(params, " ", ""), "q="); found && k == "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		ok[name] = q > 0
	}
	var out []string
	for _, enc := range []string{"br", "gzip"} {
		if ok[enc] {
			out = append(out, enc)
		}
	}
	return out
}

var encodingExt = map[string]string{"br": ".br", "gzip": ".gz"}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request, root *os.Root, rel string, status int) {
	name, encoding := rel, ""
	var f *os.File
	var st fs.FileInfo
	for _, enc := range acceptedEncodings(r.Header.Get("Accept-Encoding")) {
		if pf, err := root.Open(rel + encodingExt[enc]); err == nil {
			if pst, err := pf.Stat(); err == nil && pst.Mode().IsRegular() {
				f, st, encoding = pf, pst, enc
				break
			}
			_ = pf.Close()
		}
	}
	if f == nil {
		var err error
		if f, err = root.Open(rel); err != nil {
			plain404(w, http.StatusNotFound)
			return
		}
		if st, err = f.Stat(); err != nil || !st.Mode().IsRegular() {
			_ = f.Close()
			plain404(w, http.StatusNotFound)
			return
		}
	}
	defer func() { _ = f.Close() }()

	h := w.Header()
	h.Set("Content-Type", contentType(name))
	h.Set("Cache-Control", cacheControl(rel))
	h.Add("Vary", "Accept-Encoding")
	etag := fmt.Sprintf(`W/"%x-%x"`, st.Size(), st.ModTime().UnixNano())
	if encoding != "" {
		h.Set("Content-Encoding", encoding)
		etag = fmt.Sprintf(`W/"%x-%x-%s"`, st.Size(), st.ModTime().UnixNano(), encoding)
	}
	if status == http.StatusOK {
		h.Set("ETag", etag)
		http.ServeContent(w, r, name, st.ModTime(), f)
		return
	}
	// Error pages: never cache, never conditional.
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

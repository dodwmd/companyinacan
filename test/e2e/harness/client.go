package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// T is the part of testing.TB the helpers use, so the package can be unit tested with a fake.
type T interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Env describes where the stack is and how to reach it. Every field has a default for the
// compose stack of `make e2e-up`; the E2E_* environment variables override them.
type Env struct {
	Port string // host port of the edge (E2E_PORT, default 8080)
	// AppsPort is the first of the edge's one-host-name ports (E2E_APPS_PORT, default 8180): admin UI,
	// then work, people, jobs and finance on the next four.
	AppsPort int
	Compose  string // compose command prefix (E2E_COMPOSE, default "docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml"), run from Root
	Root     string // repository root, for the public dev secrets (E2E_ROOT, default: found from this file)
}

// FromEnv reads the environment.
func FromEnv() Env {
	e := Env{Port: os.Getenv("E2E_PORT"), Compose: os.Getenv("E2E_COMPOSE"), Root: os.Getenv("E2E_ROOT")}
	if e.Port == "" {
		e.Port = "8080"
	}
	if e.AppsPort, _ = strconv.Atoi(os.Getenv("E2E_APPS_PORT")); e.AppsPort == 0 {
		e.AppsPort = 8180
	}
	if e.Compose == "" {
		e.Compose = "docker compose -p can-e2e -f docker-compose.yml -f dev/compose/e2e.yml"
	}
	if e.Root == "" {
		_, f, _, _ := runtime.Caller(0)
		e.Root = filepath.Join(filepath.Dir(f), "..", "..", "..")
	}
	return e
}

var envLine = regexp.MustCompile(`(?m)^([A-Z0-9_]+)=(.*)$`)

// Secret reads one value from a public dev secrets file, e.g. Secret("work", "CAN_WORK_ADMIN_TOKEN").
func (e Env) Secret(t T, file, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.Root, "dev", "compose", "secrets", file+".env")) //nolint:gosec // test helper, fixed layout
	if err != nil {
		t.Fatalf("cannot read the dev secrets file %s.env: %v", file, err)
	}
	for _, m := range envLine.FindAllStringSubmatch(string(b), -1) {
		if m[1] == key {
			return strings.TrimSpace(m[2])
		}
	}
	t.Fatalf("%s.env has no %s", file, key)
	return ""
}

// Run executes a compose subcommand (for example `exec -T core /can-core ...`) and returns its output.
func (e Env) Run(t T, args ...string) string {
	t.Helper()
	parts := strings.Fields(e.Compose)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], args...)...) //nolint:gosec // operator-supplied compose command
	cmd.Dir = e.Root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", e.Compose, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// Client talks to the edge. The host name picks the virtual host; the connection always goes to
// 127.0.0.1 at the port of the URL, so *.localhost needs no resolver support.
type Client struct {
	env  Env
	http *http.Client
}

// NewClient returns a client without cookies (API use).
func NewClient(e Env) *Client { return newClient(e, false) }

// NewSession returns a client with its own cookie jar (one browser).
func NewSession(e Env) *Client { return newClient(e, true) }

func newClient(e Env, jar bool) *Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return d.DialContext(ctx, network, "127.0.0.1:"+port)
		},
		DisableKeepAlives: true,
	}
	c := &http.Client{
		Transport:     tr,
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if jar {
		c.Jar, _ = cookiejar.New(nil)
	}
	return &Client{env: e, http: c}
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// String is the body as text.
func (r *Response) String() string { return string(r.Body) }

// JSON decodes the body into a map (nil on failure).
func (r *Response) JSON() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

// Str returns a top-level string field of a JSON body, numbers rendered as decimal text.
func (r *Response) Str(key string) string {
	switch v := r.JSON()[key].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	}
	return ""
}

// Req describes one request.
type Req struct {
	Method  string
	Host    string // virtual host, e.g. "work.localhost" (no port)
	Port    string // edge port, when not Env.Port
	Path    string
	Token   string // bearer token, optional
	Body    any    // JSON-encoded unless it is url.Values (form) or []byte/string (raw)
	Headers map[string]string
}

// Do performs the request. A transport failure fails the test with a pointer at the stack.
func (c *Client) Do(t T, r Req) *Response {
	t.Helper()
	var body io.Reader
	ctype := ""
	switch b := r.Body.(type) {
	case nil:
	case url.Values:
		body, ctype = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	case string:
		body = strings.NewReader(b)
	case []byte:
		body = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("cannot encode the request body for %s %s: %v", r.Method, r.Path, err)
		}
		body, ctype = bytes.NewReader(j), "application/json"
	}
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	if r.Port == "" {
		r.Port = c.env.Port
	}
	req, err := http.NewRequest(r.Method, "http://"+r.Host+":"+r.Port+r.Path, body)
	if err != nil {
		t.Fatalf("bad request %s %s: %v", r.Method, r.Path, err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	for k, v := range r.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s http://%s%s: %v (is the stack up? `make e2e-up`; edge port %s)", r.Method, r.Host, r.Path, err, r.Port)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// Expect fails the test unless the response status is one of the wanted codes.
func (r *Response) Expect(t T, what string, want ...int) *Response {
	t.Helper()
	for _, w := range want {
		if r.Status == w {
			t.Logf("ok   %s (HTTP %d)", what, r.Status)
			return r
		}
	}
	body := r.String()
	if len(body) > 400 {
		body = body[:400] + "..."
	}
	t.Fatalf("%s: want HTTP %v, got %d: %s", what, want, r.Status, body)
	return r
}

// Cookie returns the value of a cookie the client holds for a host, or "".
func (c *Client) Cookie(host, name string) string {
	if c.http.Jar == nil {
		return ""
	}
	u, _ := url.Parse("http://" + host + ":" + c.env.Port + "/")
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

var (
	csrfField = regexp.MustCompile(`name="csrf"\s+value="([^"]*)"`)
	csrfAlt   = regexp.MustCompile(`value="([^"]*)"\s+name="csrf"`)
)

// CSRF extracts the hidden csrf form field from an HTML page.
func CSRF(html string) string {
	if m := csrfField.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := csrfAlt.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// Wait polls fn until it returns true or the deadline passes.
func Wait(t T, what string, d time.Duration, fn func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for {
		if fn() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(time.Second)
	}
}

// API is a client bound to one virtual host and one bearer token.
type API struct {
	C     *Client
	Host  string
	Token string
}

// Do sends a request with the bound host and token.
func (a API) Do(t T, method, path string, body any) *Response {
	t.Helper()
	return a.C.Do(t, Req{Method: method, Host: a.Host, Path: path, Token: a.Token, Body: body})
}

// Get is Do with GET.
func (a API) Get(t T, path string) *Response { t.Helper(); return a.Do(t, http.MethodGet, path, nil) }

// Post is Do with POST.
func (a API) Post(t T, path string, body any) *Response {
	t.Helper()
	return a.Do(t, http.MethodPost, path, body)
}

// Put is Do with PUT.
func (a API) Put(t T, path string, body any) *Response {
	t.Helper()
	return a.Do(t, http.MethodPut, path, body)
}

// Patch is Do with PATCH.
func (a API) Patch(t T, path string, body any) *Response {
	t.Helper()
	return a.Do(t, http.MethodPatch, path, body)
}

// WithToken returns the same API with another bearer token.
func (a API) WithToken(tok string) API { a.Token = tok; return a }

// Unique returns a short run-unique suffix so scenarios can be repeated against the same data.
func Unique() string { return fmt.Sprintf("%x", time.Now().UnixNano()%0xffffffffff) }

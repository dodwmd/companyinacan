package work

import (
	"crypto/tls"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

var tlsStub = tls.ConnectionState{}

func newTestClock() *clock { return &clock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)} }

// The contract (specs/023-work-ui/contracts/ui-routes.md) lists every route with the session it
// needs and the permission it checks. This test fails when code and contract drift apart.
func TestUIRoutesMatchContract(t *testing.T) {
	raw, err := os.ReadFile("../../specs/023-work-ui/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `((?:GET|POST) [^`]+)` \\| (yes|no) \\| (none|work\\.read|work\\.write) \\|")
	doc := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		doc[m[1]] = m[2] + "/" + m[3]
	}
	code := map[string]string{}
	for _, r := range UIRoutes() {
		sess := "no"
		if r.Session {
			sess = "yes"
		}
		perm := r.Perm
		if perm == "" {
			perm = "none"
		}
		code[r.Method+" "+r.Path] = sess + "/" + perm
	}
	var keys []string
	for k := range code {
		keys = append(keys, k)
	}
	for k := range doc {
		if _, ok := code[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if code[k] != doc[k] {
			t.Errorf("%s: code %q, contract %q", k, code[k], doc[k])
		}
	}
	if len(code) < 25 {
		t.Fatalf("only %d routes", len(code))
	}
}

// Every route is gated exactly as its table row says, for anonymous callers, read-only callers and
// callers whose token does not cover the project.
func TestUIRoutePermissionTable(t *testing.T) {
	f := newUIFixture(t)
	sp := f.h.mustDo(201, "POST", "/v1/projects/ACME/sprints", bootstrap, map[string]any{"name": "s"})
	sid := strconv.Itoa(int(sp["id"].(float64)))
	fill := strings.NewReplacer("{$}", "", "{key}", "ACME", "{issue}", f.acmeKey, "{id}", sid, "{file}", "work.css", "{link}", "1")
	anon := newUIBrowser(t, f.h)
	ro := f.browser(t, f.ro)
	rw := f.browser(t, f.rw)
	for _, r := range UIRoutes() {
		path := fill.Replace(r.Path)
		anonForm := url.Values(nil)
		if r.Method == "POST" {
			anonForm = url.Values{}
		}
		got := anon.do(r.Method, path, anonForm)
		switch {
		case r.Session && (got.Code != 303 || !strings.HasPrefix(got.Header.Get("Location"), "/ui/login")):
			t.Errorf("%s %s anonymous = %d %q, want a redirect to login", r.Method, path, got.Code, got.Header.Get("Location"))
		case !r.Session && got.Code == 303 && strings.HasPrefix(got.Header.Get("Location"), "/ui/login") && r.Path != "/ui/login":
			t.Errorf("%s %s is public but redirected to login", r.Method, path)
		}
		if r.Perm == PermWrite && r.Session {
			v := url.Values{"csrf": {ro.csrf("/ui/")}}
			if resp := ro.do("POST", path, v); resp.Code != 403 {
				t.Errorf("%s %s with a read-only token = %d, want 403", r.Method, path, resp.Code)
			}
		}
		if r.Perm == PermRead && r.Method == "GET" {
			if resp := rw.do("GET", path, nil); resp.Code != 200 {
				t.Errorf("GET %s with a write token = %d", path, resp.Code)
			}
		}
	}
	// Every registered pattern also answers unknown methods without a 500.
	if r := rw.do("DELETE", "/ui/issues/"+f.acmeKey, nil); r.Code >= 500 {
		t.Errorf("DELETE = %d", r.Code)
	}
}

package finance

import (
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The route table of specs/031-finance-ui/contracts/ui-routes.md is the contract: every route the UI
// serves is listed with whether it needs a session, the scope it needs and the Store methods it
// calls, and nothing is listed that is not served.

type docRoute struct{ session, scope, methods string }

func readRouteContract(t *testing.T) map[string]docRoute {
	t.Helper()
	b, err := os.ReadFile("../../specs/031-finance-ui/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `((?:GET|POST) [^`]+)` \\| ([^|]+) \\| ([^|]+) \\| ([^|]+) \\|")
	out := map[string]docRoute{}
	for _, l := range strings.Split(string(b), "\n") {
		if m := row.FindStringSubmatch(l); m != nil {
			out[m[1]] = docRoute{session: strings.TrimSpace(m[2]), scope: strings.TrimSpace(m[3]), methods: strings.TrimSpace(m[4])}
		}
	}
	return out
}

func TestUIRoutesMatchContract(t *testing.T) {
	doc := readRouteContract(t)
	patterns, scope := UIRoutes()
	for _, r := range patterns {
		d, ok := doc[r]
		if !ok {
			t.Errorf("route %q is served but not in the contract", r)
			continue
		}
		if d.scope != scope[r] {
			t.Errorf("route %q: the contract says scope %q, the code %q", r, d.scope, scope[r])
		}
		if d.session != "yes" && d.session != "no" {
			t.Errorf("route %q: session column must be yes or no, got %q", r, d.session)
		}
		if (d.session == "no") != (scope[r] == "-") {
			t.Errorf("route %q: session %q does not fit scope %q", r, d.session, scope[r])
		}
	}
	sort.Strings(patterns)
	for r := range doc {
		i := sort.SearchStrings(patterns, r)
		if i == len(patterns) || patterns[i] != r {
			t.Errorf("contract lists %q but the UI does not serve it", r)
		}
	}
}

func contractPath(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	path = strings.NewReplacer("{id}", "1", "{n}", "1", "{file}", "x", "{$}", "").Replace(path)
	if path == "" {
		path = "/"
	}
	return method, path
}

func TestUISessionRoutesRequireASession(t *testing.T) {
	u := newUIEnv(t)
	c := u.client(t)
	for pattern, d := range readRouteContract(t) {
		if d.session != "yes" {
			continue
		}
		method, path := contractPath(pattern)
		w := c.do(method, path, nil)
		if method == "POST" {
			w = c.do(method, path, url.Values{}, "Sec-Fetch-Site", "same-origin")
		}
		if w.Code != 303 || w.Header().Get("Location") != "/login" {
			t.Errorf("%s without a session = %d %q, want 303 /login", pattern, w.Code, w.Header().Get("Location"))
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", pattern, w.Header().Get("Cache-Control"))
		}
	}
}

// Every route refuses a token that lacks the scope the contract names, and an own-only token on every
// route that is not marked +own, before any handler runs.
func TestUIContractRoutesEnforceTheirScope(t *testing.T) {
	u := newUIEnv(t)
	tokens := map[string]*client{
		"read":    u.login(t, u.token("user:r", []string{"finance.read"}, nil, nil, false)),
		"write":   u.login(t, u.token("user:w", []string{"finance.read", "finance.write"}, nil, nil, false)),
		"approve": u.login(t, u.token("user:a", []string{"finance.read", "finance.write", "finance.approve"}, nil, nil, false)),
		"own":     u.login(t, u.token("user:o", []string{"finance.read", "finance.write", "finance.approve", "finance.admin"}, nil, nil, true)),
	}
	// who lacks what: a read-only token lacks write, approve and admin; a write token lacks approve and
	// admin; an approve token lacks admin.
	lacking := map[string][]string{"write": {"read"}, "approve": {"read", "write"}, "admin": {"read", "write", "approve"}}
	do := func(c *client, pattern string) int {
		method, path := contractPath(pattern)
		if method == "POST" {
			return c.post(path, url.Values{}).Code
		}
		return c.get(path).Code
	}
	checked := 0
	for pattern, d := range readRouteContract(t) {
		if d.session != "yes" {
			continue
		}
		scope, own := strings.CutSuffix(d.scope, "+own")
		for _, who := range lacking[scope] {
			checked++
			if code := do(tokens[who], pattern); code != 403 {
				t.Errorf("%s with only %s = %d, want 403", pattern, who, code)
			}
		}
		if !own {
			checked++
			if code := do(tokens["own"], pattern); code != 403 {
				t.Errorf("%s with an own-only token = %d, want 403", pattern, code)
			}
		}
	}
	if checked < 80 {
		t.Fatalf("only %d scope checks ran", checked)
	}
}

// Every Store method the contract names must exist, so the table cannot drift into describing a call
// that is not made.
func TestUIContractNamesRealStoreMethods(t *testing.T) {
	src := ""
	ents, _ := os.ReadDir(".")
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			b, _ := os.ReadFile(e.Name())
			src += string(b)
		}
	}
	for pattern, d := range readRouteContract(t) {
		for _, m := range regexp.MustCompile("`([A-Z][A-Za-z]+)`").FindAllStringSubmatch(d.methods, -1) {
			if !strings.Contains(src, "(s *Store) "+m[1]+"(") && m[1] != "Authenticator" {
				t.Errorf("%s: names Store.%s which does not exist", pattern, m[1])
			}
		}
	}
}

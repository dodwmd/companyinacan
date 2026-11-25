package people

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The route table of specs/026-people-ui/contracts/ui-routes.md is the contract: every route the
// UI serves is listed with whether it needs a session and which Service method (and so which
// policy action) gates it, and nothing is listed that is not served.

type docRoute struct{ session, gate string }

func readRouteContract(t *testing.T) map[string]docRoute {
	t.Helper()
	b, err := os.ReadFile("../../specs/026-people-ui/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `((?:GET|POST) [^`]+)` \\| ([^|]+) \\| ([^|]+) \\|")
	out := map[string]docRoute{}
	for _, l := range strings.Split(string(b), "\n") {
		if m := row.FindStringSubmatch(l); m != nil {
			out[m[1]] = docRoute{session: strings.TrimSpace(m[2]), gate: strings.TrimSpace(m[3])}
		}
	}
	return out
}

func TestUIRoutesMatchContract(t *testing.T) {
	doc := readRouteContract(t)
	var served []string
	for _, r := range UIRoutes() {
		served = append(served, r)
		d, ok := doc[r]
		if !ok {
			t.Errorf("route %q is served but not in the contract", r)
			continue
		}
		if d.gate == "" || d.gate == "-" && d.session != "no" {
			t.Errorf("route %q has no gate in the contract", r)
		}
		if d.session != "yes" && d.session != "no" {
			t.Errorf("route %q: session column must be yes or no, got %q", r, d.session)
		}
	}
	sort.Strings(served)
	for r := range doc {
		i := sort.SearchStrings(served, r)
		if i == len(served) || served[i] != r {
			t.Errorf("contract lists %q but the UI does not serve it", r)
		}
	}
}

func TestUISessionRoutesRequireASession(t *testing.T) {
	u := newUIEnv(t)
	doc := readRouteContract(t)
	for pattern, d := range doc {
		if d.session != "yes" {
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{id}", "x", "{$}", "").Replace(path)
		if path == "" {
			path = "/"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(""))
		w := httptest.NewRecorder()
		u.h.ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Errorf("%s without a session = %d %q, want 303 /login", pattern, w.Code, w.Header().Get("Location"))
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", pattern, w.Header().Get("Cache-Control"))
		}
	}
}

// Every Service method the contract names as a gate must exist, so the table cannot drift into
// describing a check that is not there.
func TestUIContractGatesNameRealServiceMethods(t *testing.T) {
	doc := readRouteContract(t)
	src := ""
	for _, f := range []string{"directory.go", "oneonone.go", "goals.go", "checklists.go", "service.go", "reviews.go", "reviews_feedback.go", "timeoff.go"} {
		b, _ := os.ReadFile(f)
		src += string(b)
	}
	for pattern, d := range doc {
		for _, m := range regexp.MustCompile("`([A-Z][A-Za-z]+)`").FindAllStringSubmatch(d.gate, -1) {
			if !strings.Contains(src, ") "+m[1]+"(") {
				t.Errorf("%s: gate names Service.%s which does not exist", pattern, m[1])
			}
		}
	}
}

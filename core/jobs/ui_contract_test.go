package jobs

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The route table of specs/027-jobs-ui/contracts/ui-routes.md is the contract: every route the UI
// serves is listed with whether it needs a session, which Store method gates it, and which role
// may use it; nothing is listed that is not served.

type docRoute struct {
	session, gate, action string
	roles                 []string
}

func readRouteContract(t *testing.T) map[string]docRoute {
	t.Helper()
	b, err := os.ReadFile("../../specs/027-jobs-ui/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `((?:GET|POST) [^`]+)` \\| ([^|]+) \\| ([^|]+) \\| ([^|]+) \\| ([^|]+) \\|")
	out := map[string]docRoute{}
	for _, l := range strings.Split(string(b), "\n") {
		if m := row.FindStringSubmatch(l); m != nil {
			d := docRoute{session: strings.TrimSpace(m[2]), gate: strings.TrimSpace(m[3]), action: strings.TrimSpace(m[4])}
			if r := strings.TrimSpace(m[5]); r != "-" {
				for _, x := range strings.Split(r, ",") {
					d.roles = append(d.roles, strings.TrimSpace(x))
				}
			}
			out[m[1]] = d
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
	u := newUIScenario(t)
	for pattern, d := range readRouteContract(t) {
		if d.session != "yes" {
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{id}", "x", "{$}", "").Replace(path)
		if path == "" {
			path = "/"
		}
		w := httptest.NewRecorder()
		u.h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader("")))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Errorf("%s without a session = %d %q, want 303 /login", pattern, w.Code, w.Header().Get("Location"))
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", pattern, w.Header().Get("Cache-Control"))
		}
	}
}

// Every Store method the contract names as a gate must exist, so the table cannot drift into
// describing a check that is not there.
func TestUIContractGatesNameRealStoreMethods(t *testing.T) {
	src := ""
	ents, _ := os.ReadDir(".")
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") && !strings.HasPrefix(e.Name(), "ui") {
			b, _ := os.ReadFile(e.Name())
			src += string(b)
		}
	}
	for pattern, d := range readRouteContract(t) {
		for _, m := range regexp.MustCompile("`([A-Z][A-Za-z]+)`").FindAllStringSubmatch(d.gate, -1) {
			if !strings.Contains(src, "(s *Store) "+m[1]+"(") {
				t.Errorf("%s: gate names Store.%s which does not exist", pattern, m[1])
			}
		}
	}
}

// The Roles column must equal the permission matrix for the Action, and the live UI must agree:
// a token of every role is sent to each GET route, and 403 appears exactly for the roles that are
// not listed.
func TestUIContractRolesMatchThePermissionMatrix(t *testing.T) {
	u := newUIScenario(t)
	tokens := map[string]string{RoleAdmin: adminTok, RoleRecruiter: u.recruiter, RoleHiringManager: u.hmAlice, RoleInterviewer: u.ivy}
	clients := map[string]*client{}
	for role, tok := range tokens {
		clients[role] = u.login(t, tok)
	}
	for pattern, d := range readRouteContract(t) {
		if d.action == "-" || d.action == "" {
			continue
		}
		want := slices.Clone(matrix[Action(d.action)])
		got := slices.Clone(d.roles)
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("%s: contract roles %v, permission matrix for %q is %v", pattern, got, d.action, want)
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		if method != "GET" {
			continue
		}
		id := map[string]string{"/jobs/{id}": u.j1, "/jobs/{id}/edit": u.j1, "/candidates/{id}": u.c1}[path]
		if id == "" {
			id = u.a1
		}
		path = strings.ReplaceAll(path, "{id}", id)
		for role, c := range clients {
			w := c.get(path)
			listed := slices.Contains(d.roles, role)
			if listed && w.Code == http.StatusForbidden {
				t.Errorf("%s: %s is listed but got 403", pattern, role)
			}
			if !listed && w.Code != http.StatusForbidden {
				t.Errorf("%s: %s is not listed but got %d", pattern, role, w.Code)
			}
		}
	}
}

package files

import (
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden HTML files of the files UI")

// ---- the route contract ------------------------------------------------------------------------

type docRoute struct{ session, scope, methods string }

func readRouteContract(t *testing.T) map[string]docRoute {
	t.Helper()
	b, err := os.ReadFile("../../specs/050-files-ui/contracts/ui-routes.md")
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
		if (d.session == "no") != (scope[r] == "-") || (d.session != "yes" && d.session != "no") {
			t.Errorf("route %q: session %q does not fit scope %q", r, d.session, scope[r])
		}
	}
	for r := range doc {
		if !slices.Contains(patterns, r) {
			t.Errorf("contract lists %q but the UI does not serve it", r)
		}
	}
}

func contractPath(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	id := "fil_000000000000000000000000"
	switch {
	case strings.HasPrefix(path, "/spaces/"):
		id = "spc_000000000000000000000000"
	case strings.HasPrefix(path, "/folders/"):
		id = "fld_000000000000000000000000"
	case strings.HasPrefix(path, "/shares/"):
		id = "shr_000000000000000000000000"
	}
	path = strings.NewReplacer("{id}", id, "{file}", "files.css", "{$}", "").Replace(path)
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

// isUpload says which routes of the contract take multipart bodies.
func isUpload(d docRoute) bool {
	return strings.Contains(d.methods, "`Upload`") || strings.Contains(d.methods, "`UploadVersion`")
}

func postRoute(c *client, d docRoute, path string) int {
	if isUpload(d) {
		return c.upload(path, "a.pdf", doc).Code
	}
	return c.post(path, url.Values{}).Code
}

// Every route refuses a token that lacks the permission the contract names, before any handler runs.
func TestUIContractRoutesEnforceTheirScope(t *testing.T) {
	u := newUIEnv(t)
	ro, rw := u.login(t, patRO), u.login(t, patAlice)
	lacking := map[string][]*client{"write": {ro}, "admin": {ro, rw}}
	checked := 0
	for pattern, d := range readRouteContract(t) {
		method, path := contractPath(pattern)
		for _, c := range lacking[d.scope] {
			checked++
			code := 0
			if method == "POST" {
				code = postRoute(c, d, path)
			} else {
				code = c.get(path).Code
			}
			if code != 403 {
				t.Errorf("%s without %s = %d, want 403", pattern, d.scope, code)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d permission checks ran", checked)
	}
}

func TestUIEveryPostNeedsTheCSRFToken(t *testing.T) {
	u := newUIEnv(t)
	c := u.login(t, patRoot)
	good, n := c.csrf, 0
	for pattern, d := range readRouteContract(t) {
		method, path := contractPath(pattern)
		if method != "POST" {
			continue
		}
		for _, bad := range []string{"", strings.Repeat("A", len(good))} {
			n++
			c.csrf = bad
			var code int
			if d.session == "no" {
				code = c.do("POST", path, url.Values{"csrf": {bad}, "token": {patRoot}, "ticket": {"x"}}).Code
				if pattern == "POST /login/ticket" && code == 404 {
					continue // not installed under can-core: the route does not exist (see the SSO tests)
				}
			} else {
				code = postRoute(c, d, path)
			}
			if code != 403 {
				t.Errorf("%s with csrf %q = %d, want 403", pattern, bad, code)
			}
		}
	}
	if n < 20 {
		t.Fatalf("only %d checks ran", n)
	}
}

// Every Store method the contract names must exist and be called by the UI, so the table cannot
// drift into describing calls that are not made.
func TestUIContractNamesRealStoreMethods(t *testing.T) {
	all, ui := "", ""
	ents, _ := os.ReadDir(".")
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			b, _ := os.ReadFile(e.Name())
			all += string(b)
			if strings.HasPrefix(e.Name(), "ui") {
				ui += string(b)
			}
		}
	}
	used := map[string]bool{}
	for pattern, d := range readRouteContract(t) {
		for _, m := range regexp.MustCompile("`([A-Z][A-Za-z]+)`").FindAllStringSubmatch(d.methods, -1) {
			if m[1] == "Authenticator" {
				continue
			}
			used[m[1]] = true
			if !strings.Contains(all, "(s *Store) "+m[1]+"(") || !strings.Contains(ui, "u.Store."+m[1]+"(") {
				t.Errorf("%s: names Store.%s, which does not exist or the UI does not call", pattern, m[1])
			}
		}
	}
	for _, m := range regexp.MustCompile(`u\.Store\.([A-Z][A-Za-z]+)\(`).FindAllStringSubmatch(ui, -1) {
		if !used[m[1]] {
			t.Errorf("the UI calls Store.%s, which no route of the contract names", m[1])
		}
		// and it is a call the API handlers make too: no second path into the service
		if api, _ := os.ReadFile("api.go"); !strings.Contains(string(api), "a.Store."+m[1]+"(") {
			t.Errorf("the UI calls Store.%s, which the API does not", m[1])
		}
	}
}

// The UI asks the Store; it never reads the database or the backend, never reads space roles to
// decide access and never checks a permission outside the guard and the presentation helpers.
func TestUISourceHasNoSecondAuthorizationPath(t *testing.T) {
	files, _ := filepath.Glob("ui*.go")
	banned := []string{"QueryContext(", "ExecContext(", "QueryRowContext(", ".db.", "sql.", "inTx(", "roleIn(", ".need(", ".getFile(", ".backend", ".seal(", ".unseal(", "space_members"}
	hasRe := regexp.MustCompile(`\.Has\(`)
	// where a permission may be checked: the guard (and what it renders), the sign-in paths and the
	// recheck interval
	allowed := map[string]int{"ui.go": 3, "ui_session.go": 2, "ui_sso.go": 1}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, bad := range banned {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q: the UI must go through the Store's methods", f, bad)
			}
		}
		if n := len(hasRe.FindAll(b, -1)); n != allowed[f] {
			t.Errorf("%s checks permissions %d times, want %d: review that it only guards routes or shows controls, then update this count", f, n, allowed[f])
		}
		if strings.Contains(string(b), "template.HTML(") || strings.Contains(string(b), "template.URL(") {
			t.Errorf("%s bypasses template escaping", f)
		}
	}
}

// ---- message catalogue -------------------------------------------------------------------------

func TestUIMessageCatalogueComplete(t *testing.T) {
	used := map[string]string{}
	prefixes := `(?:app|a11y|nav|common|login|ticket|home|space|upload|trash|members|file|share|admin|hold|erase|flash|err)\.[a-z0-9_]+`
	res := []*regexp.Regexp{regexp.MustCompile(`\bt "(` + prefixes + `)"`), regexp.MustCompile(`"(` + prefixes + `)"`)}
	ents, err := os.ReadDir("ui/templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, _ := os.ReadFile("ui/templates/" + e.Name())
		for _, m := range res[0].FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = e.Name()
		}
	}
	gofiles, _ := filepath.Glob("ui*.go")
	for _, f := range gofiles {
		if strings.HasSuffix(f, "_test.go") || f == "ui_messages.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range res[1].FindAllStringSubmatch(string(b), -1) {
			if !strings.HasSuffix(m[1], ".html") {
				used[m[1]] = f
			}
		}
	}
	for k, where := range used {
		if _, ok := uiMessagesEN[k]; !ok && k != "files.css" {
			t.Errorf("message %q used in %s is not in the catalogue", k, where)
		}
	}
	dyn := regexp.MustCompile(`^(role|scan|state|kind|flash)\.`)
	for k := range uiMessagesEN {
		if _, ok := used[k]; !ok && !dyn.MatchString(k) {
			t.Errorf("message %q is not used", k)
		}
	}
	for _, k := range flashKeys {
		if _, ok := uiMessagesEN[k]; !ok {
			t.Errorf("flash %q is not in the catalogue", k)
		}
	}
	u := &UI{}
	if u.flash("<b>") != "" || u.flash("err.internal") != "" {
		t.Error("the ok parameter can select only flash messages")
	}
}

// ---- rendered pages ----------------------------------------------------------------------------

type shot struct{ name, html string }

const hostile = `<script>alert(1)</script> & co.pdf`

// stableIDs makes new ids count up, so the golden pages (and the id order of lists) are the same
// on every run.
func stableIDs(t *testing.T) {
	t.Helper()
	old, n := idHex, 0
	idHex = func() string { n++; return fmt.Sprintf("%024x", n) }
	t.Cleanup(func() { idHex = old })
}

func renderAllPages(t *testing.T) []shot {
	t.Helper()
	stableIDs(t)
	u := newUIEnv(t, func(c *Config, _ *StoreOptions) { c.Share.BaseURL = "https://files.example.org" })
	a, b, ro, root := u.login(t, patAlice), u.login(t, patBob), u.login(t, patRO), u.login(t, patRoot)
	empty := u.login(t, patCarol)

	sp := created(t, a.post("/spaces", url.Values{"name": {"Contracts <b>2026</b>"}}))
	a.post("/spaces/"+sp+"/folders", url.Values{"name": {"Leases"}})
	fld := folderID(t, u, sp, "Leases")
	a.post("/spaces/"+sp+"/folders", url.Values{"name": {"Old"}, "parent": {fld}})
	fid := created(t, a.upload("/spaces/"+sp+"/files", "lease.pdf", doc, "folder", fld, "description", `"><script>x</script>`, "subject", "cand_golden", "ref", "jobs:application-7"))
	a.upload("/files/"+fid+"/versions", "lease.pdf", doc+"v2\n")
	// The name is sanitised of path parts and quotes but may keep angle brackets: they must be escaped.
	evil := created(t, a.upload("/spaces/"+sp+"/files", hostile, doc))
	gone := created(t, a.upload("/spaces/"+sp+"/files", "draft.pdf", doc))
	a.post("/files/"+gone+"/delete", nil)
	a.get("/files/" + fid)
	link := a.post("/files/"+fid+"/shares", url.Values{"label": {"for Dana"}, "hours": {"24"}, "one_time": {"1"}})
	a.post("/spaces/"+sp+"/members", url.Values{"kind": {"user"}, "principal": {"bob"}, "role": {"viewer"}})
	a.post("/spaces/"+sp+"/members", url.Values{"kind": {"user"}, "principal": {"ro"}, "role": {"editor"}})
	a.post("/spaces/"+sp+"/members", url.Values{"kind": {"group"}, "principal": {"grp-legal"}, "role": {"editor"}})
	root.get("/admin/erasure")
	root.post("/admin/files/"+fid+"/hold", url.Values{"hold": {"on"}, "retain_until": {"2027-01-31"}})

	var out []shot
	anon := u.client(t)
	out = append(out, shot{"login", anon.get("/login").Body.String()}, shot{"not-found-anon", anon.get("/nope").Body.String()})
	add := func(name string, c *client, path string) {
		w := c.get(path)
		if w.Code != 200 && !strings.HasPrefix(name, "err-") {
			t.Fatalf("%s %s = %d %s", name, path, w.Code, w.Body.String())
		}
		out = append(out, shot{name, w.Body.String()})
	}
	add("home", a, "/")
	add("home-empty", empty, "/")
	add("home-reader", ro, "/")
	add("space-manager", a, "/spaces/"+sp)
	add("space-viewer", b, "/spaces/"+sp)
	add("space-folder", a, "/spaces/"+sp+"?folder="+fld)
	add("upload", a, "/spaces/"+sp+"/upload?folder="+fld)
	add("members", a, "/spaces/"+sp+"/members")
	add("trash", a, "/spaces/"+sp+"/trash")
	add("file-manager", a, "/files/"+fid)
	add("file-viewer", b, "/files/"+fid)
	add("file-read-only", ro, "/files/"+fid)
	add("file-hostile", a, "/files/"+evil)
	add("file-trashed", a, "/files/"+gone)
	out = append(out, shot{"share-created", link.Body.String()})
	add("admin-files", root, "/admin/files")
	add("admin-hold", root, "/admin/files/"+fid+"/hold")
	add("erase", root, "/admin/erasure")
	add("erase-preview", root, "/admin/erasure?subject=cand_golden")
	root.post("/admin/files/"+fid+"/hold", url.Values{"hold": {"off"}, "clear_retention": {"1"}})
	erased := root.post("/admin/erasure", url.Values{"subject": {"cand_golden"}, "confirm": {"ERASE"}, "token": {patRoot}})
	out = append(out, shot{"erased", erased.Body.String()})
	add("err-not-found", a, "/files/fil_000000000000000000000000")
	add("err-forbidden", b, "/spaces/"+sp+"/upload")
	conflict := a.post("/folders/"+fld+"/delete", url.Values{"space": {sp}})
	out = append(out, shot{"err-conflict", conflict.Body.String()})
	return out
}

var reReq = regexp.MustCompile(`Request id: <code>[0-9a-f]+</code>`)

func normaliseUI(s string) string {
	s = csrfRe.ReplaceAllString(s, `name="csrf" value="CSRF"`)
	s = reReq.ReplaceAllString(s, `Request id: <code>REQID</code>`)
	return regexp.MustCompile(`/s/shr_[A-Za-z0-9._-]+`).ReplaceAllString(s, `/s/LINKTOKEN`)
}

func TestUIGoldenPages(t *testing.T) {
	for _, s := range renderAllPages(t) {
		got := normaliseUI(s.html)
		path := filepath.Join("testdata", "ui", s.name+".html")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path) //nolint:gosec // test fixture
		if err != nil {
			t.Fatalf("%s: %v (run: go test ./core/files -run TestUIGoldenPages -update)", s.name, err)
		}
		if string(want) != got {
			t.Errorf("%s differs from %s (run with -update after reviewing the change)", s.name, path)
		}
	}
}

// ---- static accessibility checklist (the finance UI's checker) ---------------------------------

var (
	reTag     = regexp.MustCompile(`(?s)<([a-zA-Z][a-zA-Z0-9]*)((?:\s+[^<>]*?)?)\s*/?>`)
	reAttr    = regexp.MustCompile(`([a-zA-Z:-]+)(?:="([^"]*)")?`)
	reHeading = regexp.MustCompile(`<h([1-6])[\s>]`)
	reStrip   = regexp.MustCompile(`(?s)<[^>]*>`)
)

type htmlTag struct {
	name  string
	attrs map[string]string
	end   int
}

func htmlTags(html string) []htmlTag {
	var out []htmlTag
	for _, m := range reTag.FindAllStringSubmatchIndex(html, -1) {
		t := htmlTag{name: strings.ToLower(html[m[2]:m[3]]), attrs: map[string]string{}, end: m[1]}
		for _, a := range reAttr.FindAllStringSubmatch(html[m[4]:m[5]], -1) {
			t.attrs[strings.ToLower(a[1])] = a[2]
		}
		out = append(out, t)
	}
	return out
}

func textAfterTag(html string, t htmlTag) string {
	rest := html[t.end:]
	i := strings.Index(rest, "</"+t.name)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(reStrip.ReplaceAllString(rest[:i], ""))
}

func a11yProblemsUI(html string) []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	ts := htmlTags(html)
	count := map[string]int{}
	ids := map[string]bool{}
	labels := map[string]bool{}
	for _, t := range ts {
		count[t.name]++
		if id, ok := t.attrs["id"]; ok {
			if ids[id] {
				add("duplicate id %q", id)
			}
			ids[id] = true
		}
		if t.name == "label" {
			labels[t.attrs["for"]] = true
		}
	}
	if !regexp.MustCompile(`(?i)^<!doctype html>`).MatchString(html) {
		add("missing doctype")
	}
	for _, t := range ts {
		if t.name == "html" && t.attrs["lang"] == "" {
			add("html has no lang")
		}
	}
	if !strings.Contains(html, `name="viewport"`) {
		add("no viewport meta")
	}
	if m := regexp.MustCompile(`<title>([^<]*)</title>`).FindStringSubmatch(html); m == nil || strings.TrimSpace(m[1]) == "" {
		add("empty title")
	}
	if count["h1"] != 1 {
		add("want exactly one h1, got %d", count["h1"])
	}
	last := 0
	for _, m := range reHeading.FindAllStringSubmatch(html, -1) {
		n, _ := strconv.Atoi(m[1])
		if last != 0 && n > last+1 {
			add("heading level skips from h%d to h%d", last, n)
		}
		last = n
	}
	if count["main"] != 1 || count["header"] < 1 || count["footer"] < 1 {
		add("landmarks: main=%d header=%d footer=%d", count["main"], count["header"], count["footer"])
	}
	if !ids["main"] || !strings.Contains(html, `href="#main"`) {
		add("no skip link to #main")
	}
	for _, t := range ts {
		for _, ref := range []string{"aria-describedby", "aria-labelledby"} {
			if v := t.attrs[ref]; v != "" && !ids[v] {
				add("%s=%q points at nothing", ref, v)
			}
		}
		switch t.name {
		case "nav":
			if t.attrs["aria-label"] == "" && t.attrs["aria-labelledby"] == "" {
				add("nav without a label")
			}
		case "input", "select", "textarea":
			ty := t.attrs["type"]
			if t.name == "input" && (ty == "hidden" || ty == "submit") {
				continue
			}
			if id := t.attrs["id"]; id == "" || !labels[id] {
				if t.attrs["aria-label"] == "" && t.attrs["aria-labelledby"] == "" {
					add("%s name=%q has no label", t.name, t.attrs["name"])
				}
			}
			if ty == "password" && t.attrs["autocomplete"] == "" {
				add("password field without autocomplete")
			}
		case "img":
			if _, ok := t.attrs["alt"]; !ok {
				add("img without alt")
			}
		case "button":
			if textAfterTag(html, t) == "" && t.attrs["aria-label"] == "" {
				add("button without a name")
			}
		case "a":
			if t.attrs["href"] == "" {
				add("anchor without href")
			} else if textAfterTag(html, t) == "" && t.attrs["aria-label"] == "" {
				add("link %q has no text", t.attrs["href"])
			}
		case "th":
			if t.attrs["scope"] == "" {
				add("th without scope")
			}
		case "table":
			rest := html[t.end:]
			if j := strings.Index(rest, "</table>"); j < 0 || !strings.Contains(rest[:j], "<caption") {
				add("table without caption")
			}
		case "form":
			if strings.EqualFold(t.attrs["method"], "post") {
				j := strings.Index(html[t.end:], "</form>")
				if j < 0 || !strings.Contains(html[t.end:t.end+j], `name="csrf"`) {
					add("post form %q without csrf field", t.attrs["action"])
				}
			}
		}
		if v, ok := t.attrs["tabindex"]; ok {
			if n, _ := strconv.Atoi(v); n > 0 {
				add("positive tabindex")
			}
		}
	}
	for l := range labels {
		if !ids[l] {
			add("label for=%q points at nothing", l)
		}
	}
	return p
}

func TestUIPagesPassAccessibilityChecklist(t *testing.T) {
	pages := renderAllPages(t)
	if len(pages) < 24 {
		t.Fatalf("only %d pages rendered", len(pages))
	}
	for _, s := range pages {
		for _, p := range a11yProblemsUI(s.html) {
			t.Errorf("%s: %s", s.name, p)
		}
	}
}

func TestUIPagesNeverLeakRawMarkupOrScript(t *testing.T) {
	for _, s := range renderAllPages(t) {
		for _, bad := range []string{"<script", "<b>2026</b>", " style=", "onclick=", "onerror=", "data:", "canpat_"} {
			if strings.Contains(s.html, bad) {
				t.Errorf("%s contains %q", s.name, bad)
			}
		}
		// the file part of every upload form is the last field, after the CSRF token
		for _, form := range regexp.MustCompile(`(?s)<form[^>]*multipart[^>]*>(.*?)</form>`).FindAllStringSubmatch(s.html, -1) {
			names := regexp.MustCompile(`<(?:input|select|textarea)[^>]* name="([a-z_]+)"`).FindAllStringSubmatch(form[1], -1)
			if len(names) < 2 || names[0][1] != "csrf" || names[len(names)-1][1] != "file" {
				t.Errorf("%s: upload form fields %v: csrf must come first and file last", s.name, names)
			}
		}
	}
}

// ---- stylesheet --------------------------------------------------------------------------------

func lum(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	var c [3]float64
	for i := 0; i < 3; i++ {
		v, _ := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		f := float64(v) / 255
		if f <= 0.03928 {
			c[i] = f / 12.92
		} else {
			c[i] = math.Pow((f+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func TestUIStylesheetContrastAndMotion(t *testing.T) {
	b, err := os.ReadFile("ui/static/files.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	pairs := regexp.MustCompile(`contrast:\s*(#[0-9a-fA-F]{6})\s+(#[0-9a-fA-F]{6})`).FindAllStringSubmatch(css, -1)
	if len(pairs) < 10 {
		t.Fatalf("only %d contrast pairs declared", len(pairs))
	}
	for _, p := range pairs {
		a, c := lum(p[1]), lum(p[2])
		if a < c {
			a, c = c, a
		}
		if r := (a + 0.05) / (c + 0.05); r < 4.5 {
			t.Errorf("contrast %s on %s is %.2f:1, want 4.5:1", p[1], p[2], r)
		}
	}
	for _, want := range []string{"prefers-reduced-motion", "prefers-color-scheme: dark", "focus-visible", "min-width", "@media print"} {
		if !strings.Contains(css, want) {
			t.Errorf("stylesheet lacks %q", want)
		}
	}
	if strings.Contains(css, "@import") || strings.Contains(css, "url(") {
		t.Error("stylesheet must not load anything")
	}
}

// Templates are parsed on first use, so a syntax error would only show on its page: parse them all.
func TestUIEveryTemplateParses(t *testing.T) {
	u := &UI{}
	u.once.Do(u.init)
	if err := u.tpl.ParseAll(); err != nil {
		t.Fatal(err)
	}
}

func TestUIHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 25 << 20: "25.0 MiB", 5 << 30: "5.0 GiB", 3 << 40: "3.0 TiB"} {
		if got := humanSize(n); got != want {
			t.Errorf("%d: %q, want %q", n, got, want)
		}
	}
}

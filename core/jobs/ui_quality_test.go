package jobs

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- message catalogue ---------------------------------------------------------------------

func TestUIMessageCatalogueComplete(t *testing.T) {
	keyRe := regexp.MustCompile(`\bt "([a-z0-9_.]+)"`)
	goRe := regexp.MustCompile(`msg\(\s*"([a-z0-9_.]+)"`)
	used := map[string]string{}
	ents, err := os.ReadDir("ui/templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, _ := os.ReadFile("ui/templates/" + e.Name())
		for _, m := range keyRe.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = e.Name()
		}
	}
	for _, f := range []string{"ui.go", "ui_pages.go", "ui_session.go"} {
		b, _ := os.ReadFile(f)
		for _, m := range goRe.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = f
		}
		for _, m := range regexp.MustCompile(`"((?:err|login|home|jobs|jobform|job|app|reveal|candidate|talent|eeo|settings|interviews)\.[a-z_]*title|err\.[a-z_]+|login\.[a-z_]+)"`).FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = f
		}
		for _, m := range regexp.MustCompile(`redirect\(c, [^,]+, "([a-z_]+)"\)`).FindAllStringSubmatch(string(b), -1) {
			used["flash."+m[1]] = f
		}
	}
	for k, where := range used {
		if strings.HasSuffix(k, ".html") {
			continue // a template file name, not a message
		}
		if _, ok := uiMessagesEN[k]; !ok {
			t.Errorf("message %q used in %s is not in the catalogue", k, where)
		}
	}
	// dynamic families
	for _, fam := range []struct {
		prefix string
		vals   []string
	}{
		{"jstatus.", []string{StatusDraft, StatusOpen, StatusPaused, StatusClosed}},
		{"jmove.", []string{StatusOpen, StatusPaused}},
		{"approval.", []string{ApprovalNone, ApprovalPending, ApprovalApproved, ApprovalRejected}},
		{"close.", CloseReasons},
		{"astatus.", []string{AppActive, AppRejected, AppWithdrawn, AppHired}},
		{"reason.", RejectReasons},
		{"scan.", []string{"clean", "skipped", "infected"}},
		{"rating.", []string{"1", "2", "3", "4"}},
		{"rec.", Recommendations},
		{"sc.", []string{"none", "draft", "submitted"}},
		{"kind.", []string{KindInitial, KindActive, KindOffer, KindHired}},
		{"eeo.field.", EEOFields},
	} {
		for _, v := range fam.vals {
			if _, ok := uiMessagesEN[fam.prefix+v]; !ok {
				t.Errorf("catalogue lacks %q", fam.prefix+v)
			}
		}
	}
	// nothing user-visible hides outside the catalogue: templates hold no bare text nodes
	textRe := regexp.MustCompile(`>([^<>{}]*[A-Za-z]{3,}[^<>{}]*)<`)
	for _, e := range ents {
		b, _ := os.ReadFile("ui/templates/" + e.Name())
		for _, m := range textRe.FindAllStringSubmatch(string(b), -1) {
			if strings.TrimSpace(m[1]) != "" {
				t.Errorf("%s has literal text %q outside the catalogue", e.Name(), strings.TrimSpace(m[1]))
			}
		}
	}
}

// ---- fixtures for the rendered-page checks -------------------------------------------------

type shot struct{ name, html string }

func renderAllPages(t *testing.T) []shot {
	t.Helper()
	u := newUIScenario(t, func(c *Config) { c.EEO.MinCell = 1 })
	u.want(u.do(u.recruiter, "PUT", "/v1/applications/"+u.a1+"/interviewers/ian", nil), 204)
	u.want(u.do(u.ian, "PUT", "/v1/applications/"+u.a1+"/scorecards/mine", map[string]any{"rating": 4, "recommendation": "strong_yes", "comments": "Great systems thinking."}), 200)
	u.want(u.do(u.ian, "POST", "/v1/applications/"+u.a1+"/scorecards/mine/submit", nil), 200)
	f := goodApplication("eeo@example.test")
	f["eeo_gender"] = "female"
	u.apply(u.slug1, f)
	u.want(u.do(u.recruiter, "PUT", "/v1/candidates/"+u.c1+"/talent-pool", map[string]any{"member": true}), 200)
	draft := u.want(u.do(u.recruiter, "POST", "/v1/jobs", jobBody("Draft role", "alice")), 201).m(t)["id"].(string)

	var out []shot
	anon := u.client(t)
	out = append(out, shot{"login", anon.get("/login").Body.String()}, shot{"not-found-anon", anon.get("/nope").Body.String()})
	u.clk.Add(40 * 24 * time.Hour) // the month of the diversity answers closes; every session starts after it
	rec := u.login(t, u.recruiter)
	hm := u.login(t, u.hmAlice)
	ivy := u.login(t, u.ivy)
	admin := u.login(t, adminTok)
	add := func(name string, c *client, path string) {
		w := c.get(path)
		if w.Code != 200 && !strings.HasPrefix(name, "err-") {
			t.Fatalf("%s %s = %d", name, path, w.Code)
		}
		out = append(out, shot{name, w.Body.String()})
	}
	add("home-recruiter", rec, "/")
	add("home-hiring-manager", hm, "/")
	add("home-interviewer", ivy, "/")
	add("jobs", rec, "/jobs")
	add("job-new", rec, "/jobs/new")
	add("job-board", rec, "/jobs/"+u.j1)
	add("job-board-hiring-manager", hm, "/jobs/"+u.j1)
	add("job-draft", rec, "/jobs/"+draft)
	add("job-edit", rec, "/jobs/"+u.j1+"/edit")
	add("application-recruiter", rec, "/applications/"+u.a1)
	add("application-hiring-manager", hm, "/applications/"+u.a1)
	add("application-interviewer-blind", ivy, "/applications/"+u.a1)
	out = append(out, shot{"reveal", rec.post("/applications/"+u.a1+"/reveal", nil).Body.String()})
	out = append(out, shot{"reveal-hiring-manager", hm.post("/applications/"+u.a1+"/reveal", nil).Body.String()})
	ivy.post("/applications/"+u.a1+"/scorecard", url.Values{"rating": {"3"}, "recommendation": {"yes"}, "comments": {"Solid."}, "action": {"submit"}})
	add("application-interviewer-released", ivy, "/applications/"+u.a1)
	add("interviews", ivy, "/interviews")
	add("talent", rec, "/talent")
	add("candidate", rec, "/candidates/"+u.c1)
	add("eeo", admin, "/reports/eeo")
	add("settings", rec, "/settings")
	add("err-not-found", hm, "/jobs/"+u.j2)
	add("err-forbidden", hm, "/talent")
	return out
}

var (
	reID  = regexp.MustCompile(`\b[a-z]{2,5}_[0-9a-f]{16}\b`)
	reReq = regexp.MustCompile(`Request id: <code>[0-9a-f]+</code>`)
)

func normaliseUI(s string) string {
	s = csrfRe.ReplaceAllString(s, `name="csrf" value="CSRF"`)
	s = reReq.ReplaceAllString(s, `Request id: <code>REQID</code>`)
	s = regexp.MustCompile(`value="[A-Za-z0-9_-]{43}"`).ReplaceAllString(s, `value="TOKEN"`)
	s = regexp.MustCompile(`careers/[a-z0-9-]+-[0-9a-f]{6}"`).ReplaceAllString(s, `careers/SLUG"`)
	return reID.ReplaceAllString(s, "ID")
}

func TestUIGoldenPages(t *testing.T) {
	for _, s := range renderAllPages(t) {
		got := normaliseUI(s.html)
		path := filepath.Join("testdata", "ui", s.name+".html")
		if *update {
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
			t.Fatalf("%s: %v (run: go test ./core/jobs -run TestUIGoldenPages -update)", s.name, err)
		}
		if string(want) != got {
			t.Errorf("%s differs from %s (run with -update after reviewing the change)", s.name, path)
		}
	}
}

// ---- static accessibility checklist (a trimmed port of the admin UI checker) -----------------

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
	if len(pages) < 20 {
		t.Fatalf("only %d pages rendered", len(pages))
	}
	for _, s := range pages {
		for _, p := range a11yProblemsUI(s.html) {
			t.Errorf("%s: %s", s.name, p)
		}
	}
}

func TestUIChecklistCatchesProblems(t *testing.T) {
	bad := `<!doctype html><html><head><title>x</title></head><body><h1>a</h1><h3>b</h3><input type="text" name="q"><button></button><table><tr><th>h</th></tr></table><form method="post"></form></body></html>`
	got := strings.Join(a11yProblemsUI(bad), "|")
	for _, want := range []string{"no lang", "skips from h1 to h3", "has no label", "button without a name", "th without scope", "table without caption", "without csrf", "no viewport", "landmarks", "skip link"} {
		if !strings.Contains(got, want) {
			t.Errorf("checker missed %q in %q", want, got)
		}
	}
}

// ---- stylesheet contrast, reduced motion, no inline code ------------------------------------

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
	b, err := os.ReadFile("ui/static/jobs.css")
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
	for _, want := range []string{"prefers-reduced-motion", "prefers-color-scheme: dark", "focus-visible", "min-width"} {
		if !strings.Contains(css, want) {
			t.Errorf("stylesheet lacks %q", want)
		}
	}
	if strings.Contains(css, "@import") || strings.Contains(css, "url(") {
		t.Error("stylesheet must not load anything: no @import, no url() (so no data: URLs either)")
	}
}

// ---- one authorisation path ------------------------------------------------------------------

func TestUISourceHasNoSecondAuthorizationPath(t *testing.T) {
	files, _ := filepath.Glob("ui*.go")
	banned := regexp.MustCompile(`\.Can\(|\.CanJob\(|\.Unscoped\(|\.canSee\(|jobPred|appPred|candidatePred|Role\s*[!=]=|\bRole(Admin|Recruiter|HiringManager|Interviewer)\b|\.db\b|\.rdb\b|QueryContext|QueryRowContext|ExecContext|SELECT |INSERT |UPDATE |DELETE FROM|template\.HTML|template\.JS|template\.URL`)
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		n++
		b, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := banned.FindString(line); m != "" {
				t.Errorf("%s:%d uses %q: the UI must call Store methods and decide nothing itself", f, i+1, m)
			}
		}
	}
	if n < 4 {
		t.Errorf("only %d ui files scanned", n)
	}
}

func TestUITemplatesHaveNoInlineCodeOrDataURLs(t *testing.T) {
	ents, _ := os.ReadDir("ui/templates")
	for _, e := range ents {
		b, _ := os.ReadFile("ui/templates/" + e.Name())
		s := strings.ToLower(string(b))
		for _, bad := range []string{"<script", "style=", "javascript:", "data:", " onclick", " onload", "<iframe", "<object", "<embed"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", e.Name(), bad)
			}
		}
	}
}

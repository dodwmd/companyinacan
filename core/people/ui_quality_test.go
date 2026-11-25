package people

import (
	"encoding/json"
	"flag"
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

var updateGolden = flag.Bool("update", false, "rewrite the golden HTML files of the people UI")

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
		for _, m := range regexp.MustCompile(`(?:Title|Key|titleKey|msgKey)?"((?:err|login|flash)\.[a-z_]+)"`).FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = f
		}
	}
	for k, where := range used {
		if strings.HasSuffix(k, ".") || strings.HasSuffix(k, ".html") {
			continue // dynamic family prefix (checked below) or a template file name
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
		{"status.", []string{"candidate", "onboarding", "active", "leave", "offboarding", "former"}},
		{"sens.", SensitiveFields},
		{"cadence.", []string{"weekly", "biweekly", "monthly"}},
		{"mstatus.", []string{"scheduled", "held", "cancelled"}},
		{"scope.", []string{"person", "team", "company"}},
		{"gstatus.", goalStatuses},
		{"vis.", goalVisibilities},
		{"goals.", []string{"team", "company"}},
	} {
		for _, v := range fam.vals {
			if _, ok := uiMessagesEN[fam.prefix+v]; !ok {
				t.Errorf("catalogue lacks %q", fam.prefix+v)
			}
		}
	}
	// nothing user-visible hides outside the catalogue: templates hold no bare text nodes
	// longer than punctuation between tags or actions.
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
	u := newUIEnv(t)
	u.ui.NotificationsURL = "http://admin.localhost/notifications"
	u.ui.Approvals = fakeApprovals{{Title: "Laptop request", URL: "http://admin.localhost/approvals/7"}}
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := u.person("Ann", boss.ID, RoleEmployee)
	u.must(u.s.PutSensitive(u.ctx, u.hr, ann.ID, map[string]json.RawMessage{FieldDOB: raw("1990-05-17"), FieldSalary: raw(Salary{Amount: 9000000, Currency: "USD", Period: "year"})}))
	tpl, err := u.s.CreateTemplate(u.ctx, u.hr, TemplateInput{Name: "Onboarding", Kind: "onboarding", Items: []TemplateItem{{Title: "Read the handbook", AssigneeRole: "self", DueOffsetDays: 3}}})
	u.must(err)
	_, err = u.s.CreateChecklist(u.ctx, u.hr, ann.ID, tpl.ID, "2026-03-01")
	u.must(err)
	ser, err := u.s.CreateSeries(u.ctx, bossA, ann.ID, "weekly")
	u.must(err)
	m, err := u.s.ScheduleMeeting(u.ctx, bossA, ser.ID, u.now.Add(48*time.Hour))
	u.must(err)
	u.must(u.s.SetNotes(u.ctx, bossA, m.ID, "private thoughts", true))
	_, err = u.s.AddAction(u.ctx, bossA, m.ID, "Send the doc", ann.ID, "2026-04-01")
	u.must(err)
	g, err := u.s.CreateGoal(u.ctx, annA, GoalInput{Scope: "person", Title: "Ship it", Visibility: "manager", Period: "2026-Q1"})
	u.must(err)
	_, err = u.s.AddKeyResult(u.ctx, annA, g.ID, KeyResultInput{Title: "Pages", Unit: "pages", Start: 0, Target: 10})
	u.must(err)

	_, benA := u.person("Ben", boss.ID, RoleEmployee)
	team, err := u.s.CreateOrgUnit(u.ctx, u.hr, "teams", OrgInput{Name: "Core"})
	u.must(err)
	for _, id := range []string{boss.ID, ann.ID} {
		_, err = u.s.UpdatePerson(u.ctx, u.hr, id, PersonPatch{TeamID: &team.ID})
		u.must(err)
	}
	vac, err := u.s.CreateLeaveType(u.ctx, u.hr, LeaveTypeIn{Key: "vacation", Name: "Vacation", Paid: true, AccrualMode: "annual", GrantHalves: 50})
	u.must(err)
	_, err = u.s.RunAccruals(u.ctx, u.hr)
	u.must(err)
	lr, err := u.s.CreateLeaveRequest(u.ctx, annA, LeaveRequestIn{TypeID: vac.ID, StartDate: "2026-03-23", EndDate: "2026-03-24", StartPart: "pm", EndPart: "full", Note: "family trip"})
	u.must(err)
	_, err = u.s.SubmitLeaveRequest(u.ctx, annA, lr.ID)
	u.must(err)
	rtpl, err := u.s.CreateReviewTemplate(u.ctx, u.hr, TemplateIn{Name: "annual", Questions: testQuestions})
	u.must(err)
	cyc, err := u.s.CreateCycle(u.ctx, u.hr, CycleIn{Name: "2026 annual", TemplateID: rtpl.ID, Anonymity: "anonymous"})
	u.must(err)
	rids, err := u.s.AddParticipants(u.ctx, u.hr, cyc.ID, []string{ann.ID})
	u.must(err)
	_, err = u.s.SetCycleState(u.ctx, u.hr, cyc.ID, "open")
	u.must(err)
	rid := rids[0]
	_, err = u.s.Nominate(u.ctx, bossA, rid, benA.PersonID, "peer")
	u.must(err)

	var out []shot
	anon := u.client(t)
	out = append(out, shot{"login", anon.get("/login").Body.String()}, shot{"not-found-anon", anon.get("/nope").Body.String()})
	cb := u.login(t, bossA)
	ca := u.login(t, annA)
	add := func(name string, c *client, path string) {
		w := c.get(path)
		if w.Code != 200 && !strings.HasPrefix(name, "err-") {
			t.Fatalf("%s %s = %d", name, path, w.Code)
		}
		out = append(out, shot{name, w.Body.String()})
	}
	add("home-manager", cb, "/")
	add("home-employee", ca, "/")
	add("profile", ca, "/me")
	add("sensitive-notice", ca, "/me/sensitive")
	out = append(out, shot{"sensitive-revealed", ca.post("/me/sensitive", nil).Body.String()})
	add("directory", ca, "/people?q=a")
	add("directory-empty", ca, "/people?q=zzzz")
	add("person", cb, "/people/"+ann.ID)
	add("org", ca, "/org")
	add("team", cb, "/team")
	add("oneonones", ca, "/oneonones")
	add("series", cb, "/oneonones/"+ser.ID)
	add("meeting-manager", cb, "/meetings/"+m.ID)
	add("meeting-report", ca, "/meetings/"+m.ID)
	add("goals", ca, "/goals")
	add("goal", ca, "/goals/"+g.ID)
	add("timeoff", ca, "/timeoff")
	add("timeoff-calendar", ca, "/timeoff/calendar?from=2026-03-09&days=28")
	add("timeoff-queue", cb, "/timeoff/queue")
	add("reviews", ca, "/reviews")
	add("review-self-form", ca, "/reviews/"+rid)
	add("review-manager-form", cb, "/reviews/"+rid)
	add("feedback-inbox", u.login(t, benA), "/feedback")
	add("err-not-found", ca, "/people/bogus")
	return out
}

var (
	reID  = regexp.MustCompile(`\b[a-z]{2,5}_[0-9a-f]{24}\b`)
	reReq = regexp.MustCompile(`Request id: <code>[0-9a-f]+</code>`)
)

func normaliseUI(s string) string {
	s = csrfRe.ReplaceAllString(s, `name="csrf" value="CSRF"`)
	s = reReq.ReplaceAllString(s, `Request id: <code>REQID</code>`)
	s = regexp.MustCompile(`value="[A-Za-z0-9_-]{43}"`).ReplaceAllString(s, `value="TOKEN"`)
	return reID.ReplaceAllString(s, "ID")
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
			t.Fatalf("%s: %v (run: go test ./core/people -run TestUIGoldenPages -update)", s.name, err)
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
	if len(pages) < 15 {
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
	b, err := os.ReadFile("ui/static/people.css")
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
	if strings.Contains(css, "@import") || strings.Contains(css, "url(http") {
		t.Error("stylesheet must not load anything remote")
	}
}

// ---- personal access tokens keep their permissions through the UI ---------------------------

func TestUIRestrictedTokenKeepsItsPermissions(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := u.person("Ann", boss.ID, RoleEmployee)
	ser, err := u.s.CreateSeries(u.ctx, bossA, ann.ID, "weekly")
	u.must(err)
	m, err := u.s.ScheduleMeeting(u.ctx, bossA, ser.ID, u.now.Add(24*time.Hour))
	u.must(err)
	// a token that only carries people.read: the roles say employee, the permissions say no 1:1s
	u.st.Add("pat-restricted-0123456789abcdef", Principal{Subject: annA.Subject, Roles: []string{RoleEmployee}, Perms: []string{"people.read"}})
	c := u.client(t)
	csrf := csrfRe.FindStringSubmatch(c.get("/login").Body.String())[1]
	if w := c.do("POST", "/login", url.Values{"token": {"pat-restricted-0123456789abcdef"}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin"); w.Code != 303 {
		t.Fatalf("login = %d", w.Code)
	}
	if w := c.get("/meetings/" + m.ID); w.Code != 404 {
		t.Fatalf("restricted token read a 1:1 = %d", w.Code)
	}
	if w := c.get("/people/" + boss.ID); w.Code != 200 {
		t.Fatalf("restricted token could not read the directory = %d", w.Code)
	}
}

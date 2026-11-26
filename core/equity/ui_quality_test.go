package equity

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

func uiSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("ui*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			b, err := os.ReadFile(f) //nolint:gosec // the package's own sources
			if err != nil {
				t.Fatal(err)
			}
			out[f] = string(b)
		}
	}
	return out
}

// The UI asks the Service: it never reads the database, never asks the policy itself and never
// inspects a role to allow something.
func TestUISourceHasNoSecondAuthorizationPath(t *testing.T) {
	banned := []string{"Decide(", "FieldAccess(", "sodOK(", ".authorize(", "selfFact(", "visibleTo(", "QueryContext(", "ExecContext(", "QueryRowContext(",
		"Svc.db", "sql.", ".tx(", "txAudit(", "loadGrant(", "loadExercise(", "openIdentity(", ".keys"}
	for f, src := range uiSources(t) {
		for _, bad := range banned {
			if strings.Contains(src, bad) {
				t.Errorf("%s contains %q: the UI goes through the Service", f, bad)
			}
		}
		// roles are looked at only to say who may hold a session and how often to ask again
		if f != "ui_session.go" && regexp.MustCompile(`\.Roles\b|\bRole(Employee|Manager|HR|Finance|Board|Admin)\b`).MatchString(src) {
			t.Errorf("%s inspects roles", f)
		}
		if strings.Contains(src, "float") && f != "ui_format.go" {
			t.Errorf("%s mentions floats: money and share figures are exact integers", f)
		}
	}
}

func templateFiles(t *testing.T) map[string]string {
	t.Helper()
	ents, err := fs.ReadDir(uiAssets, "ui/templates")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range ents {
		b, err := uiAssets.ReadFile("ui/templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func TestUIMessageCatalogueComplete(t *testing.T) {
	used := map[string]string{}
	need := func(key, where string) { used[key] = where }
	tpls := templateFiles(t)
	for name, src := range tpls {
		for _, m := range regexp.MustCompile(`\bt "([A-Za-z0-9_.]+)"`).FindAllStringSubmatch(src, -1) {
			need(m[1], name)
		}
		// nothing user-visible hides outside the catalogue
		for _, m := range regexp.MustCompile(`>([^<>{}]*[A-Za-z]{3,}[^<>{}]*)<`).FindAllStringSubmatch(src, -1) {
			if strings.TrimSpace(m[1]) != "" {
				t.Errorf("%s has literal text %q outside the catalogue", name, strings.TrimSpace(m[1]))
			}
		}
	}
	for f, src := range uiSources(t) {
		if f == "ui_messages.go" {
			continue
		}
		for _, m := range regexp.MustCompile(`"((?:err|login|flash|stepup|sched|ticket|nav|ex|home|mine|docs|grant|holder|cap|export|company|identity)\.[a-z_]+)"`).FindAllStringSubmatch(src, -1) {
			need(m[1], f)
		}
	}
	for _, k := range session.MessageKeys {
		need(k, "core/uikit/session")
	}
	families := map[string][]string{
		"gstatus": grantStatuses, "gnext": grantStatuses, "xstatus": exerciseStatuses, "xnext": exerciseStatuses,
		"kind": {"founder", "employee", "investor", "advisor"}, "classkind": {"common", "preferred"},
		"reason": {"cliff", "instalment", "tranche", "milestone", "acceleration"}, "interval": {"monthly", "quarterly"},
		"rounding": {"floor", "nearest", "fractional"}, "review": {ReviewUnconfirmedLink, ReviewUnattested, ReviewErasedHolder},
		"gtype": {"ISO", "NSO", "RSU", "SAR"},
	}
	for fam, vals := range families {
		for _, v := range vals {
			need(fam+"."+v, "family "+fam)
		}
	}
	for k, where := range used {
		if strings.HasSuffix(k, ".html") {
			continue
		}
		if _, ok := uiMessagesEN[k]; !ok {
			t.Errorf("message %q used in %s is not in the catalogue", k, where)
		}
	}
	for k := range uiMessagesEN {
		if _, ok := used[k]; !ok && k != "app.name" {
			t.Errorf("catalogue entry %q is not used", k)
		}
	}
}

// renderAllPages walks the pages as the people who use them.
func renderAllPages(t *testing.T) map[string]string {
	t.Helper()
	u := newUIEnv(t)
	u.base()
	active := u.active(u.eveStk.ID, q(4800))
	pending := u.draft(u.hr, u.eveStk.ID, q(100))
	m(u.s.SubmitGrant(u.ctx, u.hr, pending))
	approved := u.approved(u.founder.ID, q(10))
	draft := u.draft(u.hr, u.founder.ID, q(20))
	unconfirmed := m(u.s.CreateStakeholder(u.ctx, u.hr, u.ent.ID, StakeholderInput{Kind: "advisor", Identity: Identity{Name: "Ada <script>alert(1)</script>"}}))
	u.advance("2027-07-15")
	ex, _, err := u.s.RequestExercise(u.ctx, u.eve, active, q(10), "", nil)
	u.must(err)
	paid, _, err := u.s.RequestExercise(u.ctx, u.eve, active, q(10), "", nil)
	u.must(err)
	m(u.s.ApproveExercise(u.ctx, u.fin, paid))
	approvedEx, _, err := u.s.RequestExercise(u.ctx, u.eve, active, q(10), "", nil)
	u.must(err)
	m(u.s.ApproveExercise(u.ctx, u.fin, approvedEx))
	m(u.s.RecordPayment(u.ctx, u.fin, paid, PaymentInput{AmountMinor: 1250, PaidOn: "2027-07-15"}))

	out := map[string]string{}
	anon := u.client(t)
	out["login"] = anon.get("/login", 200)
	out["not-found-anon"] = anon.get("/nope", 404)
	var ticket strings.Builder
	tpl, err := u.ui.tpl.Get("ticket.html")
	u.must(err)
	u.must(tpl.ExecuteTemplate(&ticket, "layout", page{Lang: "en", Title: "Continue", Data: session.TicketView{CSRF: "c", Ticket: "t", Name: "Ann", Account: "user:ann", Subject: "user:ann", Other: "user:bob"}}))
	out["ticket"] = ticket.String()

	eve, hr, fin, board := u.login(t, u.eve), u.login(t, u.hr), u.login(t, u.fin), u.login(t, u.board2)
	e := "/company/entities/" + u.ent.ID
	for name, p := range map[string]struct {
		c    *client
		path string
	}{
		"home-employee": {eve, "/"}, "home-hr": {hr, "/"}, "me": {eve, "/me"}, "documents": {eve, "/me/documents"},
		"mygrant": {eve, "/me/grants/" + active}, "statement": {eve, "/me/grants/" + active + "/statement"},
		"companies": {hr, "/company"}, "company": {fin, e}, "captable": {fin, e + "/captable"}, "captable-basic": {fin, e + "/captable?view=basic&names=no"},
		"exports": {fin, e + "/exports"}, "stakeholders": {hr, e + "/stakeholders"}, "stakeholder": {hr, "/company/stakeholders/" + unconfirmed.ID},
		"identity": {hr, "/company/identity"}, "grants": {hr, e + "/grants"}, "grantnew": {hr, e + "/grants/new"},
		"grant-draft": {hr, "/company/grants/" + draft}, "grant-pending": {board, "/company/grants/" + pending},
		"grant-approved": {hr, "/company/grants/" + approved}, "grant-active": {hr, "/company/grants/" + active},
		"exercises": {fin, e + "/exercises"}, "exercise-requested": {fin, "/company/exercises/" + ex},
		"exercise-approved": {fin, "/company/exercises/" + approvedEx}, "exercise-paid": {fin, "/company/exercises/" + paid},
	} {
		out[name] = p.c.get(p.path, 200)
	}
	out["review"] = eve.post("/me/grants/"+active+"/exercise/review", 200, "quantity", "1", "idem", "i").Body.String()
	out["deleted"] = hr.confirm("/company/grants/"+draft+"/delete", 200).Body.String()
	out["err-not-found"] = eve.get("/me/grants/bogus", 404)
	out["grantnew-error"] = hr.post(e+"/grants", 400, "plan_id", u.plan.ID, "stakeholder_id", u.eveStk.ID, "type", "NSO", "quantity", "x").Body.String()
	return out
}

var (
	reTag     = regexp.MustCompile(`(?s)<([a-zA-Z][a-zA-Z0-9]*)((?:\s+[^<>]*?)?)\s*/?>`)
	reAttr    = regexp.MustCompile(`([a-zA-Z:-]+)(?:="([^"]*)")?`)
	reHeading = regexp.MustCompile(`<h([1-6])[\s>]`)
	reStrip   = regexp.MustCompile(`(?s)<[^>]*>`)
)

// a11yProblems is a static checklist over one page, with the rules of the strict CSP.
func a11yProblems(html string) []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	type tag struct {
		name  string
		attrs map[string]string
		end   int
	}
	var tags []tag
	count, ids, labels := map[string]int{}, map[string]bool{}, map[string]bool{}
	for _, m := range reTag.FindAllStringSubmatchIndex(html, -1) {
		x := tag{name: strings.ToLower(html[m[2]:m[3]]), attrs: map[string]string{}, end: m[1]}
		for _, a := range reAttr.FindAllStringSubmatch(html[m[4]:m[5]], -1) {
			x.attrs[strings.ToLower(a[1])] = a[2]
		}
		tags = append(tags, x)
		count[x.name]++
		if id, ok := x.attrs["id"]; ok {
			if ids[id] {
				add("duplicate id %q", id)
			}
			ids[id] = true
		}
		if x.name == "label" {
			labels[x.attrs["for"]] = true
		}
	}
	text := func(x tag) string {
		rest := html[x.end:]
		i := strings.Index(rest, "</"+x.name)
		if i < 0 {
			return ""
		}
		return strings.TrimSpace(reStrip.ReplaceAllString(rest[:i], ""))
	}
	if !strings.HasPrefix(strings.ToLower(html), "<!doctype html>") || !strings.Contains(html, `<html lang="`) || !strings.Contains(html, `name="viewport"`) {
		add("doctype, lang or viewport missing")
	}
	if count["h1"] != 1 || count["main"] != 1 || count["header"] != 1 || count["footer"] != 1 || !strings.Contains(html, `href="#main"`) {
		add("want one h1, main, header, footer and a skip link; h1=%d main=%d", count["h1"], count["main"])
	}
	if count["script"] > 0 || count["style"] > 0 {
		add("script or style element")
	}
	last := 0
	for _, m := range reHeading.FindAllStringSubmatch(html, -1) {
		n, _ := strconv.Atoi(m[1])
		if last != 0 && n > last+1 {
			add("heading level skips from h%d to h%d", last, n)
		}
		last = n
	}
	for _, x := range tags {
		if x.attrs["style"] != "" {
			add("inline style on %s", x.name)
		}
		switch x.name {
		case "nav":
			if x.attrs["aria-label"] == "" {
				add("nav without a label")
			}
		case "input", "select", "textarea":
			if x.attrs["type"] == "hidden" {
				continue
			}
			if id := x.attrs["id"]; id == "" || !labels[id] {
				add("%s name=%q has no label", x.name, x.attrs["name"])
			}
		case "button":
			if text(x) == "" {
				add("button without a name")
			}
		case "a":
			if x.attrs["href"] == "" || text(x) == "" {
				add("link %q without target or text", x.attrs["href"])
			}
		case "th":
			if x.attrs["scope"] == "" {
				add("th without scope")
			}
		case "table":
			rest := html[x.end:]
			if j := strings.Index(rest, "</table>"); j < 0 || !strings.Contains(rest[:j], "<caption") {
				add("table without caption")
			}
		case "form":
			if x.attrs["method"] == "post" {
				j := strings.Index(html[x.end:], "</form>")
				if j < 0 || !strings.Contains(html[x.end:x.end+j], `name="csrf"`) {
					add("post form %q without csrf", x.attrs["action"])
				}
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
	names := make([]string, 0, len(pages))
	for n := range pages {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		for _, p := range a11yProblems(pages[n]) {
			t.Errorf("%s: %s", n, p)
		}
		if strings.Contains(pages[n], "<script>alert") {
			t.Errorf("%s: hostile text was not escaped", n)
		}
		if k := regexp.MustCompile(`>\s*(gstatus|xstatus|gnext|xnext|err|flash|kind|reason)\.[a-z_]+`).FindString(pages[n]); k != "" {
			t.Errorf("%s: a catalogue key shows: %s", n, k)
		}
	}
}

func TestUIEveryTemplateParses(t *testing.T) {
	u := &UI{}
	u.once.Do(u.init)
	if err := u.tpl.ParseAll(); err != nil {
		t.Fatal(err)
	}
}

func TestUIChecklistCatchesProblems(t *testing.T) {
	bad := `<!doctype html><html><head><title>x</title></head><body><h1>a</h1><h3>b</h3><input type="text" name="q"><button></button><table><tr><th>h</th></tr></table><form method="post"></form><p style="x">y</p></body></html>`
	got := strings.Join(a11yProblems(bad), "|")
	for _, want := range []string{"viewport", "skips from h1 to h3", "has no label", "button without a name", "th without scope", "table without caption", "without csrf", "skip link", "inline style"} {
		if !strings.Contains(got, want) {
			t.Errorf("checker missed %q in %q", want, got)
		}
	}
}

func lum(hex string) float64 {
	var c [3]float64
	for i := range 3 {
		v, _ := strconv.ParseUint(hex[1+i*2:3+i*2], 16, 8)
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
	b, err := uiAssets.ReadFile("ui/static/equity.css")
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

// The route table of specs/066-equity-ui/contracts/ui-routes.md is the contract.
func TestUIRoutesMatchContract(t *testing.T) {
	b, err := os.ReadFile("../../specs/066-equity-ui/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `((?:GET|POST) [^`]+)` \\| (yes|no) \\| ([^|]+) \\| (yes|no) \\|")
	doc := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(string(b), -1) {
		doc[m[1]] = m[4] == "yes"
	}
	served := UIRoutes()
	for _, r := range served {
		if _, ok := doc[r]; !ok {
			t.Errorf("route %q is served but not in the contract", r)
		}
	}
	sort.Strings(served)
	for r := range doc {
		if !slices.Contains(served, r) {
			t.Errorf("contract lists %q but the UI does not serve it", r)
		}
	}
}

// Every route the contract marks as stepped up refuses a post that is not confirmed.
func TestUIStepUpRoutesRefuseUnconfirmedPosts(t *testing.T) {
	b, err := os.ReadFile("../../specs/066-equity-ui/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	u := newUIEnv(t)
	u.base()
	admin := u.login(t, Actor{Subject: "sso-all", Roles: []string{RoleHR, RoleFinance, RoleBoard, RoleAdmin}, PersonID: personMax})
	n := 0
	for _, m := range regexp.MustCompile("(?m)^\\| `POST ([^`]+)` \\| yes \\| [^|]+ \\| yes \\|").FindAllStringSubmatch(string(b), -1) {
		n++
		path := regexp.MustCompile(`\{id\}`).ReplaceAllString(m[1], "grt_000000000000000000000000")
		if strings.Contains(path, "/stakeholders/") {
			path = strings.Replace(path, "grt_", "stk_", 1)
		}
		want := 400
		if strings.Contains(path, "/payment") {
			continue // the exercise is looked up before the step-up: covered by TestUIGrantAndExerciseChain
		}
		admin.post(path, want, "token", admin.tok)                  // not ticked
		admin.post(path, want, "confirm", "yes")                    // no token
		admin.post(path, want, "confirm", "yes", "token", "wrong1") // another token
	}
	if n < 7 {
		t.Fatalf("only %d stepped-up routes in the contract", n)
	}
}

package finance

import (
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
)

var updateGolden = flag.Bool("update", false, "rewrite the golden HTML files of the finance UI")

// ---- message catalogue ---------------------------------------------------------------------------

func TestUIMessageCatalogueComplete(t *testing.T) {
	keyRe := regexp.MustCompile(`\bt "([a-z0-9_.]+)"`)
	stRe := regexp.MustCompile(`\bst "([a-z]+)" (?:"([a-z_]+)"|\.|\$)`)
	goRe := regexp.MustCompile(`(?:msg|msgOr)\(\s*"([a-z0-9_.]+)"`)
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
		for _, m := range stRe.FindAllStringSubmatch(string(b), -1) {
			if m[2] != "" {
				used[m[1]+"."+m[2]] = e.Name()
			}
		}
	}
	gofiles, _ := filepath.Glob("ui*.go")
	for _, f := range gofiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range goRe.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = f
		}
		for _, m := range regexp.MustCompile(`"((?:err|login|flash|nav|party|invoice|ledger|bank)\.[a-z_]+)"`).FindAllStringSubmatch(string(b), -1) {
			if strings.HasSuffix(m[1], ".html") {
				continue // a template file name, not a message
			}
			used[m[1]] = f
		}
	}
	for k, where := range used {
		if _, ok := uiMessagesEN[k]; !ok {
			t.Errorf("message %q used in %s is not in the catalogue", k, where)
		}
	}
	// dynamic families: statuses and kinds come from data
	for _, fam := range []struct {
		prefix string
		vals   []string
	}{
		{"inv.", invoiceStatuses}, {"ts.", timesheetStatuses}, {"ex.", expenseStatuses}, {"ct.", contractStatuses}, {"po.", poStatuses},
		{"rec.", []string{"active", "paused", "ended"}}, {"interval.", []string{"weekly", "monthly", "quarterly", "yearly"}},
		{"kind.", []string{"customer", "vendor", "both", "invoice", "credit_note"}},
		{"account.", accountTypes},
		{"dm.", documentMailStatuses}, {"dmk.", []string{DocDocument, DocStatement, DocReminder}}, {"hold.", holdStates},
		{"env.", []string{EnvPending, EnvCompleted, EnvDeclined, EnvRevoked}}, {"role.", []string{RoleCounterparty, RoleCompany}},
		{"signer.", []string{SignerWaiting, SignerInvited, SignerSigned, SignerDeclined}}, {"mailst.", []string{MailPending, MailQueued, MailNotAllowed, MailFailed}}, {"jsource.", journalSources}, {"bank.s.", []string{"open", "ready", "reconciled"}},
	} {
		for _, v := range fam.vals {
			if _, ok := uiMessagesEN[fam.prefix+v]; !ok {
				t.Errorf("catalogue lacks %q", fam.prefix+v)
			}
		}
	}
	// every catalogue entry is used somewhere (no dead text), except the dynamic families
	dyn := regexp.MustCompile(`^(inv|ts|ex|ct|po|rec|interval|kind|account|jsource|env|role|signer|mailst|bank\.s|dm|dmk|hold)\.`)
	for k := range uiMessagesEN {
		if _, ok := used[k]; !ok && !dyn.MatchString(k) && !strings.HasPrefix(k, "flash.") && !strings.HasPrefix(k, "err.") {
			t.Errorf("catalogue entry %q is not used", k)
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

// ---- no second authorisation path ------------------------------------------------------------------

// The UI asks the Store; it never reads the database, never decides an approval and never inspects a
// permission to allow something (the one scope check per route is allowedBy, shared with the API).
func TestUISourceHasNoSecondAuthorizationPath(t *testing.T) {
	files, _ := filepath.Glob("ui*.go")
	banned := []string{"authorizeApproval(", ".CanApprove(", "QueryContext(", "ExecContext(", "QueryRowContext(", ".db.", ".rdb.", "sql.", "inTx(", ".approver"}
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
		// permission checks other than the route table and the presentation capabilities
		for i, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, ".Can(") && f != "ui.go" && f != "ui_session.go" {
				t.Errorf("%s:%d checks a permission outside the route table: %s", f, i+1, strings.TrimSpace(l))
			}
		}
	}
	if b, _ := os.ReadFile("ui.go"); strings.Count(string(b), "allowedBy(") < 2 {
		t.Error("the route table must be enforced through allowedBy")
	}
	// the API's guard uses the same function
	if b, _ := os.ReadFile("api.go"); !strings.Contains(string(b), "allowedBy(") {
		t.Error("the API guard must use allowedBy so the two cannot drift")
	}
}

// ---- fixtures for the rendered-page checks --------------------------------------------------------

type shot struct{ name, html string }

func renderAllPages(t *testing.T) []shot {
	t.Helper()
	u := newUIEnv(t, func(c *Config, _ *Options) { c.UI.NotificationsURL = "http://admin.localhost/notifications" })
	maker := u.login(t, u.token("user:maker", []string{"finance.write", "finance.approve"}, nil, nil, false))
	boss := u.login(t, u.token("user:boss", []string{"finance.approve"}, nil, nil, false))
	ro := u.login(t, u.token("user:ro", []string{"finance.read"}, nil, nil, false))
	admTok := u.token("user:root", []string{"finance.admin"}, nil, nil, false)
	adm := u.login(t, admTok)
	emp := u.login(t, u.token("user:emp", []string{"finance.write", "finance.read"}, nil, nil, true))

	loc := func(w *client, path string, form url.Values, prefix string) string {
		r := w.post(path, form)
		if r.Code != 303 {
			t.Fatalf("POST %s = %d %s", path, r.Code, r.Body.String())
		}
		return locID(t, r.Header().Get("Location"), prefix)
	}
	cust := loc(maker, "/parties", url.Values{"kind": {"customer"}, "name": {"Acme Pty"}, "contact_name": {"Ann Example"}, "email": {"ap@acme.example"}, "address": {"1 Fake Rd"}, "tax_id": {"TAX-1"}, "currency": {"USD"}}, "/parties/")
	vendor := loc(maker, "/parties", url.Values{"kind": {"vendor"}, "name": {"Parts Ltd"}, "currency": {"USD"}}, "/parties/")
	jp := loc(maker, "/parties", url.Values{"kind": {"customer"}, "name": {"Tokyo KK"}, "currency": {"JPY"}}, "/parties/")
	evil := loc(maker, "/parties", url.Values{"kind": {"customer"}, "name": {`<script>alert(1)</script> "quoted" & co`}, "currency": {"USD"}}, "/parties/")

	draft := loc(maker, "/invoices", invoiceValues(cust, "Consulting", "100.00"), "/invoices/")
	issued := loc(maker, "/invoices", invoiceValues(cust, "Hosting", "49.50"), "/invoices/")
	loc(boss, "/invoices/"+issued+"/approve", nil, "/invoices/")
	loc(maker, "/invoices/"+issued+"/send", nil, "/invoices/")
	loc(maker, "/invoices/"+issued+"/payments", url.Values{"amount": {"40.00"}, "method": {"bank"}, "reference": {"ref-1"}, "received_on": {"2026-11-24"}, "idem": {"pay-golden-0001"}}, "/invoices/")
	yen := url.Values{"party_id": {jp}, "currency": {"JPY"}, "idem": {"idem-yen-0001"}, "fx_rate": {"0.0067"}, "desc": {"Widgets", "", ""}, "qty": {"3", "", ""}, "price": {"1500", "", ""}, "disc": {"", "", ""}, "tax": {"", "", ""}, "sku": {"", "", ""}}
	yenID := loc(maker, "/invoices", yen, "/invoices/")
	hostile := loc(maker, "/invoices", url.Values{"party_id": {evil}, "currency": {"USD"}, "idem": {"idem-evil-0001"}, "notes": {`"><script>x</script>`}, "desc": {`<img src=x onerror=1>`}, "qty": {"1"}, "price": {"1.00"}, "disc": {""}, "tax": {""}, "sku": {""}}, "/invoices/")
	cnID := loc(maker, "/invoices/"+issued+"/credit-note", url.Values{"reason": {"Goodwill"}, "idem": {"cn-golden-0001"}, "desc": {"Credit"}, "qty": {"1"}, "price": {"5.00"}, "disc": {""}, "tax": {""}, "sku": {""}}, "/invoices/")
	loc(maker, "/recurring", url.Values{"party_id": {cust}, "name": {"Retainer"}, "currency": {"USD"}, "interval": {"monthly"}, "start_date": {"2026-12-01"}, "due_days": {"14"}, "desc": {"Retainer"}, "qty": {"1"}, "price": {"500.00"}, "disc": {""}, "tax": {""}, "sku": {""}}, "/recurring")

	var tmpl ContractTemplate
	u.must(201, adminTok, "POST", "/v1/contract-templates", obj{"name": "NDA", "body": "Agreement with {{party}} until {{until}}."}, &tmpl)
	ctID := loc(maker, "/contracts", url.Values{"party_id": {cust}, "title": {"Mutual NDA"}, "template_id": {fmt.Sprint(tmpl.ID)}, "var_party": {"Acme Pty"}, "var_until": {"2027-01-01"}, "renewal_date": {"2026-12-15"}}, "/contracts/")

	tsID := loc(emp, "/timesheets", url.Values{"week_start": {"2026-11-23"}}, "/timesheets/")
	form := url.Values{"version": {"1"}, "project": {"ACME", ""}, "task": {"ACME-1", ""}, "desc": {"Build", ""}, "billable": {"1", "1"}}
	for d := 0; d < 7; d++ {
		v := ""
		if d < 5 {
			v = "7:30"
		}
		form.Add("h", v)
	}
	for d := 0; d < 7; d++ {
		form.Add("h", "")
	}
	loc(emp, "/timesheets/"+tsID+"/entries", form, "/timesheets/")
	loc(emp, "/timesheets/"+tsID+"/submit", url.Values{"version": {"2"}}, "/timesheets/")
	ts2 := loc(emp, "/timesheets", url.Values{"week_start": {"2026-11-16"}}, "/timesheets/")

	exID := loc(emp, "/expenses", url.Values{"description": {"Train"}, "amount": {"45.50"}, "currency": {"USD"}, "category": {"travel"}, "incurred_on": {"2026-11-20"}, "receipt_filename": {"train.pdf"}, "receipt_type": {"application/pdf"}, "receipt_size": {"1024"}, "receipt_sha256": {strings.Repeat("ab", 32)}, "idem": {"exp-golden-0001"}}, "/expenses/")
	loc(emp, "/expenses/"+exID+"/submit", nil, "/expenses/")
	poID := loc(maker, "/purchase-orders", url.Values{"vendor_id": {vendor}, "currency": {"USD"}, "idem": {"po-golden-0001"}, "desc": {"Widgets"}, "qty": {"10"}, "price": {"2.50"}, "disc": {""}, "tax": {""}, "sku": {""}}, "/purchase-orders/")

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
	add("home-approver", boss, "/")
	add("home-reader", ro, "/")
	add("home-employee", emp, "/")
	add("approvals", boss, "/approvals")
	add("parties", ro, "/parties")
	add("party", ro, "/parties/"+cust)
	add("party-hostile", ro, "/parties/"+evil)
	add("party-new", maker, "/parties/new")
	add("party-edit", maker, "/parties/"+cust+"/edit")
	add("party-erase", adm, "/parties/"+cust+"/erase")
	add("invoices", ro, "/invoices")
	add("invoices-filtered", ro, "/invoices?status=draft&overdue=1")
	add("invoice-draft-maker", maker, "/invoices/"+draft)
	add("invoice-draft-approver", boss, "/invoices/"+draft)
	add("invoice-issued", maker, "/invoices/"+issued)
	add("invoice-yen", maker, "/invoices/"+yenID)
	add("invoice-hostile", maker, "/invoices/"+hostile)
	add("credit-note", maker, "/invoices/"+cnID)
	add("invoice-new", maker, "/invoices/new?party="+cust)
	add("invoice-edit", maker, "/invoices/"+draft+"/edit")
	add("credit-note-form", maker, "/invoices/"+issued+"/credit-note")
	add("invoice-print", ro, "/invoices/"+issued+"/html")
	link := maker.post("/invoices/"+issued+"/link", url.Values{"ttl_hours": {"24"}})
	out = append(out, shot{"link-created", link.Body.String()})
	add("recurring", ro, "/recurring")
	add("recurring-new", maker, "/recurring/new")
	add("contracts", ro, "/contracts")
	add("contract-new", maker, "/contracts/new")
	add("contract-new-template", maker, fmt.Sprintf("/contracts/new?template=%d&party=%s", tmpl.ID, cust))
	add("contract", maker, "/contracts/"+ctID)
	add("contract-version", ro, "/contracts/"+ctID+"/versions/1")
	add("timesheets", emp, "/timesheets")
	add("timesheet-submitted-employee", emp, "/timesheets/"+tsID)
	add("timesheet-submitted-approver", boss, "/timesheets/"+tsID)
	add("timesheet-draft-grid", emp, "/timesheets/"+ts2)
	add("expenses", emp, "/expenses")
	add("expense-new", emp, "/expenses/new")
	add("expense-submitted-approver", boss, "/expenses/"+exID)
	add("pos", ro, "/purchase-orders")
	add("po-new", maker, "/purchase-orders/new")
	add("po-draft-approver", boss, "/purchase-orders/"+poID)
	add("aging", ro, "/reports/aging?as_of=2027-01-31")
	add("statement", ro, "/reports/statement?party="+cust)
	add("settings", ro, "/settings")
	add("reminders", ro, "/settings/reminders")
	add("reminders-admin", adm, "/settings/reminders")
	add("err-not-found", maker, "/invoices/99999")
	add("err-forbidden", ro, "/invoices/new")
	erased := adm.post("/parties/"+vendor+"/erase", url.Values{"confirm": {"ERASE"}, "token": {admTok}})
	out = append(out, shot{"party-erased", erased.Body.String()})
	bad := maker.post("/invoices/"+draft+"/approve", nil)
	out = append(out, shot{"err-separation", bad.Body.String()})
	return out
}

var (
	reReq   = regexp.MustCompile(`Request id: <code>[0-9a-f]+</code>`)
	reToken = regexp.MustCompile(`value="[A-Za-z0-9_-]{43}"`)
	reLink  = regexp.MustCompile(`/public/invoices/[A-Za-z0-9._-]+`)
)

func normaliseUI(s string) string {
	s = csrfRe.ReplaceAllString(s, `name="csrf" value="CSRF"`)
	s = reReq.ReplaceAllString(s, `Request id: <code>REQID</code>`)
	s = reToken.ReplaceAllString(s, `value="TOKEN"`)
	s = reLink.ReplaceAllString(s, `/public/invoices/LINKTOKEN`)
	s = regexp.MustCompile(`value="[A-Za-z0-9_-]{32}"`).ReplaceAllString(s, `value="IDEM"`)
	return s
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
			t.Fatalf("%s: %v (run: go test ./core/finance -run TestUIGoldenPages -update)", s.name, err)
		}
		if string(want) != got {
			t.Errorf("%s differs from %s (run with -update after reviewing the change)", s.name, path)
		}
	}
}

// ---- static accessibility checklist (a trimmed port of the admin UI checker) -------------------------

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
	if len(pages) < 40 {
		t.Fatalf("only %d pages rendered", len(pages))
	}
	for _, s := range pages {
		if s.name == "invoice-print" {
			continue // the service's own printable document, checked by its own tests
		}
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

// ---- hostile text, money, immutability on the rendered pages ------------------------------------------

func TestUIGoldenPagesNeverLeakRawHostileMarkup(t *testing.T) {
	for _, s := range renderAllPages(t) {
		if s.name == "invoice-print" {
			continue
		}
		for _, raw := range []string{"<script>alert(1)</script>", "<img src=x onerror=1>", `"><script>x</script>`} {
			if strings.Contains(s.html, raw) {
				t.Errorf("%s contains raw hostile markup %q", s.name, raw)
			}
		}
		// no inline script, style attribute or handler, and no data: URL anywhere
		for _, bad := range []string{"<script", " style=", "onclick=", "onerror=", "data:"} {
			if strings.Contains(s.html, bad) && !strings.Contains(s.name, "hostile") && s.name != "party-hostile" {
				t.Errorf("%s contains %q", s.name, bad)
			}
		}
	}
}

func TestUIYenInvoiceRendersWholeUnits(t *testing.T) {
	for _, s := range renderAllPages(t) {
		if s.name != "invoice-yen" {
			continue
		}
		mustContainAll(t, s.html, "4500", "1500")
		if strings.Contains(s.html, "4500.00") || strings.Contains(s.html, "45.00") {
			t.Error("a JPY amount was rendered with decimals or scaled")
		}
	}
}

func mustContainAll(t *testing.T, s string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(s, p) {
			t.Errorf("missing %q", p)
		}
	}
}

// ---- stylesheet contrast, reduced motion, no inline code ------------------------------------------------

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
	b, err := os.ReadFile("ui/static/finance.css")
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
		t.Error("stylesheet must not load anything")
	}
}

// the print stylesheet of the app pages hides the navigation, so a printed page is just the content
func TestUIStylesheetIsPrintFriendly(t *testing.T) {
	b, _ := os.ReadFile("ui/static/finance.css")
	if !strings.Contains(string(b), "@media print") {
		t.Error("no print styles")
	}
}

// Templates are parsed on first use, so a syntax error would only show on its page: parse them all.
func TestUIEveryTemplateParses(t *testing.T) {
	u := &UI{}
	u.once.Do(u.init)
	ents, err := os.ReadDir("ui/templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() == "layout.html" || e.Name() == "partials.html" {
			continue
		}
		if _, err := u.template(e.Name()); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
	if _, err := u.template("nope.html"); err == nil {
		t.Error("an unknown template must be an error")
	}
}

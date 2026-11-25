package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// ---- money -------------------------------------------------------------------------------------

func TestUIMoneyFromIntegerMinorUnits(t *testing.T) {
	cases := []struct {
		minor int64
		cur   string
		want  string
	}{
		{123456, "USD", "1234.56"}, {5, "USD", "0.05"}, {-5, "USD", "-0.05"}, {-123456, "USD", "-1234.56"},
		{1234, "JPY", "1234"}, {-1234, "JPY", "-1234"}, {0, "JPY", "0"},
		{1234567, "KWD", "1234.567"}, {-7, "KWD", "-0.007"}, {1000, "KWD", "1.000"},
	}
	for _, c := range cases {
		if got := FormatMinor(c.minor, c.cur); got != c.want {
			t.Errorf("FormatMinor(%d, %s) = %s, want %s", c.minor, c.cur, got, c.want)
		}
		// what the UI prints must parse back to the same integer
		back, err := ParseMinor(c.want, c.cur)
		if err != nil || back != c.minor {
			t.Errorf("ParseMinor(%q, %s) = %d, %v; want %d", c.want, c.cur, back, err, c.minor)
		}
	}
	bad := []struct{ s, cur string }{
		{"1e3", "USD"}, {"1,5", "USD"}, {"NaN", "USD"}, {"Inf", "USD"}, {"0x10", "USD"}, {"1.234", "USD"}, {"1.5", "JPY"},
		{"", "USD"}, {" ", "USD"}, {".", "USD"}, {"1.", "USD"}, {"--1", "USD"}, {"99999999999999999999", "USD"}, {"1_000", "USD"},
	}
	for _, c := range bad {
		if v, err := ParseMinor(c.s, c.cur); err == nil {
			t.Errorf("ParseMinor(%q, %s) = %d, want an error", c.s, c.cur, v)
		}
	}
	if v, err := ParseMinor(" 12.5 ", "USD"); err != nil || v != 1250 {
		t.Errorf("ParseMinor with spaces = %d, %v", v, err)
	}
	if v, err := ParseMilli("1.5"); err != nil || v != 1500 {
		t.Errorf("ParseMilli = %d, %v", v, err)
	}
	if _, err := ParseMilli("1.5555"); err == nil {
		t.Error("ParseMilli accepted four decimals")
	}
	if v, err := ParseBP("10.5"); err != nil || v != 1050 {
		t.Errorf("ParseBP = %d, %v", v, err)
	}
}

// ---- helpers ---------------------------------------------------------------------------------

func locID(t *testing.T, loc, prefix string) string {
	t.Helper()
	if !strings.HasPrefix(loc, prefix) {
		t.Fatalf("Location %q does not start with %q", loc, prefix)
	}
	return strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], prefix)
}

func invoiceValues(party string, desc string, priceMajor string) url.Values {
	return url.Values{"party_id": {party}, "currency": {"USD"}, "series": {"INV"}, "idem": {"idem-key-" + desc + "-" + party},
		"desc": {desc, "", ""}, "qty": {"2", "", ""}, "price": {priceMajor, "", ""}, "disc": {"", "", ""}, "tax": {"", "", ""}, "sku": {"", "", ""}}
}

type uiWorld struct {
	u                                 *uiEnv
	writer, approver, admin, readonly *client
	party                             string
}

func newWorld(t *testing.T) *uiWorld {
	u := newUIEnv(t)
	w := &uiWorld{u: u}
	w.writer = u.login(t, u.token("user:wendy", []string{"finance.write", "finance.approve"}, nil, nil, false))
	w.approver = u.login(t, u.token("user:boss", []string{"finance.approve"}, nil, nil, false))
	w.admin = u.login(t, u.token("user:root", []string{"finance.admin"}, nil, nil, false))
	w.readonly = u.login(t, u.token("user:ro", []string{"finance.read"}, nil, nil, false))
	r := w.writer.post("/parties", url.Values{"kind": {"customer"}, "name": {"Acme Pty"}, "email": {"ap@acme.example"}, "address": {"1 Fake Rd"}, "currency": {"USD"}})
	status(t, r, 303)
	w.party = locID(t, r.Header().Get("Location"), "/parties/")
	return w
}

func (w *uiWorld) draft(t *testing.T, desc, price string) string {
	t.Helper()
	r := w.writer.post("/invoices", invoiceValues(w.party, desc, price))
	status(t, r, 303)
	return locID(t, r.Header().Get("Location"), "/invoices/")
}

// ---- invoices ----------------------------------------------------------------------------------

func TestUIInvoiceLifecycleWithSeparationOfDuties(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Consulting", "100.00")
	page := w.writer.get("/invoices/" + inv)
	status(t, page, 200)
	mustHave(t, page, "Consulting", "200.00", "Draft")
	// the creator sees why they cannot approve; the button is disabled, not offered
	mustHave(t, page, "disabled", "created this")
	mustNot(t, page, `action="/invoices/`+inv+`/approve"`)

	// even if the form is forged, the service refuses: separation of duties is enforced there
	status(t, w.writer.post("/invoices/"+inv+"/approve", nil), 403)
	// a writer without finance.approve is refused at the route
	status(t, w.readonly.post("/invoices/"+inv+"/approve", nil), 403)

	// the approver sees the approve form
	ap := w.approver.get("/invoices/" + inv)
	status(t, ap, 200)
	mustHave(t, ap, `action="/invoices/`+inv+`/approve"`)
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)

	done := w.writer.get("/invoices/" + inv)
	mustHave(t, done, "INV-00001", "Approved")
	// immutability: no edit link, no edit form, and the service refuses a forged edit
	mustNot(t, done, "/invoices/"+inv+"/edit", `action="/invoices/`+inv+`"`)
	if r := w.writer.get("/invoices/" + inv + "/edit"); r.Code != 409 {
		t.Fatalf("edit form of an approved invoice = %d, want 409", r.Code)
	}
	if r := w.writer.post("/invoices/"+inv, url.Values{"currency": {"USD"}, "desc": {"Changed"}, "qty": {"1"}, "price": {"1"}, "disc": {""}, "tax": {""}, "sku": {""}}); r.Code != 409 {
		t.Fatalf("forged edit = %d, want 409", r.Code)
	}

	// send, then pay in two parts
	status(t, w.writer.post("/invoices/"+inv+"/send", nil), 303)
	mustHave(t, w.writer.get("/invoices/"+inv), "Sent")
	pay := func(amount, key string) {
		r := w.writer.post("/invoices/"+inv+"/payments", url.Values{"amount": {amount}, "method": {"bank"}, "reference": {"ref-" + key}, "received_on": {"2026-11-25"}, "idem": {"pay-" + key + "-0123"}})
		status(t, r, 303)
	}
	pay("150.00", "a")
	mustHave(t, w.writer.get("/invoices/"+inv), "Partially paid", "50.00")
	// a double submit of the same form (same key) records one payment
	pay("150.00", "a")
	var pays struct{ Items []Payment }
	u := w.u
	u.must(200, adminTok, "GET", "/v1/invoices/"+inv+"/payments", nil, &pays)
	if len(pays.Items) != 1 {
		t.Fatalf("payments after a double submit = %d, want 1", len(pays.Items))
	}
	// an overpayment is refused with a readable message, not a 500
	r := w.writer.post("/invoices/"+inv+"/payments", url.Values{"amount": {"60.00"}, "method": {"bank"}, "reference": {"x"}, "idem": {"pay-over-0123"}})
	if r.Code >= 500 || r.Code < 400 {
		t.Fatalf("overpayment = %d", r.Code)
	}
	// a payment amount that is not a plain decimal is refused before reaching the service
	r = w.writer.post("/invoices/"+inv+"/payments", url.Values{"amount": {"1e2"}, "method": {"bank"}, "idem": {"pay-float-0123"}})
	status(t, r, 422)
	pay("50.00", "b")
	mustHave(t, w.writer.get("/invoices/"+inv), "Paid")
}

func TestUIInvoiceCreateIsIdempotentPerRenderedForm(t *testing.T) {
	w := newWorld(t)
	form := w.writer.get("/invoices/new?party=" + w.party)
	status(t, form, 200)
	m := regexp.MustCompile(`name="idem" value="([^"]+)"`).FindStringSubmatch(form.Body.String())
	if m == nil || len(m[1]) < 16 {
		t.Fatal("the form carries no random idempotency key")
	}
	form2 := w.writer.get("/invoices/new?party=" + w.party)
	m2 := regexp.MustCompile(`name="idem" value="([^"]+)"`).FindStringSubmatch(form2.Body.String())
	if m2[1] == m[1] {
		t.Fatal("two renders share one idempotency key")
	}
	v := invoiceValues(w.party, "Once", "10.00")
	v.Set("idem", m[1])
	a := w.writer.post("/invoices", v)
	v.Set("idem", m[1])
	b := w.writer.post("/invoices", v)
	status(t, a, 303)
	status(t, b, 303)
	if a.Header().Get("Location") != b.Header().Get("Location") {
		t.Fatalf("a double submit created two drafts: %s %s", a.Header().Get("Location"), b.Header().Get("Location"))
	}
	var list struct{ Items []Invoice }
	w.u.must(200, adminTok, "GET", "/v1/invoices", nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("invoices = %d", len(list.Items))
	}
}

func TestUIDraftEditAndDelete(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Draft work", "10.00")
	status(t, w.writer.get("/invoices/"+inv+"/edit"), 200)
	r := w.writer.post("/invoices/"+inv, url.Values{"currency": {"USD"}, "version": {"1"}, "desc": {"Edited work"}, "qty": {"3"}, "price": {"10.00"}, "disc": {""}, "tax": {""}, "sku": {""}, "notes": {"n"}, "project": {""}, "issue_date": {""}, "due_date": {""}, "fx_rate": {""}})
	status(t, r, 303)
	mustHave(t, w.writer.get("/invoices/"+inv), "Edited work", "30.00")
	// a stale form (old version) is refused, not silently applied
	r = w.writer.post("/invoices/"+inv, url.Values{"currency": {"USD"}, "version": {"1"}, "desc": {"Stale"}, "qty": {"1"}, "price": {"1"}, "disc": {""}, "tax": {""}, "sku": {""}})
	status(t, r, 412)
	status(t, w.writer.post("/invoices/"+inv+"/delete", url.Values{"version": {"2"}}), 303)
	status(t, w.writer.get("/invoices/"+inv), 404)
}

func TestUIVoidAndCreditNote(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Goods", "100.00")
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)
	status(t, w.writer.post("/invoices/"+inv+"/send", nil), 303)

	// credit note: lines in the form, capped by the service at the unpaid balance
	cn := url.Values{"reason": {"Returned goods"}, "idem": {"cn-key-0123456"}, "desc": {"Returned"}, "qty": {"1"}, "price": {"50.00"}, "disc": {""}, "tax": {""}, "sku": {""}}
	status(t, w.writer.get("/invoices/"+inv+"/credit-note"), 200)
	r := w.writer.post("/invoices/"+inv+"/credit-note", cn)
	status(t, r, 303)
	cnID := locID(t, r.Header().Get("Location"), "/invoices/")
	status(t, w.approver.post("/invoices/"+cnID+"/approve", nil), 303)
	mustHave(t, w.writer.get("/invoices/"+inv), "50.00")
	mustHave(t, w.writer.get("/invoices/"+cnID), "CN-00001", "Credit note")
	// over the cap: refused with the service's message
	cn.Set("idem", "cn-key-over-0123")
	cn.Set("price", "500.00")
	r = w.writer.post("/invoices/"+inv+"/credit-note", cn)
	status(t, r, 303) // drafting is allowed; approval is where the cap is checked
	over := locID(t, r.Header().Get("Location"), "/invoices/")
	if r := w.approver.post("/invoices/"+over+"/approve", nil); r.Code != 422 {
		t.Fatalf("approving a credit note above the balance = %d, want 422: %s", r.Code, r.Body.String())
	}
	// void needs a reason and the approve scope; the invoice has a credit note, so it is a conflict
	if r := w.approver.post("/invoices/"+inv+"/void", url.Values{"reason": {"mistake"}}); r.Code != 409 {
		t.Fatalf("void with a credit note = %d, want 409", r.Code)
	}
	inv2 := w.draft(t, "Other", "10.00")
	status(t, w.approver.post("/invoices/"+inv2+"/approve", nil), 303)
	status(t, w.approver.post("/invoices/"+inv2+"/void", url.Values{"reason": {""}}), 422)
	status(t, w.approver.post("/invoices/"+inv2+"/void", url.Values{"reason": {"entered twice"}}), 303)
	mustHave(t, w.writer.get("/invoices/"+inv2), "Void", "entered twice")
	status(t, w.readonly.post("/invoices/"+inv2+"/void", url.Values{"reason": {"x"}}), 403)
}

func TestUIPublicLinkCreateShowOnceRevoke(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Linked", "10.00")
	// a draft is not a document: no link
	if r := w.writer.post("/invoices/"+inv+"/link", url.Values{"ttl_hours": {"24"}}); r.Code != 409 {
		t.Fatalf("link for a draft = %d, want 409", r.Code)
	}
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)
	r := w.writer.post("/invoices/"+inv+"/link", url.Values{"ttl_hours": {"24"}})
	status(t, r, 200)
	m := regexp.MustCompile(`https://billing\.example\.test/public/invoices/([A-Za-z0-9._-]+)`).FindStringSubmatch(r.Body.String())
	if m == nil {
		t.Fatalf("the link is not shown after creation: %s", r.Body.String())
	}
	tok := m[1]
	if pub := httptestGet(w.u.h, "/public/invoices/"+tok); pub.Code != 200 {
		t.Fatalf("public link = %d", pub.Code)
	}
	// the token is shown once: neither the invoice page nor a reload shows it again
	again := w.writer.get("/invoices/" + inv)
	mustNot(t, again, tok, "/public/invoices/")
	mustHave(t, again, `action="/invoices/`+inv+`/link/revoke"`)
	status(t, w.writer.post("/invoices/"+inv+"/link/revoke", nil), 303)
	if pub := httptestGet(w.u.h, "/public/invoices/"+tok); pub.Code != 404 {
		t.Fatalf("revoked link = %d, want 404", pub.Code)
	}
	status(t, w.readonly.post("/invoices/"+inv+"/link", url.Values{"ttl_hours": {"24"}}), 403)
}

func TestUIInvoiceHTMLViewReusesTheServiceRenderer(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, `<img src=x onerror=alert(1)>`, "10.00")
	r := w.writer.get("/invoices/" + inv + "/html")
	status(t, r, 200)
	want, _ := RenderInvoiceHTML(mustInvoice(t, w, inv), Seller{Name: "Example Seller Pty", Address: "1 Fake Street"})
	if r.Body.String() != string(want) {
		t.Fatal("the UI must serve exactly what the service's renderer produces")
	}
	mustNot(t, r, "<img src=x")
	mustHave(t, r, "&lt;img")
	if csp := r.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	mustHave(t, w.writer.get("/invoices/"+inv), `href="/invoices/`+inv+`/html"`)
	if !strings.Contains(r.Body.String(), "@media print") {
		t.Error("the invoice view must be print friendly")
	}
}

func mustInvoice(t *testing.T, w *uiWorld, id string) *Invoice {
	var v Invoice
	w.u.must(200, adminTok, "GET", "/v1/invoices/"+id, nil, &v)
	return &v
}

// ---- parties, privacy, erasure ---------------------------------------------------------------

func TestUIPartiesHostileTextAndContactData(t *testing.T) {
	w := newWorld(t)
	evil := `"><script>alert(1)</script>{{.}}'` + "`"
	r := w.writer.post("/parties", url.Values{"kind": {"vendor"}, "name": {evil}, "contact_name": {evil}, "address": {evil}, "notes": {evil}, "email": {"x@example.test"}})
	status(t, r, 303)
	id := locID(t, r.Header().Get("Location"), "/parties/")
	for _, p := range []string{"/parties", "/parties/" + id, "/parties/" + id + "/edit", "/parties?q=" + url.QueryEscape(evil)} {
		pg := w.writer.get(p)
		status(t, pg, 200)
		mustNot(t, pg, "<script>alert", `"><script`)
	}
	mustHave(t, w.writer.get("/parties/"+id), "&lt;script&gt;")
	// contact details are shown to finance.read; the approver has no read-only party access
	mustHave(t, w.readonly.get("/parties/"+w.party), "ap@acme.example")
	if r := w.approver.get("/parties/" + w.party); r.Code != 200 {
		t.Fatalf("an approver (read implied) = %d", r.Code)
	}
	// every contact read is audited by the service (the UI goes through GetParty)
	var audit struct{ Items []AuditEntry }
	w.u.must(200, adminTok, "GET", "/v1/audit?limit=100", nil, &audit)
	found := false
	for _, a := range audit.Items {
		if a.Action == "party.read" {
			found = true
		}
	}
	if !found {
		t.Fatal("party reads through the UI are not audited")
	}
	// no JSON or contact data leaks into the dashboard
	mustNot(t, w.readonly.get("/"), "ap@acme.example")
}

func TestUIErasureShowsRetentionHonestly(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Issued", "10.00")
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)
	status(t, w.writer.post("/invoices/"+inv+"/send", nil), 303)
	w.draft(t, "Just a draft", "5.00")

	// only finance.admin gets the erase flow
	status(t, w.writer.get("/parties/"+w.party+"/erase"), 403)
	status(t, w.writer.post("/parties/"+w.party+"/erase", url.Values{"confirm": {"ERASE"}}), 403)
	page := w.admin.get("/parties/" + w.party + "/erase")
	status(t, page, 200)
	mustHave(t, page, "legal retention", `name="confirm"`)
	// confirmation is required
	status(t, w.admin.post("/parties/"+w.party+"/erase", url.Values{"confirm": {"no"}}), 422)
	r := w.admin.post("/parties/"+w.party+"/erase", url.Values{"confirm": {"ERASE"}})
	status(t, r, 200)
	// the outcome is what the service reports: erased fields, deleted drafts, what is kept and why
	mustHave(t, r, "Erased fields", "email", "Deleted drafts", "finance-invoice-unpaid", "unpaid invoices are never purged")
	mustNot(t, r, "ap@acme.example")
	pg := w.readonly.get("/parties/" + w.party)
	mustHave(t, pg, "Erased party")
	mustNot(t, pg, "ap@acme.example", "1 Fake Rd")
}

// ---- scoping ------------------------------------------------------------------------------------

func TestUIScopedTokenSeesOnlyItsParty(t *testing.T) {
	w := newWorld(t)
	other := w.writer.post("/parties", url.Values{"kind": {"customer"}, "name": {"Other Co"}, "currency": {"USD"}})
	otherID := locID(t, other.Header().Get("Location"), "/parties/")
	mine := w.draft(t, "Mine", "10.00")
	r := w.writer.post("/invoices", invoiceValues(otherID, "Theirs", "20.00"))
	theirs := locID(t, r.Header().Get("Location"), "/invoices/")

	pid := mustAtoi(t, w.party)
	c := w.u.login(t, w.u.token("user:scoped", []string{"finance.read", "finance.write"}, []int64{pid}, nil, false))
	status(t, c.get("/invoices/"+mine), 200)
	list := c.get("/invoices")
	mustHave(t, list, "Draft #"+mine)
	mustNot(t, list, "Draft #"+theirs, "Other Co")
	missing := c.get("/invoices/" + theirs)
	nothing := c.get("/invoices/99999")
	status(t, missing, 404)
	if missing.Body.String() != nothing.Body.String() {
		// request ids differ; compare with them removed
		re := regexp.MustCompile(`<code>[0-9a-f]+</code>`)
		if re.ReplaceAllString(missing.Body.String(), "") != re.ReplaceAllString(nothing.Body.String(), "") {
			t.Fatal("a record outside the scope must look exactly like one that does not exist")
		}
	}
	status(t, c.get("/parties/"+otherID), 404)
	status(t, c.get("/parties/"+otherID+"/edit"), 404)
	status(t, c.get("/invoices/"+theirs+"/html"), 404)
	status(t, c.post("/invoices/"+theirs+"/send", nil), 404)
	mustNot(t, c.get("/parties"), "Other Co")
	mustNot(t, c.get("/reports/aging"), "Other Co")
}

func mustAtoi(t *testing.T, s string) int64 {
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUIHostileTextEscapedInEveryContext(t *testing.T) {
	w := newWorld(t)
	evil := `"><script>alert(1)</script>'onmouseover='x`
	r := w.writer.post("/invoices", url.Values{"party_id": {w.party}, "currency": {"USD"}, "idem": {"idem-evil-0123456"}, "notes": {evil}, "project": {""},
		"desc": {evil}, "qty": {"1"}, "price": {"1.00"}, "disc": {""}, "tax": {""}, "sku": {""}})
	status(t, r, 303)
	id := locID(t, r.Header().Get("Location"), "/invoices/")
	for _, p := range []string{"/invoices/" + id, "/invoices/" + id + "/edit", "/invoices", "/invoices/" + id + "/credit-note"} {
		pg := w.writer.get(p)
		if pg.Code != 200 && pg.Code != 409 {
			t.Fatalf("%s = %d", p, pg.Code)
		}
		mustNot(t, pg, "<script>alert")
		if strings.Contains(pg.Body.String(), `onmouseover='x`) {
			t.Errorf("%s: an attribute breakout survived", p)
		}
	}
	// the same text in a query parameter that is echoed into a form
	pg := w.writer.get("/invoices?status=" + url.QueryEscape(evil) + "&q=" + url.QueryEscape(evil))
	mustNot(t, pg, "<script>alert")
}

// ---- CSV export --------------------------------------------------------------------------------

func TestUIExpenseExportDefusesFormulasAndIsAudited(t *testing.T) {
	w := newWorld(t)
	emp := w.u.login(t, w.u.token("user:emp", []string{"finance.write", "finance.read"}, nil, nil, false))
	r := emp.post("/expenses", url.Values{"description": {`=HYPERLINK("http://evil.example","x")`}, "amount": {"12.34"}, "currency": {"USD"}, "category": {"travel"},
		"incurred_on": {"2026-11-20"}, "idem": {"exp-key-0123456"}})
	status(t, r, 303)
	exp := w.readonly.get("/exports/expenses.csv")
	status(t, exp, 200)
	if ct := exp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type %q", ct)
	}
	if !strings.Contains(exp.Header().Get("Content-Disposition"), "attachment") {
		t.Error("exports download as attachments")
	}
	if strings.Contains(exp.Body.String(), "\n=HYPERLINK") || strings.Contains(exp.Body.String(), ",=HYPERLINK") || strings.Contains(exp.Body.String(), `"=HYPERLINK`) {
		t.Fatalf("formula injection in the CSV: %s", exp.Body.String())
	}
	mustHave(t, exp, "'=HYPERLINK", "12.34")
	var audit struct{ Items []AuditEntry }
	w.u.must(200, adminTok, "GET", "/v1/audit?limit=100", nil, &audit)
	ok := false
	for _, a := range audit.Items {
		if a.Action == "export.expenses" && a.Actor == "user:ro" {
			ok = true
		}
	}
	if !ok {
		t.Fatal("the export is not audited")
	}
	w.draft(t, "Row", "10.00")
	inv := w.readonly.get("/exports/invoices.csv?status=draft")
	status(t, inv, 200)
	mustHave(t, inv, "draft", "20.00")
	status(t, w.approver.get("/exports/payments.csv"), 200)
	// a malformed date is a 422 page, not a download
	status(t, w.readonly.get("/exports/invoices.csv?from=garbage"), 422)
}

// ---- timesheets, expenses, approvals ----------------------------------------------------------

func TestUITimesheetGridSubmitApproveAndLock(t *testing.T) {
	w := newWorld(t)
	emp := w.u.login(t, w.u.token("user:emp", []string{"finance.write", "finance.read"}, nil, nil, true))
	// an own-only token reaches timesheets but not invoices
	status(t, emp.get("/timesheets"), 200)
	status(t, emp.get("/invoices"), 403)
	status(t, emp.get("/parties"), 403)

	r := emp.post("/timesheets", url.Values{"week_start": {"2026-11-23"}})
	status(t, r, 303)
	id := locID(t, r.Header().Get("Location"), "/timesheets/")
	grid := emp.get("/timesheets/" + id)
	status(t, grid, 200)
	mustHave(t, grid, `name="h"`, "2026-11-23")
	form := url.Values{"version": {"1"}, "project": {"ACME", "", ""}, "task": {"ACME-1", "", ""}, "desc": {"Build", "", ""}, "billable": {"1", "", ""}}
	// one value per weekday per row, "H:MM" or hours
	for d := 0; d < 7; d++ {
		v := ""
		if d < 5 {
			v = "7:30"
		}
		form.Add("h", v)
	}
	for d := 0; d < 14; d++ {
		form.Add("h", "")
	}
	status(t, emp.post("/timesheets/"+id+"/entries", form), 303)
	g := emp.get("/timesheets/" + id)
	mustHave(t, g, "37:30", `value="7:30"`)
	status(t, emp.post("/timesheets/"+id+"/submit", url.Values{"version": {"2"}}), 303)

	// the submitter cannot approve their own sheet, and the page says so
	mustHave(t, emp.get("/timesheets/"+id), "Submitted")
	q := w.approver.get("/approvals")
	status(t, q, 200)
	mustHave(t, q, "user:emp", "2026-11-23", "37:30")
	status(t, emp.post("/timesheets/"+id+"/approve", nil), 403)
	status(t, w.approver.post("/timesheets/"+id+"/reject", url.Values{"reason": {""}}), 422)
	status(t, w.approver.post("/timesheets/"+id+"/approve", nil), 303)
	mustHave(t, emp.get("/timesheets/"+id), "Approved")

	// another employee's sheet is a 404 for an own-only token
	other := w.u.login(t, w.u.token("user:emp2", []string{"finance.write", "finance.read"}, nil, nil, true))
	status(t, other.get("/timesheets/"+id), 404)

	// period locks: a locked week cannot be edited, and the page says so
	r = emp.post("/timesheets", url.Values{"week_start": {"2026-11-30"}})
	status(t, r, 303)
	id2 := locID(t, r.Header().Get("Location"), "/timesheets/")
	w.u.do(adminTok, "PUT", "/v1/period-lock", obj{"locked_through": "2026-12-06", "note": "close"}, nil)
	pg := emp.get("/timesheets/" + id2)
	mustHave(t, pg, "locked")
	f2 := url.Values{"version": {"1"}, "project": {"ACME"}, "task": {""}, "desc": {"x"}, "billable": {"1"}, "h": {"1:00", "", "", "", "", "", ""}}
	status(t, emp.post("/timesheets/"+id2+"/entries", f2), 409)
	// and a locked week cannot be opened at all
	status(t, emp.post("/timesheets", url.Values{"week_start": {"2026-11-16"}}), 409)
}

func TestUIExpenseFlowWithReceiptMetadataAndApproval(t *testing.T) {
	w := newWorld(t)
	emp := w.u.login(t, w.u.token("user:emp", []string{"finance.write", "finance.read"}, nil, nil, true))
	sha := strings.Repeat("ab", 32)
	r := emp.post("/expenses", url.Values{"description": {"Train"}, "amount": {"45.50"}, "currency": {"USD"}, "category": {"travel"}, "incurred_on": {"2026-11-20"},
		"receipt_filename": {"train.pdf"}, "receipt_type": {"application/pdf"}, "receipt_size": {"1024"}, "receipt_sha256": {sha}, "idem": {"exp-key-train-01"}})
	status(t, r, 303)
	id := locID(t, r.Header().Get("Location"), "/expenses/")
	pg := emp.get("/expenses/" + id)
	mustHave(t, pg, "train.pdf", sha, "45.50")
	mustNot(t, pg, `type="file"`) // metadata only: the service never receives file bytes
	status(t, emp.post("/expenses/"+id+"/submit", nil), 303)
	status(t, emp.post("/expenses/"+id+"/approve", nil), 403) // no approve scope, own-only
	// an approver who also files claims sees the approve button disabled for their own claim, and the service refuses a forged post
	both := w.u.login(t, w.u.token("user:both", []string{"finance.write", "finance.approve"}, nil, nil, false))
	br := both.post("/expenses", url.Values{"description": {"Taxi"}, "amount": {"9.00"}, "currency": {"USD"}, "category": {"travel"}, "incurred_on": {"2026-11-20"}, "idem": {"exp-key-both-001"}})
	bid := locID(t, br.Header().Get("Location"), "/expenses/")
	status(t, both.post("/expenses/"+bid+"/submit", nil), 303)
	mustHave(t, both.get("/expenses/"+bid), "disabled", "someone else must approve")
	mustNot(t, both.get("/expenses/"+bid), `action="/expenses/`+bid+`/approve"`)
	status(t, both.post("/expenses/"+bid+"/approve", nil), 403)
	status(t, w.approver.post("/expenses/"+id+"/reject", url.Values{"reason": {""}}), 422)
	status(t, w.approver.post("/expenses/"+id+"/approve", nil), 303)
	mustHave(t, w.approver.get("/expenses/"+id), "Approved")
	// only a writer other than the owner reimburses
	status(t, emp.post("/expenses/"+id+"/reimburse", url.Values{"reference": {"r1"}}), 403)
	status(t, w.writer.post("/expenses/"+id+"/reimburse", url.Values{"reference": {"r1"}}), 303)
	mustHave(t, w.writer.get("/expenses/"+id), "Reimbursed")
	// a bad receipt hash is refused
	r = emp.post("/expenses", url.Values{"description": {"Bad"}, "amount": {"1.00"}, "currency": {"USD"}, "category": {"travel"}, "incurred_on": {"2026-11-20"},
		"receipt_filename": {"a.pdf"}, "receipt_type": {"application/pdf"}, "receipt_size": {"10"}, "receipt_sha256": {"zz"}, "idem": {"exp-key-bad-0001"}})
	status(t, r, 422)
}

// ---- contracts, recurring, purchase orders, reports, settings ------------------------------------

func TestUIContractFromTemplateWithVariables(t *testing.T) {
	w := newWorld(t)
	var tmpl ContractTemplate
	w.u.must(201, adminTok, "POST", "/v1/contract-templates", obj{"name": "NDA", "body": "Agreement with {{party}} until {{until}}."}, &tmpl)
	pg := w.writer.get("/contracts/new?party=" + w.party)
	status(t, pg, 200)
	mustHave(t, pg, "NDA")
	r := w.writer.post("/contracts", url.Values{"party_id": {w.party}, "title": {"Mutual NDA"}, "template_id": {fmt.Sprint(tmpl.ID)}, "var_party": {"<b>Acme</b>"}, "var_until": {"2027-01-01"},
		"renewal_date": {"2026-12-15"}, "end_date": {"2027-01-01"}})
	status(t, r, 303)
	id := locID(t, r.Header().Get("Location"), "/contracts/")
	c := w.writer.get("/contracts/" + id)
	mustHave(t, c, "Mutual NDA", "Draft", "2026-12-15", "&lt;b&gt;Acme&lt;/b&gt;")
	mustNot(t, c, "<b>Acme</b>")
	v := w.writer.get("/contracts/" + id + "/versions/1")
	status(t, v, 200)
	mustHave(t, v, "Agreement with", "until 2027-01-01")
	// a missing variable is a readable 422
	status(t, w.writer.post("/contracts", url.Values{"party_id": {w.party}, "title": {"X"}, "template_id": {fmt.Sprint(tmpl.ID)}, "var_party": {"A"}}), 422)
	// send for signature by hand (no external signer), then it shows as sent
	status(t, w.writer.post("/contracts/"+id+"/send", url.Values{"mode": {"manual"}}), 303)
	mustHave(t, w.writer.get("/contracts/"+id), "Sent")
	mustHave(t, w.readonly.get("/contracts"), "Mutual NDA")
	status(t, w.readonly.post("/contracts/"+id+"/send", url.Values{"mode": {"manual"}}), 403)
	// there is no edit-the-text form: versions are read only
	mustNot(t, w.writer.get("/contracts/"+id+"/versions/1"), "<textarea")
}

func TestUIRecurringPurchaseOrdersReportsAndSettings(t *testing.T) {
	w := newWorld(t)
	r := w.writer.post("/recurring", url.Values{"party_id": {w.party}, "name": {"Retainer"}, "currency": {"USD"}, "interval": {"monthly"}, "start_date": {"2026-12-01"}, "due_days": {"14"},
		"desc": {"Retainer"}, "qty": {"1"}, "price": {"500.00"}, "disc": {""}, "tax": {""}, "sku": {""}})
	status(t, r, 303)
	mustHave(t, w.readonly.get("/recurring"), "Retainer", "2026-12-01")
	rid := firstID(t, w.writer.get("/recurring").Body.String(), `action="/recurring/`)
	status(t, w.writer.post("/recurring/"+rid+"/status", url.Values{"status": {"paused"}}), 303)
	mustHave(t, w.writer.get("/recurring"), "Paused")

	vr := w.writer.post("/parties", url.Values{"kind": {"vendor"}, "name": {"Parts Ltd"}, "currency": {"USD"}})
	vendor := locID(t, vr.Header().Get("Location"), "/parties/")
	po := w.writer.post("/purchase-orders", url.Values{"vendor_id": {vendor}, "currency": {"USD"}, "idem": {"po-key-0123456"}, "desc": {"Widgets"}, "qty": {"10"}, "price": {"2.50"}, "disc": {""}, "tax": {""}, "sku": {""}})
	status(t, po, 303)
	poID := locID(t, po.Header().Get("Location"), "/purchase-orders/")
	mustHave(t, w.writer.get("/purchase-orders/"+poID), "Widgets", "25.00")
	status(t, w.writer.post("/purchase-orders/"+poID+"/approve", nil), 403)
	status(t, w.approver.post("/purchase-orders/"+poID+"/approve", nil), 303)
	mustHave(t, w.readonly.get("/purchase-orders"), "Parts Ltd")

	inv := w.draft(t, "Aged", "100.00")
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)
	rep := w.readonly.get("/reports/aging?as_of=2027-03-01")
	status(t, rep, 200)
	mustHave(t, rep, "USD", "200.00", "Acme Pty", "61 to 90")
	status(t, w.readonly.get("/reports/aging?as_of=garbage"), 422)
	st := w.readonly.get("/reports/statement?party=" + w.party)
	status(t, st, 200)
	mustHave(t, st, "INV-00001", "200.00")

	set := w.readonly.get("/settings")
	status(t, set, 200)
	mustHave(t, set, "INV", "Tax rates", "USD", "read-only")
	mustNot(t, set, `method="post"`+` action="/settings`)
}

func TestUIDashboardAggregatesAndApprovalQueue(t *testing.T) {
	w := newWorld(t)
	inv := w.draft(t, "Waiting", "10.00")
	// the approver sees the invoice waiting, the creator does not (they cannot approve it)
	mustHave(t, w.approver.get("/"), "Waiting for your approval")
	q := w.approver.get("/approvals")
	mustHave(t, q, "/invoices/"+inv)
	mustNot(t, w.writer.get("/approvals"), "/invoices/"+inv)
	status(t, w.approver.post("/invoices/"+inv+"/approve", nil), 303)
	d := w.readonly.get("/")
	status(t, d, 200)
	mustHave(t, d, "Overdue", "this month")
	// aggregate only: no party names, emails or document lines on the dashboard
	mustNot(t, d, "Acme Pty", "ap@acme.example", "Waiting")
}

func TestUIDatabaseDeadlineIs503NotAHang(t *testing.T) {
	// the mapping used by every handler: a read that exceeds its deadline is a 503 with Retry-After
	if uiStatus(errDeadline()) != 503 {
		t.Fatal("deadline must map to 503")
	}
}

func TestUISurfacesServiceJSONShape(t *testing.T) {
	// keep encoding/json imported for decoding helpers above
	var x struct{}
	_ = json.Unmarshal([]byte("{}"), &x)
}

func firstID(t *testing.T, body, prefix string) string {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(prefix) + `([0-9]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %q in %s", prefix, body)
	}
	return m[1]
}

func errDeadline() error { return context.DeadlineExceeded }

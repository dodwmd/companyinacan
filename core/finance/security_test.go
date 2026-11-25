package finance

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPartyAndProjectScopingIDOR(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	a := e.party(maker, "Alpha Fake", "customer")
	b := e.party(maker, "Beta Fake", "customer")
	ia := e.invoice(maker, a, obj{"project": "ALPHA"}, line("a", 1000, 100))
	ib := e.invoice(maker, b, obj{"project": "BETA"}, line("b", 1000, 200))
	for _, i := range []*Invoice{ia, ib} {
		e.must(200, boss, "POST", "/v1/invoices/"+id(i.ID)+"/approve", nil, nil)
	}
	scopedA := e.token("a-reader", []string{"finance.write"}, []int64{a}, nil, false)
	scopedProj := e.token("proj-reader", []string{"finance.read"}, nil, []string{"BETA"}, false)

	var list struct{ Items []Invoice }
	e.must(200, scopedA, "GET", "/v1/invoices", nil, &list)
	if len(list.Items) != 1 || list.Items[0].PartyID != a {
		t.Fatalf("party-scoped list %+v", list.Items)
	}
	// An OR-ish filter cannot widen the scope: asking for the other party or the other status gets nothing.
	for _, q := range []string{"?party_id=" + id(b), "?status=approved&party_id=" + id(b), "?project=BETA", "?overdue_as_of=2030-01-01&party_id=" + id(b)} {
		e.must(200, scopedA, "GET", "/v1/invoices"+q, nil, &list)
		if len(list.Items) != 0 {
			t.Errorf("%s leaked %d invoices", q, len(list.Items))
		}
	}
	e.must(200, scopedProj, "GET", "/v1/invoices", nil, &list)
	if len(list.Items) != 1 || list.Items[0].ID != ib.ID {
		t.Fatalf("project-scoped list %+v", list.Items)
	}
	// Every by-id route answers 404 for a record outside the scope, as for one that does not exist.
	other := id(ib.ID)
	for _, c := range [][3]string{
		{"GET", "/v1/invoices/" + other, ""}, {"PATCH", "/v1/invoices/" + other, `{"notes":"x"}`}, {"DELETE", "/v1/invoices/" + other, ""},
		{"POST", "/v1/invoices/" + other + "/send", ""}, {"GET", "/v1/invoices/" + other + "/html", ""},
		{"POST", "/v1/invoices/" + other + "/link", ""}, {"POST", "/v1/invoices/" + other + "/link/revoke", ""},
		{"GET", "/v1/invoices/" + other + "/payments", ""}, {"POST", "/v1/invoices/" + other + "/payments", `{"amount_minor":1}`},
		{"POST", "/v1/invoices/" + other + "/credit-notes", `{"reason":"r","lines":[{"description":"x","quantity_milli":1000,"unit_price_minor":1}]}`},
		{"POST", "/v1/invoices/" + other + "/checkout", ""},
		{"GET", "/v1/parties/" + id(b), ""}, {"PATCH", "/v1/parties/" + id(b), `{"name":"x"}`}, {"POST", "/v1/parties/" + id(b) + "/erase", ""},
	} {
		code, _, body := e.req(scopedA, c[0], c[1], c[2])
		if code != 404 && code != 403 {
			t.Errorf("%s %s = %d (%s), want 404", c[0], c[1], code, body)
		}
	}
	missing, _, _ := e.req(maker, "GET", "/v1/invoices/9999", nil)
	scoped, _, _ := e.req(scopedA, "GET", "/v1/invoices/"+other, nil)
	if missing != scoped {
		t.Errorf("out-of-scope (%d) must look like missing (%d)", scoped, missing)
	}
	// A party-scoped token cannot create parties, nor invoices for another party.
	e.must(403, scopedA, "POST", "/v1/parties", obj{"name": "Gamma"}, nil)
	e.must(404, scopedA, "POST", "/v1/invoices", obj{"party_id": b, "lines": []obj{line("x", 1000, 1)}}, nil)
	e.must(404, scopedA, "POST", "/v1/invoices/"+other+"/payments", obj{"amount_minor": 1}, nil)
	// Scoped aging and exports follow the scope.
	var ag AgingReport
	e.must(200, scopedA, "GET", "/v1/reports/aging", nil, &ag)
	if len(ag.Currencies) != 1 || ag.Currencies[0].Totals.Total != 100 {
		t.Fatalf("scoped aging %+v", ag)
	}
	_, _, csv := e.req(scopedA, "GET", "/v1/exports/invoices.csv", nil)
	if strings.Contains(string(csv), "INV-00002") || !strings.Contains(string(csv), "INV-00001") {
		t.Fatalf("scoped export:\n%s", csv)
	}
	// Token creation cannot widen scope.
	scopedAdmin := e.token("a-admin", []string{"finance.admin"}, []int64{a}, nil, false)
	e.must(422, scopedAdmin, "POST", "/v1/tokens", obj{"kind": "service", "subject": "x", "scopes": []string{"finance.read"}}, nil)
	e.must(422, scopedAdmin, "POST", "/v1/tokens", obj{"kind": "service", "subject": "x", "scopes": []string{"finance.read"}, "parties": []int64{a, b}}, nil)
	e.must(201, scopedAdmin, "POST", "/v1/tokens", obj{"kind": "service", "subject": "x", "scopes": []string{"finance.read"}, "parties": []int64{a}}, nil)
	e.must(403, scopedA, "POST", "/v1/tokens", obj{"kind": "service", "subject": "x", "scopes": []string{"finance.read"}, "parties": []int64{a}}, nil) // not admin
	// Scope enforcement per permission.
	reader := e.token("reader", []string{"finance.read"}, nil, nil, false)
	e.must(403, reader, "POST", "/v1/parties", obj{"name": "x"}, nil)
	e.must(403, reader, "POST", "/v1/invoices", obj{}, nil)
	e.must(403, maker, "GET", "/v1/tokens", nil, nil)
	e.must(403, maker, "GET", "/v1/audit", nil, nil)
	e.must(403, boss, "POST", "/v1/invoices", obj{}, nil) // approve is not write
	e.must(401, "", "GET", "/v1/invoices", nil, nil)
	e.must(401, "wrong-token", "GET", "/v1/invoices", nil, nil)
}

func TestAuthThrottleNeverBlocksValidCredentials(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *Options) { c.RateLimit.AuthFailuresPerMin = 5 })
	e.app.handler = e.h
	var saw429 bool
	for i := 0; i < 30; i++ {
		code, _, _ := e.req(fmt.Sprintf("bad-token-%d", i), "GET", "/v1/me", nil)
		saw429 = saw429 || code == 429
	}
	if !saw429 {
		t.Fatal("a flood of bad tokens should be throttled")
	}
	// From the same address, a valid token still works every time.
	for i := 0; i < 20; i++ {
		if code, _, _ := e.req(adminTok, "GET", "/v1/me", nil); code != 200 {
			t.Fatalf("valid token got %d after the flood", code)
		}
	}
}

func TestAuditAppendOnlyAndSensitiveReads(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	p := e.party(maker, "Acme Fake Ltd", "customer")
	count := func(action string) int {
		var n int
		_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n)
		return n
	}
	e.must(200, maker, "GET", "/v1/parties/"+id(p), nil, nil)
	e.must(200, maker, "GET", "/v1/parties", nil, nil)
	e.must(200, maker, "GET", "/v1/exports/invoices.csv", nil, nil)
	e.must(200, maker, "GET", "/v1/exports/payments.csv", nil, nil)
	e.must(200, maker, "GET", "/v1/exports/expenses.csv", nil, nil)
	for _, a := range []string{"party.read", "party.list", "export.invoices", "export.payments", "export.expenses", "token.create"} {
		if count(a) == 0 {
			t.Errorf("no audit entry for %s", a)
		}
	}
	var log struct{ Items []AuditEntry }
	e.must(200, adminTok, "GET", "/v1/audit", nil, &log)
	for _, it := range log.Items {
		if strings.Contains(string(it.Detail), "example.test") || strings.Contains(string(it.Detail), "cft_") {
			t.Fatalf("audit entry holds contact data or a secret: %s", it.Detail)
		}
	}
	if _, err := e.app.store.db.Exec(`UPDATE audit_log SET actor = 'x'`); err == nil {
		t.Error("audit entries must not be updatable")
	}
	if _, err := e.app.store.db.Exec(`DELETE FROM audit_log`); err == nil {
		t.Error("audit entries must not be deletable")
	}
	// Even through the purge gate, entries younger than a year stay.
	if _, err := e.app.store.Purge(t.Context(), 7, 365); err != nil {
		t.Fatal(err)
	}
	if count("party.read") == 0 {
		t.Error("purge removed a recent audit entry")
	}
}

func TestOwnOnlyTimesheetsAndExpenses(t *testing.T) {
	e := newEnv(t)
	alice := e.token("alice", rw, nil, nil, true)
	bob := e.token("bob", rw, nil, nil, true)
	var ts1, ts2 Timesheet
	e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-23"}, &ts1)
	e.must(201, bob, "POST", "/v1/timesheets", obj{"week_start": "2026-11-23"}, &ts2)
	// Alice cannot open a week for Bob, read, edit or submit Bob's.
	e.must(404, alice, "POST", "/v1/timesheets", obj{"owner": "bob", "week_start": "2026-11-16"}, nil)
	for _, c := range [][3]string{{"GET", "", ""}, {"PUT", "/entries", `{"entries":[]}`}, {"POST", "/submit", ""}, {"POST", "/approve", ""}} {
		code, _, _ := e.req(alice, c[0], "/v1/timesheets/"+id(ts2.ID)+c[1], c[2])
		if code != 404 && code != 403 {
			t.Errorf("%s %s on someone else's timesheet = %d", c[0], c[1], code)
		}
	}
	var list struct{ Items []Timesheet }
	e.must(200, alice, "GET", "/v1/timesheets", nil, &list)
	if len(list.Items) != 1 || list.Items[0].Owner != "alice" {
		t.Fatalf("own-only list %+v", list.Items)
	}
	e.must(200, alice, "GET", "/v1/timesheets?owner=bob", nil, &list)
	if len(list.Items) != 0 {
		t.Fatal("owner filter widened the scope")
	}
	var ex Expense
	e.must(201, bob, "POST", "/v1/expenses", validExpense(), &ex)
	e.must(404, alice, "GET", "/v1/expenses/"+id(ex.ID), nil, nil)
	e.must(404, alice, "PUT", "/v1/expenses/"+id(ex.ID), validExpense(), nil)
	// Own-only tokens have no business routes.
	for _, p := range []string{"/v1/invoices", "/v1/parties", "/v1/contracts", "/v1/reports/aging", "/v1/purchase-orders"} {
		e.must(403, alice, "GET", p, nil, nil)
	}
	e.must(403, alice, "POST", "/v1/timesheets/billing-draft", obj{}, nil)
}

func validExpense() obj {
	return obj{"description": "Train ticket", "amount_minor": 4500, "currency": "USD", "category": "travel", "incurred_on": "2026-11-20",
		"receipt": obj{"filename": "ticket.pdf", "content_type": "application/pdf", "size_bytes": 1234, "sha256": strings.Repeat("ab", 32)}}
}

func TestPublicLink(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	p := e.party(maker, "Acme <b>Fake</b> Ltd", "customer")
	inv := e.invoice(maker, p, nil, line(`<script>alert(1)</script> work`, 1000, 12345))
	// no link for a draft
	e.must(409, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link", nil, nil)
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	var link struct {
		URL       string `json:"url"`
		ExpiresAt time.Time
	}
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link", obj{"ttl_hours": 24}, &link)
	path := strings.TrimPrefix(link.URL, "https://billing.example.test")
	code, hdr, body := e.req("", "GET", path, nil)
	html := string(body)
	if code != 200 || !strings.Contains(html, "INV-00001") || !strings.Contains(html, "123.45") || !strings.Contains(html, "Pay to IBAN XX00") {
		t.Fatalf("public view %d:\n%s", code, html)
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "<b>Fake") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("output is not escaped:\n%s", html)
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", hdr)
	}
	// Views are audited, coalesced.
	e.req("", "GET", path, nil)
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'invoice.public_view'`).Scan(&n)
	if n != 1 {
		t.Errorf("public views audited %d times, want 1 (coalesced)", n)
	}

	tok := strings.TrimPrefix(path, "/public/invoices/")
	bad := []string{tok + "x", "x" + tok, strings.Replace(tok, ".", "A.", 1), "", "abc.def", SignLink([]byte("other-secret-other-secret-other-secret!"), inv.ID, 0, e.now.Add(time.Hour)),
		SignLink([]byte(linkSec), inv.ID+1, 0, e.now.Add(time.Hour)), SignLink([]byte(linkSec), inv.ID, 5, e.now.Add(time.Hour))}
	for _, b := range bad {
		if code, _, _ := e.req("", "GET", "/public/invoices/"+b, nil); code != 404 {
			t.Errorf("token %.20q = %d, want 404", b, code)
		}
	}
	// expiry
	e.advance(25 * time.Hour)
	if code, _, _ := e.req("", "GET", path, nil); code != 404 {
		t.Errorf("expired link = %d", code)
	}
	// revocation
	var l2 struct{ URL string }
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link", nil, &l2)
	p2 := strings.TrimPrefix(l2.URL, "https://billing.example.test")
	if code, _, _ := e.req("", "GET", p2, nil); code != 200 {
		t.Fatalf("fresh link = %d", code)
	}
	e.must(200, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link/revoke", nil, nil)
	if code, _, _ := e.req("", "GET", p2, nil); code != 404 {
		t.Errorf("revoked link = %d", code)
	}
	e.must(422, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link", obj{"ttl_hours": 100000}, nil)
	// void is shown as void
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/void", obj{"reason": "test"}, nil)
	var l3 struct{ URL string }
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link", nil, &l3)
	_, _, vb := e.req("", "GET", strings.TrimPrefix(l3.URL, "https://billing.example.test"), nil)
	if !strings.Contains(string(vb), "VOID") {
		t.Error("void invoice not marked")
	}
}

func TestPublicLinkValidNeverThrottledBadOnesAre(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *Options) { c.RateLimit.AuthFailuresPerMin = 5 })
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	p := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, p, nil, line("x", 1000, 100))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	var l struct{ URL string }
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link", nil, &l)
	path := strings.TrimPrefix(l.URL, "https://billing.example.test")
	saw := false
	for i := 0; i < 20; i++ {
		c, _, _ := e.req("", "GET", fmt.Sprintf("/public/invoices/bad%d.bad", i), nil)
		saw = saw || c == 429
	}
	if !saw {
		t.Error("guessing links should be throttled")
	}
	for i := 0; i < 20; i++ {
		if c, _, _ := e.req("", "GET", path, nil); c != 200 {
			t.Fatalf("valid link got %d", c)
		}
	}
}

func TestCSVFormulaInjection(t *testing.T) {
	for in, want := range map[string]string{"=1+1": "'=1+1", "+cmd": "'+cmd", "-2": "'-2", "@SUM(A1)": "'@SUM(A1)", "\tx": "'\tx", "  =x": "'  =x", "plain": "plain", "a=b": "a=b", "": ""} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	p := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, p, obj{"project": "ACME"}, line("x", 1000, 100))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 50, "reference": "=HYPERLINK(\"http://evil.invalid\")", "method": "@bank"}, nil)
	_, hdr, csv := e.req(maker, "GET", "/v1/exports/payments.csv", nil)
	s := string(csv)
	if !strings.Contains(hdr.Get("Content-Type"), "text/csv") || strings.Contains(s, ",=HYPERLINK") || !strings.Contains(s, "'=HYPERLINK") || !strings.Contains(s, "'@bank") {
		t.Fatalf("payments csv:\n%s", s)
	}
	// numbers are written as numbers, not defused
	e.must(201, maker, "POST", "/v1/expenses", obj{"description": "=cmd|' /C calc'!A0", "amount_minor": 4500, "currency": "USD", "category": "travel", "incurred_on": "2026-11-20"}, nil)
	_, _, ex := e.req(maker, "GET", "/v1/exports/expenses.csv", nil)
	if !strings.Contains(string(ex), "'=cmd") || !strings.Contains(string(ex), ",45.00,") {
		t.Fatalf("expenses csv:\n%s", ex)
	}
}

func TestErasureLegalRetentionAndWALScrub(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	p := e.party(maker, "Erase Me Fake", "customer")
	issued := e.invoice(maker, p, nil, line("x", 1000, 100))
	e.must(200, boss, "POST", "/v1/invoices/"+id(issued.ID)+"/approve", nil, nil)
	draft := e.invoice(maker, p, nil, line("d", 1000, 100))

	var rep ErasureReport
	e.must(403, maker, "POST", "/v1/parties/"+id(p)+"/erase", nil, nil) // admin only
	e.must(200, adminTok, "POST", "/v1/parties/"+id(p)+"/erase", nil, &rep)
	if rep.DeletedDrafts != 1 || len(rep.Retained) != 1 || rep.Retained[0].Class != "finance-invoice" || rep.Retained[0].Until != "2033-11-25" {
		t.Fatalf("report %+v", rep)
	}
	// No contact data lingers in the database files, WAL included.
	for _, f := range []string{"f.db", "f.db-wal"} {
		b, err := os.ReadFile(e.dir + "/" + f)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "erase.me.fake@example.test") {
			t.Errorf("%s still holds the erased email", f)
		}
	}
	if st, err := os.Stat(e.dir + "/f.db-wal"); err == nil && st.Size() != 0 {
		t.Errorf("WAL not truncated: %d bytes", st.Size())
	}
	e.must(404, maker, "GET", "/v1/invoices/"+id(draft.ID), nil, nil)
	var party Party
	e.must(200, maker, "GET", "/v1/parties/"+id(p), nil, &party)
	if !party.Erased || party.Email != "" || party.Address != "" || strings.Contains(party.Name, "Erase Me") {
		t.Fatalf("party after erasure %+v", party)
	}
	// The issued invoice keeps its legal snapshot (name and address, not email or phone) and cannot be edited.
	var inv Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(issued.ID), nil, &inv)
	if inv.PartySnapshot == nil || inv.PartySnapshot.Name != "Erase Me Fake" || inv.Status != "approved" {
		t.Fatalf("invoice after erasure %+v", inv)
	}
	e.must(409, maker, "PATCH", "/v1/parties/"+id(p), obj{"name": "back"}, nil)
	e.must(422, maker, "POST", "/v1/invoices", obj{"party_id": p, "lines": []obj{line("x", 1000, 1)}}, nil)
	// After the retention period the purge deletes the invoice and then the erased party.
	e.must(201, maker, "POST", "/v1/invoices/"+id(issued.ID)+"/payments", obj{"amount_minor": 100}, nil)
	e.advance(7*365*24*time.Hour + 10*24*time.Hour)
	r1, err := e.app.store.Purge(t.Context(), 7, 2555)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Invoices != 1 || r1.ErasedParties != 1 {
		t.Fatalf("purge %+v", r1)
	}
	e.must(404, maker, "GET", "/v1/invoices/"+id(issued.ID), nil, nil)
}

func TestPurgeKeepsOpenInvoicesAndRecentRecords(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	p := e.party(maker, "Acme Fake Ltd", "customer")
	open := e.invoice(maker, p, nil, line("open", 1000, 100))
	e.must(200, boss, "POST", "/v1/invoices/"+id(open.ID)+"/approve", nil, nil)
	e.advance(9 * 365 * 24 * time.Hour)
	rep, err := e.app.store.Purge(t.Context(), 7, 2555)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Invoices != 0 {
		t.Fatalf("an unpaid invoice was purged: %+v", rep)
	}
	e.must(200, maker, "GET", "/v1/invoices/"+id(open.ID), nil, nil)
}

func TestBodyLimitsAndStrictJSON(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *Options) { c.MaxBodyBytes = 2048 })
	maker := e.token("maker", rw, nil, nil, false)
	big := strings.Repeat("x", 5000)
	e.must(413, maker, "POST", "/v1/parties", obj{"name": "a", "notes": big}, nil)
	e.must(400, maker, "POST", "/v1/parties", `{"name":"a"}{"name":"b"}`, nil)
	e.must(400, maker, "POST", "/v1/parties", `{"name":"a","bogus":1}`, nil)
	e.must(400, maker, "POST", "/v1/parties", `not json`, nil)
	code, _, _ := e.req(maker, http.MethodGet, "/v1/invoices?cursor=!!!", nil)
	if code != 400 && code != 404 {
		t.Errorf("bad cursor = %d", code)
	}
}

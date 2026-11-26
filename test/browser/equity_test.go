//go:build browser

package browser

import (
	"bytes"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The introspected identities of the e2e overlay (dev/compose/config/introstub.json): equity has no
// directory in this stack, so only these carry a person and the board role.
const (
	eqHR      = "e2e-intro-hr-token-0123456789abcdef"
	eqBoard   = "e2e-intro-board-token-0123456789abcdef"
	eqFinance = "e2e-intro-finance-token-0123456789abcdef"
	eqHolder  = "e2e-intro-holder-token-0123456789abcdef"
	eqPerson  = "pers_0000000000000000000e17a1"
)

// equitySignIn opens a fresh browser signed in with a pasted token.
func equitySignIn(t *testing.T, r *run, token string) *session {
	t.Helper()
	s := newSession(t, r.vp)
	s.goTo(host("equity") + "/login")
	s.fill(`input[name="token"]`, token)
	s.submit("/login")
	if s.path() != "/" {
		t.Fatalf("equity token sign-in failed: %.300s", s.text())
	}
	return s
}

// stepUp confirms an irreversible form the way a person does and sends it.
func stepUp(s *session, action, token string) {
	s.t.Helper()
	form := `form[action="` + action + `"] `
	s.run(chromedp.Click(form+`input[name="confirm"]`, chromedp.ByQuery))
	s.fill(form+`input[name="token"]`, token)
	s.submit(action)
}

// equityFlow takes a stock option from draft to issued shares, each step by the person who takes it.
func equityFlow(t *testing.T, r *run) {
	svc := apiOf(t, "equity", "CAN_EQUITY_API_TOKEN")
	ent := svc.Post(t, "/v1/entities", map[string]any{"name": "Browser Inc " + r.u, "jurisdiction": "Delaware, US", "currency": "USD"}).Expect(t, "entity", 201).Str("id")
	class := svc.Post(t, "/v1/entities/"+ent+"/share-classes", map[string]any{"name": "Common", "kind": "common", "par_minor": 1, "currency": "USD", "authorised": "10000000"}).Expect(t, "class", 201).Str("id")
	plan := svc.Post(t, "/v1/entities/"+ent+"/plans", map[string]any{"name": "Browser plan with a long name for a phone " + r.u, "class_id": class, "authorised": "1000000", "adopted_on": "2025-01-02"}).Expect(t, "plan", 201).Str("id")
	name := "Hollie Holder-Averyveryveryverylongfamilyname " + r.u
	stk := svc.Post(t, "/v1/entities/"+ent+"/stakeholders", map[string]any{"kind": "employee", "person_id": eqPerson, "name": name, "email": "holder-" + r.u + "@example.test"}).Expect(t, "stakeholder", 201).Str("id")
	e := host("equity") + "/company/entities/" + ent

	// HR confirms the holder from the queue, drafts and submits.
	hr := equitySignIn(t, r, eqHR)
	hr.goTo(host("equity") + "/company/identity")
	hr.click(`a[href="/company/stakeholders/` + stk + `"]`)
	stepUp(hr, "/company/stakeholders/"+stk+"/attest", eqHR)
	hr.has("Holder attested as external.")

	hr.goTo(e + "/grants/new")
	hr.fill("#plan_id", plan)
	hr.fill("#stakeholder_id", stk)
	hr.fill("#quantity", "4800")
	hr.fill("#strike", "1.25")
	hr.fill("#grant_date", "2025-01-15")
	hr.fill("#vesting_start", "2025-01-15")
	hr.fill("#expiry_date", "2035-01-15")
	hr.submit("/company/entities/" + ent + "/grants")
	hr.has("4,800", "1.25 USD", "Draft")
	grant := hr.path()
	gid := grant[len("/company/grants/"):]
	hr.submit(grant + "/submit")
	hr.has("Waiting for approval")

	board := equitySignIn(t, r, eqBoard)
	board.goTo(host("equity") + grant)
	stepUp(board, grant+"/approve", eqBoard)
	board.has("Grant approved.")

	hr.goTo(host("equity") + grant)
	stepUp(hr, grant+"/activate", eqHR)
	hr.has("Grant activated.")

	// The holder: their own grant, its vesting today, the request.
	holder := equitySignIn(t, r, eqHolder)
	holder.goTo(host("equity") + "/me")
	holder.has("4,800", "Not yet vested")
	holder.goTo(host("equity") + "/me/grants/" + gid)
	holder.has("Vesting schedule", "not legal, tax or securities advice")
	holder.fill("#quantity", "100.5")
	holder.submit("/me/grants/" + gid + "/exercise/review")
	holder.has("125.63 USD") // 100.5 x 1.25 = 125.625, half away from zero
	holder.run(chromedp.Click("#accept", chromedp.ByQuery))
	holder.submit("/me/grants/" + gid + "/exercise")
	holder.has("Your request was sent.", "Requested")
	holder.goTo(host("equity") + "/me/grants/" + gid + "/statement")
	holder.has("Grant statement", "4,800")
	holder.goTo(host("equity") + "/me/documents")
	holder.crawl(host("equity")+"/", 15, skipCrawl)

	// Board approves the exercise, finance records the money, HR (a second person) issues.
	board.goTo(host("equity") + "/company/entities/" + ent + "/exercises")
	board.click(`a[href^="/company/exercises/"]`)
	exercise := board.path()
	board.submit(exercise + "/approve")
	board.has("Waiting for the payment.")

	fin := equitySignIn(t, r, eqFinance)
	fin.goTo(host("equity") + exercise)
	fin.fill("#reference", "WIRE-"+r.u)
	fin.fill("#paid_on", time.Now().UTC().Format("2006-01-02"))
	stepUp(fin, exercise+"/payment", eqFinance)
	fin.has("Payment recorded.")

	hr.goTo(host("equity") + exercise)
	stepUp(hr, exercise+"/issue", eqHR)
	hr.has("Shares issued.")

	fin.goTo(e + "/captable")
	fin.has("100.5", "100.0000%", name)
	fin.goTo(e)
	fin.goTo(e + "/exports")
	fin.fill("#name", "grants")
	file, b := fin.download(`form[action="/company/entities/` + ent + `/exports"] button`)
	if file != "equity-grants.csv" || !bytes.Contains(b, []byte(gid)) {
		t.Errorf("export %q: %.200q", file, b)
	}
	fin.crawl(host("equity")+"/", 25, skipCrawl)

	// The administrator arrives from Apps. Equity has no directory here, so their own account names no
	// person: the pages say so instead of offering what the policy would refuse.
	a := r.admin.in(t)
	open(a, "equity", equityURL())
	a.has("not linked to a person")
	a.goTo(equityURL() + "/company/entities/" + ent + "/grants")
	a.has(name)
	a.crawl(equityURL()+"/", 20, skipCrawl)
}

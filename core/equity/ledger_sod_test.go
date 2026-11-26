package equity

import "testing"

// A manual ledger entry is refused when the caller's directory person is a holder in it.
func TestLedgerWritesRefuseTheHolder(t *testing.T) {
	e := newEnv(t).base()
	// eve holds shares, issued by someone else.
	issue := func(a Actor, stk string) error {
		_, _, err := e.s.IssueNewShares(e.ctx, a, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: stk, Quantity: q(100), EffectiveDate: "2026-01-01"}, "", nil)
		return err
	}
	e.must(issue(e.fin, e.eveStk.ID))
	e.must(issue(e.fin, e.founder.ID))

	eve := Actor{Subject: "sso-eve", Roles: []string{RoleFinance, RoleAdmin}, PersonID: personEve}
	transfer := func(a Actor, from, to string) error {
		_, _, err := e.s.TransferShares(e.ctx, a, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: from, ToID: to, Quantity: q(1), EffectiveDate: "2026-01-02"}, "", nil)
		return err
	}
	cancel := func(a Actor, stk string) error {
		_, _, err := e.s.CancelShares(e.ctx, a, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: stk, Quantity: q(1), EffectiveDate: "2026-01-02"}, "", nil)
		return err
	}
	for name, err := range map[string]error{
		"issue to herself":     issue(eve, e.eveStk.ID),
		"transfer from":        transfer(eve, e.eveStk.ID, e.founder.ID),
		"transfer to":          transfer(eve, e.founder.ID, e.eveStk.ID),
		"cancel her own":       cancel(eve, e.eveStk.ID),
		"subject is person id": issue(Actor{Subject: personEve, Roles: []string{RoleFinance}}, e.eveStk.ID),
	} {
		if !isErr(err, ErrSeparation) {
			t.Errorf("%s: want separation of duties, got %v", name, err)
		}
	}
	page, err := e.s.ListLedger(e.ctx, e.fin, e.ent.ID, "", 0, 0)
	e.must(err)
	if len(page.Items) != 2 {
		t.Fatalf("a refused entry must leave nothing behind (a transfer writes two rows): %d rows", len(page.Items))
	}
	// Entries that do not involve her, and entries by others on her holding, still work.
	e.must(issue(eve, e.founder.ID))
	e.must(transfer(e.fin, e.eveStk.ID, e.founder.ID))
	e.must(cancel(e.fin, e.eveStk.ID))
	// A caller with no person_id cannot be compared: documented in docs/equity.md.
	e.must(cancel(Actor{Subject: "sso-eve", Roles: []string{RoleFinance}, CanCore: true}, e.eveStk.ID))
}

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

// Refusals are audited.
func TestRefusedLedgerWritesAreAudited(t *testing.T) {
	e := newEnv(t).base()
	eve := Actor{Subject: "sso-eve", Roles: []string{RoleFinance}, PersonID: personEve}
	in := LedgerInput{ClassID: e.class.ID, StakeholderID: e.eveStk.ID, Quantity: q(1), EffectiveDate: "2026-01-01"}
	denied := func(action string) int {
		return e.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied' AND action = ? AND resource = ?`, action, "entity:"+e.ent.ID)
	}
	if _, _, err := e.s.IssueNewShares(e.ctx, eve, e.ent.ID, in, "", nil); !isErr(err, ErrSeparation) {
		t.Fatal(err)
	}
	if _, _, err := e.s.CancelShares(e.ctx, e.fin, e.ent.ID, in, "", nil); !isErr(err, ErrConflict) { // holds nothing
		t.Fatal(err)
	}
	if denied("ledger.issue") != 1 || denied("ledger.cancel") != 1 {
		t.Fatalf("refusals not audited: issue %d, cancel %d", denied("ledger.issue"), denied("ledger.cancel"))
	}
	e.must(e.s.VerifyAudit(e.ctx))
}

// A holder cannot remove the link the checks compare by erasing their own record.
func TestHolderCannotEraseOwnRecord(t *testing.T) {
	e := newEnv(t).base()
	eve := Actor{Subject: "sso-eve", Roles: []string{RoleAdmin, RoleFinance}, PersonID: personEve}
	if _, err := e.s.RequestErasure(e.ctx, eve, e.eveStk.ID); !isErr(err, ErrSeparation) {
		t.Fatalf("want separation of duties, got %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM stakeholders WHERE id = ? AND person_id = ? AND erased_at IS NULL`, e.eveStk.ID, personEve); n != 1 {
		t.Fatal("the record was changed")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied' AND action = 'erase' AND resource = ?`, "stakeholder:"+e.eveStk.ID) != 1 {
		t.Fatal("the refused erasure was not audited")
	}
	if _, err := e.s.RequestErasure(e.ctx, e.admin, e.eveStk.ID); err != nil {
		t.Fatalf("another admin may erase it: %v", err)
	}
}

// Shares go only to a holder tied to a person or attested external by a second actor, so a second,
// unlinked record of one's own cannot receive them unnoticed.
func TestSharesNeedALinkedOrAttestedRecipient(t *testing.T) {
	e := newEnv(t).base()
	eve := Actor{Subject: "sso-eve", Roles: []string{RoleFinance, RoleHR}, PersonID: personEve}
	second := m(e.s.CreateStakeholder(e.ctx, eve, e.ent.ID, StakeholderInput{Kind: "investor", Identity: Identity{Name: "Eve Holdings"}}))
	e.must(func() error {
		_, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, Quantity: q(10), EffectiveDate: "2026-01-01"}, "", nil)
		return err
	}())
	issue := func() error {
		_, _, err := e.s.IssueNewShares(e.ctx, eve, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: second.ID, Quantity: q(5), EffectiveDate: "2026-01-01"}, "", nil)
		return err
	}
	transfer := func() error {
		_, _, err := e.s.TransferShares(e.ctx, eve, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, ToID: second.ID, Quantity: q(5), EffectiveDate: "2026-01-02"}, "", nil)
		return err
	}
	if err := issue(); !isErr(err, ErrConflict) {
		t.Fatalf("issue to an unlinked record: %v", err)
	}
	if err := transfer(); !isErr(err, ErrConflict) {
		t.Fatalf("transfer to an unlinked record: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM ledger`); n != 1 {
		t.Fatalf("a refused entry left rows: %d", n)
	}
	if _, err := e.s.AttestExternal(e.ctx, eve, second.ID); !isErr(err, ErrSeparation) {
		t.Fatalf("the creator cannot attest: %v", err)
	}
	m(e.s.AttestExternal(e.ctx, e.hr, second.ID))
	e.must(issue())
	e.must(transfer())
}

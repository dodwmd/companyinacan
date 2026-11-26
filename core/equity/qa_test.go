package equity

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

// Regression tests for the QA findings on PR #27.

// txFailAuditor can join transactions and refuses to write: a mutation must then not commit.
type txFailAuditor struct{ failingAuditor }

func (txFailAuditor) RecordTx(context.Context, *sql.Tx, []AuditEvent) error {
	return errors.New("audit unavailable")
}

func TestUnlinkedHolderCannotBeUsedToSidestepTheBoardCheck(t *testing.T) {
	e := newEnv(t).base()
	// a stakeholder with no directory person, created by finance and not yet attested
	stranger := m(e.s.CreateStakeholder(e.ctx, e.fin, e.ent.ID, StakeholderInput{Kind: "investor", Identity: Identity{Name: "Sam Stranger"}}))
	id := e.draft(e.hr, stranger.ID, q(10))
	if _, err := e.s.SubmitGrant(e.ctx, e.hr, id); !isErr(err, ErrConflict) {
		t.Fatalf("submitting a grant for an unlinked holder: %v", err)
	}
	// the creator of the stakeholder cannot attest it, nor can a holder-less non-staff role
	if _, err := e.s.AttestExternal(e.ctx, e.fin, stranger.ID); !isErr(err, ErrSeparation) {
		t.Fatalf("creator attesting: %v", err)
	}
	if _, err := e.s.AttestExternal(e.ctx, Actor{Subject: "FIN-Frank ", Roles: []string{RoleFinance}}, stranger.ID); !isErr(err, ErrSeparation) {
		t.Fatalf("creator (variant) attesting: %v", err)
	}
	if _, err := e.s.AttestExternal(e.ctx, e.eve, stranger.ID); !isErr(err, ErrForbidden) {
		t.Fatalf("employee attesting: %v", err)
	}
	if _, err := e.s.db.Exec(`UPDATE stakeholders SET external_attested_by = created_by WHERE id = ?`, stranger.ID); err == nil {
		t.Fatal("the database accepted the creator as attester")
	}
	// an established holder cannot be attested again
	if _, err := e.s.AttestExternal(e.ctx, e.admin, e.eveStk.ID); !isErr(err, ErrConflict) {
		t.Fatalf("attesting a linked holder: %v", err)
	}
	st := m(e.s.AttestExternal(e.ctx, e.admin, stranger.ID))
	if !st.ExternalAttested {
		t.Fatal("not attested")
	}
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	// whoever attested the holder may approve (two actors are on the record already); it is on record
	attester := Actor{Subject: "svc-admin", Roles: []string{RoleBoard}}
	m(e.s.ApproveGrant(e.ctx, attester, id))
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'grant.approve.by_record_establisher'`) != 1 {
		t.Fatal("an approval by the attester is not audited as such")
	}
	// and the link cannot be removed behind the service's back by approval time
	id2 := e.draft(e.hr, stranger.ID, q(10))
	m(e.s.SubmitGrant(e.ctx, e.hr, id2))
	if _, err := e.s.db.Exec(`UPDATE stakeholders SET external_attested_by = NULL WHERE id = ?`, stranger.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ApproveGrant(e.ctx, e.board, id2); !isErr(err, ErrConflict) {
		t.Fatalf("approval without a link: %v", err)
	}
}

func TestGranteeCannotAdministerTheirOwnGrant(t *testing.T) {
	e := newEnv(t).base()
	// the attack: one token holding hr (and admin) and the employee role, linked to eve's own record
	evil := Actor{Subject: "sso-eve", Roles: []string{RoleEmployee, RoleHR, RoleAdmin}, PersonID: personEve}
	in := e.standardInput(e.eveStk.ID, q(100))
	if _, _, err := e.s.CreateGrant(e.ctx, evil, e.plan.ID, in, "", nil); !isErr(err, ErrSeparation) {
		t.Fatalf("drafting own grant: %v", err)
	}
	// a grant made by someone else
	id := e.draft(e.hr, e.eveStk.ID, q(4800))
	qty := q(1)
	steps := map[string]func() error{
		"update": func() error { _, err := e.s.UpdateGrant(e.ctx, evil, id, GrantPatch{Quantity: &qty}); return err },
		"submit": func() error { _, err := e.s.SubmitGrant(e.ctx, evil, id); return err },
		"delete": func() error { return e.s.DeleteGrant(e.ctx, evil, id) },
	}
	for name, f := range steps {
		if err := f(); !isErr(err, ErrSeparation) {
			t.Errorf("%s own grant: %v", name, err)
		}
	}
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	m(e.s.ApproveGrant(e.ctx, e.board, id))
	steps = map[string]func() error{
		"activate":  func() error { _, err := e.s.ActivateGrant(e.ctx, evil, id, ""); return err },
		"amend":     func() error { _, err := e.s.AmendGrant(e.ctx, evil, id); return err },
		"cancel":    func() error { _, err := e.s.CancelGrant(e.ctx, evil, id, "x"); return err },
		"terminate": func() error { _, err := e.s.TerminateGrant(e.ctx, evil, id, "2026-03-10", false); return err },
	}
	for name, f := range steps {
		if err := f(); !isErr(err, ErrSeparation) {
			t.Errorf("%s own grant: %v", name, err)
		}
	}
	m(e.s.ActivateGrant(e.ctx, e.hr, id, ""))
	// vesting facts: not by the evil token, and not by the grantor chain either
	in2 := e.standardInput(e.eveStk.ID, q(1000))
	in2.Schedule = Schedule{Kind: "tranches", Tranches: []Tranche{{Label: "beta", BasisPoints: 10000, Milestone: true}},
		Acceleration: []Acceleration{{Name: "coc", Type: "single", Triggers: []string{"change_of_control"}, PercentBP: 10000}}}
	g2, _, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in2, "", nil)
	e.must(err)
	m(e.s.SubmitGrant(e.ctx, e.hr, g2))
	m(e.s.ApproveGrant(e.ctx, e.board, g2))
	m(e.s.ActivateGrant(e.ctx, e.hr, g2, ""))
	if err := e.s.RecordMilestone(e.ctx, evil, g2, 0, "2026-03-10"); !isErr(err, ErrSeparation) {
		t.Fatalf("grantee recording a milestone: %v", err)
	}
	if err := e.s.RecordMilestone(e.ctx, e.hr, g2, 0, "2026-03-10"); !isErr(err, ErrSeparation) {
		t.Fatalf("grantor recording a milestone: %v", err)
	}
	if err := e.s.RecordAcceleration(e.ctx, e.hr, g2, "change_of_control", "2026-03-10"); !isErr(err, ErrSeparation) {
		t.Fatalf("grantor recording an acceleration: %v", err)
	}
	if _, err := e.s.TerminateGrant(e.ctx, e.hr, g2, "2026-03-10", true); !isErr(err, ErrSeparation) {
		t.Fatalf("grantor terminating: %v", err)
	}
	if v := m(e.s.Vesting(e.ctx, e.hr, g2, "")); v.Vested != 0 {
		t.Fatalf("vesting moved without a second actor: %s", v.Vested)
	}
	// a second actor can, within the window and not before approval
	if err := e.s.RecordMilestone(e.ctx, e.hr2, g2, 0, "2026-03-09"); err == nil {
		t.Fatal("a milestone dated before the approval was accepted")
	}
	e.advance("2026-07-01")
	if err := e.s.RecordMilestone(e.ctx, e.hr2, g2, 0, "2026-03-10"); err == nil {
		t.Fatal("a milestone dated beyond the backdating window was accepted")
	}
	if err := e.s.RecordAcceleration(e.ctx, e.hr2, g2, "change_of_control", "2026-03-10"); err == nil {
		t.Fatal("an acceleration dated beyond the backdating window was accepted")
	}
	e.must(e.s.RecordMilestone(e.ctx, e.hr2, g2, 0, "2026-06-15"))
	if v := m(e.s.Vesting(e.ctx, e.hr, g2, "")); v.Vested != q(1000) {
		t.Fatalf("vested = %s", v.Vested)
	}
	// the holder still has their legitimate self-service: requesting an exercise on their own grant
	if _, _, err := e.s.RequestExercise(e.ctx, evil, g2, q(1), "", nil); err != nil {
		t.Fatalf("self-service exercise: %v", err)
	}
}

func TestFreeTextReasonsDoNotReachTheHolder(t *testing.T) {
	e := newEnv(t).base()
	id := e.draft(e.hr, e.eveStk.ID, q(100))
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	m(e.s.RejectGrant(e.ctx, e.board, id, "performance concerns, do not grant"))
	var reason string
	e.must(e.s.db.QueryRow(`SELECT terminal_reason FROM grants WHERE id = ?`, id).Scan(&reason))
	if reason == "" {
		t.Fatal("reason not stored")
	}
	if _, err := e.s.GetGrant(e.ctx, e.eve, id); !isErr(err, ErrNotFound) {
		t.Fatalf("the holder saw a rejected draft: %v", err)
	}
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	e.must(e.s.db.QueryRow(`SELECT terminal_reason FROM grants WHERE id = ?`, id).Scan(&reason))
	if reason != "" {
		t.Fatalf("the rejection reason survived resubmission: %q", reason)
	}
	m(e.s.ApproveGrant(e.ctx, e.board, id))
	m(e.s.CancelGrant(e.ctx, e.hr, id, "performance concerns, let go"))
	own := m(e.s.GetGrant(e.ctx, e.eve, id))
	if own.TerminalReason != "" || own.Status != GrantCancelled {
		t.Fatalf("holder view = %+v", own)
	}
	if hr := m(e.s.GetGrant(e.ctx, e.hr, id)); hr.TerminalReason == "" {
		t.Fatal("hr lost the reason")
	}
	page := m(e.s.MyGrants(e.ctx, e.eve, GrantFilter{}))
	b, _ := json.Marshal(page)
	if bytes.Contains(b, []byte("performance")) {
		t.Fatalf("my grants leaks the reason: %s", b)
	}
	// superseded grants name the replacement only to staff
	old := e.active(e.eveStk.ID, q(10))
	nv := m(e.s.AmendGrant(e.ctx, e.hr, old))
	m(e.s.SubmitGrant(e.ctx, e.hr, nv.ID))
	m(e.s.ApproveGrant(e.ctx, e.board, nv.ID))
	if g := m(e.s.GetGrant(e.ctx, e.eve, old)); g.TerminalReason != "" {
		t.Fatalf("holder view of a superseded grant: %+v", g)
	}
}

func TestMutationAndAuditCommitTogether(t *testing.T) {
	e := newEnv(t).base()
	gid := e.draft(e.hr, e.eveStk.ID, q(10))
	e.s.audit = txFailAuditor{}
	entities := e.count(`SELECT COUNT(*) FROM entities`)
	if _, err := e.s.CreateEntity(e.ctx, e.board, EntityInput{Name: "Other", Currency: "USD"}); err == nil {
		t.Fatal("entity created although its audit entry could not be written")
	}
	if e.count(`SELECT COUNT(*) FROM entities`) != entities {
		t.Fatal("the entity committed without its audit entry")
	}
	if _, err := e.s.SubmitGrant(e.ctx, e.hr, gid); err == nil {
		t.Fatal("submit succeeded")
	}
	if g := m(e.s.loadGrant(e.ctx, e.s.db, gid)); g.Status != GrantDraft {
		t.Fatalf("grant moved to %s without an audit entry", g.Status)
	}
	// an idempotent create leaves neither the record nor the key behind
	in := e.standardInput(e.eveStk.ID, q(5))
	body, _ := json.Marshal(in)
	if _, _, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in, "key-x", body); err == nil {
		t.Fatal("create succeeded")
	}
	if e.count(`SELECT COUNT(*) FROM idempotency`) != 0 || e.count(`SELECT COUNT(*) FROM grants`) != 1 {
		t.Fatal("a failed create left rows behind")
	}
	// legal hold and erasure are audited in the same transaction too
	if err := e.s.SetLegalHold(e.ctx, e.admin, e.founder.ID, true, "x"); err == nil {
		t.Fatal("hold set without an audit entry")
	}
	if st := m(e.s.stakeholder(e.ctx, e.s.db, e.founder.ID)); st.LegalHold {
		t.Fatal("hold committed")
	}
	// and with a working auditor, the idempotency row and the grant commit together
	e.s.audit = &SQLAuditor{db: e.s.db, now: e.s.now}
	id, _, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in, "key-x", body)
	e.must(err)
	if e.count(`SELECT COUNT(*) FROM idempotency WHERE resource_id = ?`, id) != 1 {
		t.Fatal("idempotency row missing")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'grant.create' AND resource = ?`, "grant:"+id) != 1 {
		t.Fatal("audit row missing")
	}
	e.must(e.s.VerifyAudit(e.ctx))
}

func TestExportFailsInsteadOfTruncating(t *testing.T) {
	e := newEnv(t).base()
	old := exportLimit
	exportLimit = 2
	defer func() { exportLimit = old }()
	for i := 0; i < 3; i++ {
		e.draft(e.hr, e.eveStk.ID, q(1))
		_, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, Quantity: q(1), EffectiveDate: "2026-01-01"}, "", nil)
		e.must(err)
	}
	for _, name := range []string{"grants", "ledger"} {
		var buf bytes.Buffer
		if err := e.s.ExportCSV(e.ctx, e.fin, e.ent.ID, name, "", &buf); !isErr(err, ErrConflict) || buf.Len() != 0 {
			t.Errorf("%s export of too many rows: %v (%d bytes)", name, err, buf.Len())
		}
	}
	exportLimit = 3
	var buf bytes.Buffer
	e.must(e.s.ExportCSV(e.ctx, e.fin, e.ent.ID, "ledger", "", &buf))
}

func TestStakeholderCreatorAndOriginalSubmitterStayInTheChain(t *testing.T) {
	e := newEnv(t).base()
	// a person holding finance and board creates an external holder; a second person attests it
	fb := Actor{Subject: "fin-board-fred", Roles: []string{RoleFinance, RoleBoard}}
	st := m(e.s.CreateStakeholder(e.ctx, fb, e.ent.ID, StakeholderInput{Kind: "investor", Identity: Identity{Name: "Ivy Investor"}}))
	m(e.s.AttestExternal(e.ctx, e.admin, st.ID))
	id := e.draft(e.hr, st.ID, q(10))
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	// two actors established the record, so its creator may approve a grant somebody else wrote
	// (specs/051-holder-identity); it is on record
	m(e.s.ApproveGrant(e.ctx, fb, id))
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'grant.approve.by_record_establisher'`) != 1 {
		t.Fatal("an approval by the record's creator is not audited as such")
	}
	// a board member cannot register themself as a holder
	self := Actor{Subject: "sso-bob", Roles: []string{RoleBoard}, PersonID: personBob}
	if _, err := e.s.CreateStakeholder(e.ctx, self, e.ent.ID, StakeholderInput{Kind: "advisor", PersonID: personBob, Identity: Identity{Name: "Bob"}}); !isErr(err, ErrSeparation) {
		t.Fatalf("board member registering themself: %v", err)
	}
	// reject and resubmit by someone else does not launder the first submitter
	id2 := e.draft(e.hr2, e.eveStk.ID, q(10))
	m(e.s.SubmitGrant(e.ctx, e.hr, id2))
	m(e.s.RejectGrant(e.ctx, e.board, id2, "again"))
	m(e.s.SubmitGrant(e.ctx, Actor{Subject: "hr-third", Roles: []string{RoleHR}}, id2))
	firstSubmitter := Actor{Subject: "HR-Alice", Roles: []string{RoleBoard}}
	if _, err := e.s.ApproveGrant(e.ctx, firstSubmitter, id2); !isErr(err, ErrSeparation) {
		t.Fatalf("the first submitter approving after a resubmission: %v", err)
	}
	if _, err := e.s.ApproveGrant(e.ctx, e.board, id2); err != nil {
		t.Fatal(err)
	}
}

func TestTerminationBackdatingIsBounded(t *testing.T) {
	e := newEnv(t).base()
	id := e.active(e.eveStk.ID, q(4800))
	e.advance("2026-09-01")
	if _, err := e.s.TerminateGrant(e.ctx, e.hr2, id, "2026-03-11", false); err == nil {
		t.Fatal("a termination dated months back was accepted")
	}
	if _, err := e.s.TerminateGrant(e.ctx, e.hr2, id, "2026-06-15", false); err != nil {
		t.Fatalf("a termination inside the window: %v", err)
	}
}

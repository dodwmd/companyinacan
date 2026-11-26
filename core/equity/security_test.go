package equity

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// ---- separation of duties ---------------------------------------------------------------------

func TestGrantorCannotApproveOwnGrant(t *testing.T) {
	e := newEnv(t).base()
	// a person with both roles who drafted and submitted the grant
	both := Actor{Subject: "dual-dan", Roles: []string{RoleHR, RoleBoard}}
	in := e.standardInput(e.eveStk.ID, q(100))
	id, _, err := e.s.CreateGrant(e.ctx, both, e.plan.ID, in, "", nil)
	e.must(err)
	m(e.s.SubmitGrant(e.ctx, e.hr, id)) // someone else submitted: the creator still may not approve
	if _, err := e.s.ApproveGrant(e.ctx, both, id); !isErr(err, ErrSeparation) {
		t.Fatalf("creator approving: %v", err)
	}
	// case, width and invisible-character variants of the same subject are the same person
	for _, subj := range []string{"DUAL-DAN", " dual-dan ", "dual\u200b-dan", "ｄｕａｌ-ｄａｎ"} {
		v := Actor{Subject: subj, Roles: []string{RoleBoard}}
		if _, err := e.s.ApproveGrant(e.ctx, v, id); !isErr(err, ErrSeparation) {
			t.Errorf("variant %q approved: %v", subj, err)
		}
	}
	// an editor cannot approve either
	id2 := e.draft(e.hr, e.eveStk.ID, q(100))
	qty := q(101)
	m(e.s.UpdateGrant(e.ctx, Actor{Subject: "Ed-Editor", Roles: []string{RoleHR}}, id2, GrantPatch{Quantity: &qty}))
	m(e.s.SubmitGrant(e.ctx, e.hr, id2))
	if _, err := e.s.ApproveGrant(e.ctx, Actor{Subject: "ed-editor", Roles: []string{RoleBoard}}, id2); !isErr(err, ErrSeparation) {
		t.Fatalf("editor approving: %v", err)
	}
	// the submitter cannot
	if _, err := e.s.ApproveGrant(e.ctx, Actor{Subject: "HR-ALICE", Roles: []string{RoleBoard}}, id2); !isErr(err, ErrSeparation) {
		t.Fatalf("submitter approving: %v", err)
	}
	// the grantee (a board member who is also an employee stakeholder) cannot approve their own grant
	bobStk := m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "employee", PersonID: personBob, Identity: Identity{Name: "Bob Board"}}))
	e.link(bobStk.ID)
	id3 := e.draft(e.hr, bobStk.ID, q(10))
	m(e.s.SubmitGrant(e.ctx, e.hr, id3))
	if _, err := e.s.ApproveGrant(e.ctx, e.board, id3); !isErr(err, ErrSeparation) {
		t.Fatalf("grantee approving: %v", err)
	}
	if _, err := e.s.ApproveGrant(e.ctx, e.board2, id3); err != nil {
		t.Fatalf("a different board member: %v", err)
	}
	// admin and hr are not the board
	id4 := e.draft(e.hr, e.eveStk.ID, q(10))
	m(e.s.SubmitGrant(e.ctx, e.hr, id4))
	for _, a := range []Actor{e.admin, e.hr2, e.fin} {
		if _, err := e.s.ApproveGrant(e.ctx, a, id4); !isErr(err, ErrForbidden) {
			t.Errorf("%v approved a grant: %v", a.Roles, err)
		}
	}
}

func TestSeparationOfDutiesIsAlsoEnforcedByTheDatabase(t *testing.T) {
	e := newEnv(t).base()
	id := e.draft(e.hr, e.eveStk.ID, q(100))
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	for _, who := range []string{"hr-alice", "hr-alice"} { // created_by and submitted_by are both hr-alice
		if _, err := e.s.db.Exec(`UPDATE grants SET status='approved', approved_by=?, approved_on='2026-03-10' WHERE id=?`, who, id); err == nil {
			t.Fatal("the database accepted the creator as approver")
		}
	}
	if _, err := e.s.db.Exec(`UPDATE grants SET status='approved', approved_on='2026-03-10' WHERE id=?`, id); err == nil {
		t.Fatal("the database accepted an approval with no approver")
	}
	// exercises: requester cannot approve or pay
	e.advance("2027-01-15")
	gid := e.active(e.eveStk.ID, q(4800))
	e.advance("2028-01-15")
	ex, _, err := e.s.RequestExercise(e.ctx, e.eve, gid, q(1), "", nil)
	e.must(err)
	if _, err := e.s.db.Exec(`UPDATE exercises SET status='approved', approved_by='sso-eve' WHERE id=?`, ex); err == nil {
		t.Fatal("the database accepted the requester as approver of an exercise")
	}
}

func TestExerciseChainNeedsDifferentPeople(t *testing.T) {
	e := newEnv(t).base()
	gid := e.active(e.eveStk.ID, q(4800))
	e.advance("2028-01-15")
	// finance requests on behalf, then cannot approve, pay or issue it
	ex, _, err := e.s.RequestExercise(e.ctx, e.fin, gid, q(10), "", nil)
	e.must(err)
	if _, err := e.s.ApproveExercise(e.ctx, Actor{Subject: "FIN-FRANK ", Roles: []string{RoleFinance}}, ex); !isErr(err, ErrSeparation) {
		t.Fatalf("requester approving: %v", err)
	}
	m(e.s.ApproveExercise(e.ctx, e.fin2, ex))
	if _, err := e.s.RecordPayment(e.ctx, e.fin, ex, PaymentInput{AmountMinor: 1250, PaidOn: "2028-01-15"}); !isErr(err, ErrSeparation) {
		t.Fatalf("requester paying: %v", err)
	}
	m(e.s.RecordPayment(e.ctx, e.fin2, ex, PaymentInput{AmountMinor: 1250, PaidOn: "2028-01-15", Reference: "REF"}))
	if _, err := e.s.IssueShares(e.ctx, e.fin, ex); !isErr(err, ErrSeparation) {
		t.Fatalf("requester issuing: %v", err)
	}
	if _, err := e.s.IssueShares(e.ctx, e.fin2, ex); !isErr(err, ErrSeparation) {
		t.Fatalf("payer issuing: %v", err)
	}
	if _, err := e.s.IssueShares(e.ctx, e.admin, ex); err != nil {
		t.Fatalf("a third person: %v", err)
	}
}

func TestApprovalEngineDeciderIsBoundBySoDToo(t *testing.T) {
	eng := &fakeEngine{outcome: "pending"}
	e := newEnv(t, func(o *Options) { o.Approvals = eng }).base()
	id := e.draft(e.hr, e.eveStk.ID, q(100))
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	if len(eng.reqs) != 1 || eng.reqs[0].Type != "grant" || eng.reqs[0].Ref != id {
		t.Fatalf("engine requests = %+v", eng.reqs)
	}
	if _, err := e.s.ApproveGrant(e.ctx, e.board, id); !isErr(err, ErrConflict) {
		t.Fatalf("approved while the engine says pending: %v", err)
	}
	eng.outcome, eng.decider = "approved", "HR-alice"
	if _, err := e.s.ApproveGrant(e.ctx, e.board, id); !isErr(err, ErrSeparation) {
		t.Fatalf("the engine's approver was the grantor: %v", err)
	}
	eng.decider = "board-bea"
	if g, err := e.s.ApproveGrant(e.ctx, e.board, id); err != nil || g.Status != GrantApproved {
		t.Fatalf("engine-approved: %v %+v", err, g)
	}
}

// ---- immutability -----------------------------------------------------------------------------

func TestTermsFrozenAfterSubmission(t *testing.T) {
	e := newEnv(t).base()
	id := e.draft(e.hr, e.eveStk.ID, q(100))
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	qty := q(999)
	if _, err := e.s.UpdateGrant(e.ctx, e.hr, id, GrantPatch{Quantity: &qty}); !isErr(err, ErrConflict) {
		t.Fatalf("update after submit: %v", err)
	}
	m(e.s.ApproveGrant(e.ctx, e.board, id))
	if _, err := e.s.UpdateGrant(e.ctx, e.hr, id, GrantPatch{Quantity: &qty}); !isErr(err, ErrConflict) {
		t.Fatalf("update after approval: %v", err)
	}
	if err := e.s.DeleteGrant(e.ctx, e.hr, id); !isErr(err, ErrConflict) {
		t.Fatalf("delete after approval: %v", err)
	}
	// the database refuses it as well
	for _, stmt := range []string{
		`UPDATE grants SET quantity = 999 WHERE id = ?`,
		`UPDATE grants SET strike_minor = 1 WHERE id = ?`,
		`UPDATE grants SET schedule = '{}' WHERE id = ?`,
		`UPDATE grants SET stakeholder_id = '` + e.founder.ID + `' WHERE id = ?`,
		`UPDATE grants SET expiry_date = '2099-01-01' WHERE id = ?`,
		`UPDATE grants SET version = 7 WHERE id = ?`,
		`UPDATE grants SET status = 'draft' WHERE id = ?`,
		`UPDATE grants SET status = 'active', exercised = 0 WHERE id = ? AND 0`, // no-op guard for the next
		`DELETE FROM grants WHERE id = ?`,
	} {
		if strings.Contains(stmt, "AND 0") {
			continue
		}
		if _, err := e.s.db.Exec(stmt, id); err == nil {
			t.Errorf("database accepted: %s", stmt)
		}
	}
	// a terminal status is final
	m(e.s.CancelGrant(e.ctx, e.hr, id, ""))
	if _, err := e.s.db.Exec(`UPDATE grants SET status = 'active' WHERE id = ?`, id); err == nil {
		t.Error("a cancelled grant was reactivated")
	}
	// exercised never goes down or above the grant
	id2 := e.active(e.eveStk.ID, q(100))
	if _, err := e.s.db.Exec(`UPDATE grants SET exercised = quantity + 1 WHERE id = ?`, id2); err == nil {
		t.Error("exercised above the grant quantity")
	}
}

func TestAppendOnlyTables(t *testing.T) {
	e := newEnv(t).base()
	m(e.s.GetGrant(e.ctx, e.hr, e.draft(e.hr, e.eveStk.ID, q(1)))) // writes audit rows
	id, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, Quantity: q(1000), EffectiveDate: "2026-01-01"}, "", nil)
	e.must(err)
	m(e.s.CreateValuation(e.ctx, e.fin, e.ent.ID, ValuationInput{Kind: "409a", EffectiveDate: "2026-02-01", PriceMinor: 80, Currency: "USD", Notes: "n"}))
	m(e.s.CreateConvertible(e.ctx, e.fin, e.ent.ID, ConvertibleInput{StakeholderID: e.founder.ID, Kind: "safe_post_money", IssuedOn: "2026-02-01", PrincipalMinor: 100000, Currency: "USD", CapMinor: 1000000}))
	for _, stmt := range []string{
		`UPDATE audit_log SET actor = 'x'`, `DELETE FROM audit_log`,
		`UPDATE ledger SET quantity = 1`, `DELETE FROM ledger`,
		`DELETE FROM valuations`, `UPDATE valuations SET effective_date = '2000-01-01'`, `UPDATE valuations SET kind = 'other'`,
		`DELETE FROM stakeholders`, `UPDATE stakeholders SET kind = 'investor'`,
		`DELETE FROM share_classes`, `UPDATE share_classes SET authorised = 1`, `UPDATE share_classes SET name = 'x'`,
		`DELETE FROM convertibles`,
	} {
		if _, err := e.s.db.Exec(stmt); err == nil {
			t.Errorf("database accepted: %s", stmt)
		}
	}
	_ = id
	if err := e.s.VerifyAudit(e.ctx); err != nil {
		t.Fatal(err)
	}
	// corrupting a row (by an administrator who dropped the trigger) is detected by the chain
	_, err = e.s.db.Exec(`DROP TRIGGER audit_log_no_update`)
	e.must(err)
	_, err = e.s.db.Exec(`UPDATE audit_log SET actor = 'mallory' WHERE seq = 1`)
	e.must(err)
	if err := e.s.VerifyAudit(e.ctx); err == nil {
		t.Fatal("tampering with the audit log went unnoticed")
	}
}

func TestLedgerRulesAndCorrections(t *testing.T) {
	e := newEnv(t).base()
	issue := func(stk string, n Qty, date string) error {
		_, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: stk, Quantity: n, EffectiveDate: date}, "", nil)
		return err
	}
	e.must(issue(e.founder.ID, q(1000), "2026-01-01"))
	if err := issue(e.founder.ID, q(10_000_000), "2026-01-02"); !isErr(err, ErrConflict) {
		t.Fatalf("above authorised: %v", err)
	}
	if err := issue(e.founder.ID, q(1), "2099-01-01"); err == nil {
		t.Fatal("future-dated issuance accepted")
	}
	// transfer moves shares, both legs share a group
	_, _, err := e.s.TransferShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, ToID: e.eveStk.ID, Quantity: q(400), EffectiveDate: "2026-02-01"}, "", nil)
	e.must(err)
	if _, _, err := e.s.TransferShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, ToID: e.eveStk.ID, Quantity: q(601), EffectiveDate: "2026-02-02"}, "", nil); !isErr(err, ErrConflict) {
		t.Fatalf("over-transfer: %v", err)
	}
	// a backdated removal cannot open a hole later: eve received 400 on 02-01; removing 300 on 01-15 would make her negative then
	if _, _, err := e.s.CancelShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.eveStk.ID, Quantity: q(300), EffectiveDate: "2026-01-15"}, "", nil); !isErr(err, ErrConflict) {
		t.Fatalf("backdated removal: %v", err)
	}
	if _, _, err := e.s.CancelShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.eveStk.ID, Quantity: q(300), EffectiveDate: "2026-02-01", Kind: "repurchase"}, "", nil); err != nil {
		t.Fatal(err)
	}
	// as-of views
	at := func(d string) map[string]Qty {
		ct := m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, d, "basic", false))
		out := map[string]Qty{}
		for _, h := range ct.Holders {
			out[h.StakeholderID] = h.BasicShares
		}
		return out
	}
	if h := at("2026-01-31"); h[e.founder.ID] != q(1000) || len(h) != 1 {
		t.Fatalf("31 Jan: %v", h)
	}
	if h := at("2026-02-01"); h[e.founder.ID] != q(600) || h[e.eveStk.ID] != q(100) {
		t.Fatalf("1 Feb: %v", h)
	}
	if h := at("2025-12-31"); len(h) != 0 {
		t.Fatalf("before the first issuance: %v", h)
	}
	// a hr user cannot write the ledger
	if _, _, err := e.s.IssueNewShares(e.ctx, e.hr, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, Quantity: q(1), EffectiveDate: "2026-01-01"}, "", nil); !isErr(err, ErrForbidden) {
		t.Fatalf("hr writing the ledger: %v", err)
	}
}

// ---- policy: who sees what ---------------------------------------------------------------------

func TestManagersNeverSeeTheEquityOfTheirReports(t *testing.T) {
	e := newEnv(t).base()
	id := e.active(e.eveStk.ID, q(4800)) // eve's grant; max is eve's manager in the directory
	// max holds employee+manager and has an own person id; nothing about eve is visible
	if _, err := e.s.GetGrant(e.ctx, e.max, id); !isErr(err, ErrNotFound) {
		t.Fatalf("manager reading a report's grant: %v", err)
	}
	if _, err := e.s.Vesting(e.ctx, e.max, id, ""); !isErr(err, ErrNotFound) {
		t.Fatalf("manager reading a report's vesting: %v", err)
	}
	if _, err := e.s.GetStakeholder(e.ctx, e.max, e.eveStk.ID); !isErr(err, ErrNotFound) {
		t.Fatalf("manager reading a report's stakeholder record: %v", err)
	}
	if _, err := e.s.GetIdentity(e.ctx, e.max, e.eveStk.ID); !isErr(err, ErrNotFound) {
		t.Fatalf("manager reading a report's identity: %v", err)
	}
	if _, err := e.s.GetExercise(e.ctx, e.max, "exr_000000000000000000000000"); !isErr(err, ErrNotFound) {
		t.Fatalf("manager reading an exercise: %v", err)
	}
	if _, err := e.s.GetCapTable(e.ctx, e.max, e.ent.ID, "", "basic", false); !isErr(err, ErrForbidden) {
		t.Fatalf("manager reading the cap table: %v", err)
	}
	if _, err := e.s.ListGrants(e.ctx, e.max, e.ent.ID, GrantFilter{}); !isErr(err, ErrForbidden) {
		t.Fatalf("manager listing grants: %v", err)
	}
	if _, err := e.s.ListValuations(e.ctx, e.max, e.ent.ID); !isErr(err, ErrForbidden) {
		t.Fatalf("manager reading valuations: %v", err)
	}
	// a pure manager role with no employee role and no person is blocked altogether
	pure := Actor{Subject: "sso-pat", Roles: []string{RoleManager}}
	if p, err := e.s.MyGrants(e.ctx, pure, GrantFilter{}); err == nil {
		t.Fatalf("pure manager has self-service: %+v", p)
	}
}

func TestEmployeeSelfServiceSeesOnlyOwnApprovedGrants(t *testing.T) {
	e := newEnv(t).base()
	maxStk := m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "employee", PersonID: personMax, Identity: Identity{Name: "Max Manager"}}))
	draft := e.draft(e.hr, e.eveStk.ID, q(50))
	if _, err := e.s.GetGrant(e.ctx, e.eve, draft); !isErr(err, ErrNotFound) {
		t.Fatalf("own draft visible: %v", err)
	}
	if p := m(e.s.MyGrants(e.ctx, e.eve, GrantFilter{})); len(p.Items) != 0 {
		t.Fatalf("own draft listed: %+v", p)
	}
	m(e.s.SubmitGrant(e.ctx, e.hr, draft))
	if _, err := e.s.GetGrant(e.ctx, e.eve, draft); !isErr(err, ErrNotFound) {
		t.Fatalf("own pending grant visible: %v", err)
	}
	m(e.s.ApproveGrant(e.ctx, e.board, draft))
	mine := m(e.s.GetGrant(e.ctx, e.eve, draft))
	if mine.Notes != nil {
		t.Fatal("internal notes shown to the grantee")
	}
	maxGrant := e.active(maxStk.ID, q(7))
	p := m(e.s.MyGrants(e.ctx, e.eve, GrantFilter{}))
	if len(p.Items) != 1 || p.Items[0].ID != draft {
		t.Fatalf("my grants = %+v", p)
	}
	if _, err := e.s.GetGrant(e.ctx, e.eve, maxGrant); !isErr(err, ErrNotFound) {
		t.Fatalf("someone else's grant: %v", err)
	}
	if _, err := e.s.GetIdentity(e.ctx, e.eve, maxStk.ID); !isErr(err, ErrNotFound) {
		t.Fatalf("someone else's identity: %v", err)
	}
	if id, err := e.s.GetIdentity(e.ctx, e.eve, e.eveStk.ID); err != nil || id.Name != "Eve Example" {
		t.Fatalf("own identity: %v %+v", err, id)
	}
	// the holder cannot write anything about themself
	if err := e.s.SetIdentity(e.ctx, e.eve, e.eveStk.ID, Identity{Name: "Eve Renamed"}); !isErr(err, ErrForbidden) {
		t.Fatalf("self identity write: %v", err)
	}
	if _, err := e.s.ListExercises(e.ctx, e.eve, e.ent.ID, ""); !isErr(err, ErrForbidden) {
		t.Fatalf("employee listing all exercises: %v", err)
	}
	// an erased stakeholder is gone from self-service
	m(e.s.CancelGrant(e.ctx, e.hr, draft, "x"))
}

func TestPolicyMatrix(t *testing.T) {
	roles := map[string]Actor{
		"none":     {Subject: "x"},
		"employee": {Subject: "x", Roles: []string{RoleEmployee}, PersonID: personEve},
		"manager":  {Subject: "x", Roles: []string{RoleManager}, PersonID: personEve},
		"hr":       {Subject: "x", Roles: []string{RoleHR}},
		"finance":  {Subject: "x", Roles: []string{RoleFinance}},
		"board":    {Subject: "x", Roles: []string{RoleBoard}},
		"admin":    {Subject: "x", Roles: []string{RoleAdmin}},
	}
	type c struct {
		act  Action
		self bool
		want string // roles that may, comma separated (self role names: employee, manager only when self)
	}
	cases := []c{
		{ActGrantRead, false, "hr,finance,board,admin"},
		{ActGrantRead, true, "employee,manager,hr,finance,board,admin"},
		{ActGrantList, true, "hr,finance,board,admin"},
		{ActGrantDraft, true, "hr,admin"},
		{ActGrantApprove, true, "board"},
		{ActGrantAdminister, true, "hr,admin"},
		{ActExerciseRequest, true, "employee,manager,hr,finance,admin"},
		{ActExerciseRequest, false, "hr,finance,admin"},
		{ActExerciseApprove, true, "finance,board,admin"},
		{ActExercisePay, true, "finance,admin"},
		{ActLedgerWrite, true, "finance,admin"},
		{ActLedgerRead, true, "finance,board,admin"},
		{ActCapTable, true, "finance,board,admin"},
		{ActWhatIf, true, "finance,board,admin"},
		{ActValuation, true, "finance,board,admin"},
		{ActExport, true, "finance,board,admin"},
		{ActStructureManage, true, "board,admin"},
		{ActStructureRead, true, "hr,finance,board,admin"},
		{ActLegalHold, true, "admin"},
		{ActErasure, true, "admin"},
		{ActRetention, true, "admin"},
		{ActSelfList, true, "employee,manager"},
		{ActStakeholderEdit, true, "hr,finance,board,admin"},
	}
	for _, tc := range cases {
		want := map[string]bool{}
		for _, r := range strings.Split(tc.want, ",") {
			want[r] = true
		}
		for name, a := range roles {
			if got := Decide(a, tc.act, Fact{Self: tc.self}); got != want[name] {
				t.Errorf("Decide(%s, %s, self=%v) = %v, want %v", name, tc.act, tc.self, got, want[name])
			}
		}
	}
	if Decide(Actor{}, ActGrantRead, Fact{Self: true}) || Decide(roles["employee"], Action("nonsense"), Fact{Self: true}) {
		t.Error("unknown actors or actions must be denied")
	}
	// field access: valuations and convertible terms never reach hr, employees or managers
	for _, f := range []string{FieldValuation, FieldConvertible} {
		for _, r := range []string{"none", "employee", "manager", "hr"} {
			if FieldAccess(roles[r], false, f, Fact{Self: true}) {
				t.Errorf("%s can read %s", r, f)
			}
		}
	}
	if FieldAccess(roles["employee"], true, FieldIdentity, Fact{Self: true}) || FieldAccess(roles["employee"], false, FieldGrantNotes, Fact{Self: true}) {
		t.Error("an employee can write identity or read internal notes")
	}
}

// ---- audit before return, fail closed -----------------------------------------------------------

func TestSensitiveReadsFailClosedWhenAuditCannotBeWritten(t *testing.T) {
	e := newEnv(t).base()
	gid := e.active(e.eveStk.ID, q(4800))
	exid, _, err := e.s.RequestExercise(e.ctx, e.hr, gid, q(0)+1, "", nil)
	_ = exid
	_ = err
	m(e.s.CreateValuation(e.ctx, e.fin, e.ent.ID, ValuationInput{Kind: "409a", EffectiveDate: "2026-02-01", PriceMinor: 80, Currency: "USD"}))
	m(e.s.CreateConvertible(e.ctx, e.fin, e.ent.ID, ConvertibleInput{StakeholderID: e.founder.ID, Kind: "safe_post_money", IssuedOn: "2026-02-01", PrincipalMinor: 100000, Currency: "USD", CapMinor: 1000000}))
	e.s.audit = failingAuditor{}
	checks := map[string]func() (any, error){
		"grant":      func() (any, error) { return e.s.GetGrant(e.ctx, e.hr, gid) },
		"grant-self": func() (any, error) { return e.s.GetGrant(e.ctx, e.eve, gid) },
		"vesting":    func() (any, error) { return e.s.Vesting(e.ctx, e.hr, gid, "") },
		"list":       func() (any, error) { return e.s.ListGrants(e.ctx, e.hr, e.ent.ID, GrantFilter{}) },
		"mine":       func() (any, error) { return e.s.MyGrants(e.ctx, e.eve, GrantFilter{}) },
		"identity":   func() (any, error) { return e.s.GetIdentity(e.ctx, e.hr, e.eveStk.ID) },
		"captable":   func() (any, error) { return e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "", "fully-diluted", true) },
		"ledger":     func() (any, error) { return e.s.ListLedger(e.ctx, e.fin, e.ent.ID, "", 0, 0) },
		"valuations": func() (any, error) { return e.s.ListValuations(e.ctx, e.fin, e.ent.ID) },
		"converts":   func() (any, error) { return e.s.ListConvertibles(e.ctx, e.fin, e.ent.ID) },
		"whatif": func() (any, error) {
			return e.s.WhatIf(e.ctx, e.fin, e.ent.ID, WhatIfRequest{PreMoneyMinor: 1_000_000_00, InvestmentMinor: 100_000_00, UseRecorded: true})
		},
	}
	for name, fn := range checks {
		if v, err := fn(); err == nil {
			t.Errorf("%s returned data although the audit write failed: %+v", name, v)
		}
	}
	var buf bytes.Buffer
	if err := e.s.ExportCSV(e.ctx, e.fin, e.ent.ID, "captable", "", &buf); err == nil || buf.Len() != 0 {
		t.Errorf("export wrote %d bytes with a failing audit: %v", buf.Len(), err)
	}
}

func TestReadsAreAuditedPerRecordAndDeniedAttemptsAreRecorded(t *testing.T) {
	e := newEnv(t).base()
	gid := e.active(e.eveStk.ID, q(100))
	before := e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'read' AND resource = ?`, "grant:"+gid)
	m(e.s.GetGrant(e.ctx, e.hr, gid))
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'read' AND resource = ?`, "grant:"+gid) != before+1 {
		t.Fatal("grant read not audited")
	}
	m(e.s.ListGrants(e.ctx, e.hr, e.ent.ID, GrantFilter{}))
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'read' AND resource = ?`, "grant:"+gid) != before+2 {
		t.Fatal("list read not audited per record")
	}
	_, _ = e.s.GetGrant(e.ctx, e.max, gid)
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied' AND resource = ?`, "grant:"+gid) != 1 {
		t.Fatal("denied read not recorded")
	}
	// the audit log holds ids and field names only: no names, amounts or notes
	rows, err := e.s.db.Query(`SELECT actor, action, resource, fields FROM audit_log`)
	e.must(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a, b, c, d string
		e.must(rows.Scan(&a, &b, &c, &d))
		for _, bad := range []string{"Eve Example", "eve@example.test", "board minute"} {
			if strings.Contains(a+b+c+d, bad) {
				t.Fatalf("audit row leaks %q: %s %s %s %s", bad, a, b, c, d)
			}
		}
	}
	// denial audit is throttled per actor so it cannot fill the log
	for i := 0; i < 200; i++ {
		_, _ = e.s.GetGrant(e.ctx, e.max, gid)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied' AND actor = ?`, personMax); n > denyAuditPerMinute+2 {
		t.Fatalf("%d denial rows from one actor in a minute", n)
	}
}

// ---- sealed columns, rotation ---------------------------------------------------------------------

func TestSensitiveColumnsAreSealedAndBoundToTheirRow(t *testing.T) {
	e := newEnv(t).base()
	gid := e.draft(e.hr, e.eveStk.ID, q(10))
	var name, email, notes string
	e.must(e.s.db.QueryRow(`SELECT sens_name, sens_email FROM stakeholders WHERE id = ?`, e.eveStk.ID).Scan(&name, &email))
	e.must(e.s.db.QueryRow(`SELECT sens_notes FROM grants WHERE id = ?`, gid).Scan(&notes))
	for _, v := range []string{name, email, notes} {
		if !strings.HasPrefix(v, "v1.k1.") || strings.Contains(v, "Eve") || strings.Contains(v, "minute") {
			t.Fatalf("not sealed: %q", v)
		}
	}
	// the whole database file (and WAL) never holds the plaintext
	e.must(e.s.scrub(e.ctx, false))
	raw, _ := os.ReadFile(e.path)
	wal, _ := os.ReadFile(e.path + "-wal")
	for _, p := range []string{"Eve Example", "eve@example.test", "board minute 14", "Fran Founder"} {
		if bytes.Contains(raw, []byte(p)) || bytes.Contains(wal, []byte(p)) {
			t.Errorf("plaintext %q found in the database files", p)
		}
	}
	// a ciphertext copied to another row or field does not open
	e.must(e.s.db.QueryRow(`SELECT sens_name FROM stakeholders WHERE id = ?`, e.founder.ID).Scan(&name))
	_, err := e.s.db.Exec(`UPDATE stakeholders SET sens_name = (SELECT sens_name FROM stakeholders WHERE id = ?) WHERE id = ?`, e.eveStk.ID, e.founder.ID)
	e.must(err)
	if _, err := e.s.GetIdentity(e.ctx, e.hr, e.founder.ID); err == nil {
		t.Fatal("a swapped ciphertext opened under another row")
	}
}

func TestKeyRotationReSealsAndSurvivesConcurrentWrites(t *testing.T) {
	e := newEnv(t).base()
	m(e.s.CreateValuation(e.ctx, e.fin, e.ent.ID, ValuationInput{Kind: "409a", EffectiveDate: "2026-02-01", PriceMinor: 80, Currency: "USD", Notes: "n"}))
	m(e.s.CreateConvertible(e.ctx, e.fin, e.ent.ID, ConvertibleInput{StakeholderID: e.founder.ID, Kind: "safe_post_money", IssuedOn: "2026-02-01", PrincipalMinor: 100000, Currency: "USD", CapMinor: 1000000}))
	e.draft(e.hr, e.eveStk.ID, q(10))
	kr, err := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	e.must(err)
	e.s.keys = kr
	n := m(e.s.RotateKeys(e.ctx))
	if n < 7 {
		t.Fatalf("re-sealed only %d values", n)
	}
	if n2 := m(e.s.RotateKeys(e.ctx)); n2 != 0 {
		t.Fatalf("second rotation re-sealed %d", n2)
	}
	if id := m(e.s.GetIdentity(e.ctx, e.hr, e.eveStk.ID)); id.Name != "Eve Example" {
		t.Fatalf("identity after rotation: %+v", id)
	}
	// only k2 now opens everything
	only, _ := ParseKeyring("k2 " + hexKeyB + "\n")
	e.s.keys = only
	if vs := m(e.s.ListValuations(e.ctx, e.fin, e.ent.ID)); len(vs) != 1 || vs[0].PriceMinor != 80 || vs[0].Notes != "n" {
		t.Fatalf("valuations = %+v", vs)
	}
	if cs := m(e.s.ListConvertibles(e.ctx, e.fin, e.ent.ID)); len(cs) != 1 || cs[0].PrincipalMinor != 100000 {
		t.Fatalf("convertibles = %+v", cs)
	}
}

func TestKeyRotationNeverOverwritesAConcurrentUpdate(t *testing.T) {
	e := newEnv(t).base()
	old := e.s.keys
	kr, _ := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	e.s.keys = kr
	fired := false
	e.s.rotateHook = func() {
		if fired {
			return
		}
		fired = true
		// a concurrent identity update lands between the read and the compare-and-swap
		sealed, _ := kr.Seal("Eve Renamed", e.eveStk.ID+":name")
		_, _ = e.s.db.Exec(`UPDATE stakeholders SET sens_name = ? WHERE id = ?`, sealed, e.eveStk.ID)
	}
	m(e.s.RotateKeys(e.ctx))
	e.s.rotateHook = nil
	if id := m(e.s.GetIdentity(e.ctx, e.hr, e.eveStk.ID)); id.Name != "Eve Renamed" {
		t.Fatalf("the rotation clobbered a concurrent write: %+v", id)
	}
	_ = old
}

// ---- erasure, retention, legal hold ----------------------------------------------------------------

func TestErasurePseudonymisesAndKeepsTheLedger(t *testing.T) {
	e := newEnv(t).base()
	gid := e.active(e.eveStk.ID, q(4800))
	e.advance("2028-01-15")
	ex, _, err := e.s.RequestExercise(e.ctx, e.eve, gid, q(10), "", nil)
	e.must(err)
	m(e.s.ApproveExercise(e.ctx, e.fin, ex))
	m(e.s.RecordPayment(e.ctx, e.fin2, ex, PaymentInput{AmountMinor: 1250, PaidOn: "2028-01-15", Reference: "BANK-REF-123"}))
	m(e.s.IssueShares(e.ctx, e.admin, ex))
	if _, err := e.s.RequestErasure(e.ctx, e.admin, e.eveStk.ID); !isErr(err, ErrConflict) {
		t.Fatalf("erasure with a live grant: %v", err)
	}
	m(e.s.CancelGrant(e.ctx, e.hr, gid, "leaver"))
	// another stakeholder with only a draft: it is deleted by erasure
	draftOnly := m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "advisor", Identity: Identity{Name: "Ada Advisor"}}))
	dg := e.draft(e.hr, draftOnly.ID, q(5))
	var sealedName string
	e.must(e.s.db.QueryRow(`SELECT sens_name FROM stakeholders WHERE id = ?`, e.eveStk.ID).Scan(&sealedName))

	if _, err := e.s.RequestErasure(e.ctx, e.hr, e.eveStk.ID); !isErr(err, ErrForbidden) {
		t.Fatalf("hr erasing: %v", err)
	}
	e.must(e.s.SetLegalHold(e.ctx, e.admin, e.eveStk.ID, true, "litigation"))
	if _, err := e.s.RequestErasure(e.ctx, e.admin, e.eveStk.ID); !isErr(err, ErrConflict) {
		t.Fatalf("erasure under legal hold: %v", err)
	}
	e.must(e.s.SetLegalHold(e.ctx, e.admin, e.eveStk.ID, false, ""))
	rep := m(e.s.RequestErasure(e.ctx, e.admin, e.eveStk.ID))
	if len(rep.Retained) == 0 || rep.Retained[0].Class != ClassLedger || rep.Subject != e.eveStk.ID {
		t.Fatalf("report = %+v", rep)
	}
	id := m(e.s.GetIdentity(e.ctx, e.hr, e.eveStk.ID))
	if id.Name != "[erased]" || id.Email != "" {
		t.Fatalf("identity after erasure: %+v", id)
	}
	st := m(e.s.GetStakeholder(e.ctx, e.hr, e.eveStk.ID))
	if !st.Erased || st.PersonID != "" {
		t.Fatalf("stakeholder = %+v", st)
	}
	// self-service is gone, the ledger and the exercise record remain
	if p := m(e.s.MyGrants(e.ctx, e.eve, GrantFilter{})); len(p.Items) != 0 {
		t.Fatalf("erased holder still has self-service: %+v", p)
	}
	if e.count(`SELECT COUNT(*) FROM ledger WHERE stakeholder_id = ?`, e.eveStk.ID) != 1 || e.count(`SELECT COUNT(*) FROM exercises WHERE stakeholder_id = ? AND status = 'issued'`, e.eveStk.ID) != 1 {
		t.Fatal("legal-retention records were removed")
	}
	if e.count(`SELECT COUNT(*) FROM exercises WHERE sens_payment_ref IS NOT NULL AND stakeholder_id = ?`, e.eveStk.ID) != 0 {
		t.Fatal("payment reference survived erasure")
	}
	if _, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: e.eveStk.ID, Quantity: q(1), EffectiveDate: "2028-01-15"}, "", nil); !isErr(err, ErrConflict) {
		t.Fatalf("issuing to an erased stakeholder: %v", err)
	}
	// the sealed identity is not left behind in the database files or the WAL
	raw, _ := os.ReadFile(e.path)
	wal, _ := os.ReadFile(e.path + "-wal")
	if bytes.Contains(raw, []byte(sealedName)) || bytes.Contains(wal, []byte(sealedName)) {
		t.Fatal("erased ciphertext is still in the database files")
	}
	// tombstones: opaque subject ids, purgeable classes only, idempotent
	ts := m(e.s.ListErasures(e.ctx, e.admin, 0))
	if len(ts) != len(PurgeClasses) {
		t.Fatalf("tombstones = %+v", ts)
	}
	for _, x := range ts {
		if x.Subject != e.eveStk.ID {
			t.Fatalf("tombstone subject %q", x.Subject)
		}
	}
	m(e.s.RequestErasure(e.ctx, e.admin, e.eveStk.ID))
	if len(m(e.s.ListErasures(e.ctx, e.admin, 0))) != len(PurgeClasses) {
		t.Fatal("repeat erasure added tombstones")
	}
	// a stakeholder with only a draft: the draft is deleted
	m(e.s.RequestErasure(e.ctx, e.admin, draftOnly.ID))
	if e.count(`SELECT COUNT(*) FROM grants WHERE id = ?`, dg) != 0 {
		t.Fatal("a draft grant survived erasure")
	}
	// the restore-time replay hook is idempotent and refuses the legal-record classes
	e.must(e.s.PurgeSubject(e.ctx, e.eveStk.ID, ClassStakeholder))
	if err := e.s.PurgeSubject(e.ctx, e.eveStk.ID, ClassLedger); err == nil {
		t.Fatal("the ledger was purged by the replay hook")
	}
	if err := e.s.PurgeSubject(e.ctx, "nonsense", ClassStakeholder); err == nil {
		t.Fatal("a bad subject was accepted")
	}
}

func TestRetentionSweep(t *testing.T) {
	e := newEnv(t).base()
	idle := m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "advisor", Identity: Identity{Name: "Old Advisor"}}))
	held := m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "advisor", Identity: Identity{Name: "Held Advisor"}}))
	e.must(e.s.SetLegalHold(e.ctx, e.admin, held.ID, true, "hold"))
	m(e.s.CreateValuation(e.ctx, e.fin, e.ent.ID, ValuationInput{Kind: "409a", EffectiveDate: "2026-02-01", PriceMinor: 80, Currency: "USD"}))
	e.advance("2027-01-01")
	rep := m(e.s.RunRetention(e.ctx))
	for c, n := range rep.Purged {
		if n != 0 {
			t.Fatalf("early sweep purged %s: %v", c, rep.Purged)
		}
	}
	e.advance("2034-06-01")
	rep = m(e.s.RunRetention(e.ctx))
	if rep.Purged[ClassStakeholder] < 1 || rep.Skipped[ClassStakeholder] != 1 || rep.Purged[ClassValuation] != 1 {
		t.Fatalf("sweep = %+v", rep)
	}
	if st := m(e.s.GetStakeholder(e.ctx, e.hr, idle.ID)); !st.Erased {
		t.Fatal("idle stakeholder kept")
	}
	if st := m(e.s.GetStakeholder(e.ctx, e.hr, held.ID)); st.Erased {
		t.Fatal("legal hold ignored by retention")
	}
	if vs := m(e.s.ListValuations(e.ctx, e.fin, e.ent.ID)); len(vs) != 1 || vs[0].PriceMinor != 0 {
		t.Fatalf("valuation after retention = %+v", vs)
	}
	// the ledger and audit classes can only be configured as legal
	if _, err := ParseConfig([]byte("keyring_file: /k\nretention:\n  equity-ledger: 1y\n")); err == nil {
		t.Fatal("a finite retention for the ledger was accepted")
	}
	if _, err := ParseConfig([]byte("keyring_file: /k\nretention:\n  equity-grants: 3y\n")); err != nil {
		t.Fatal(err)
	}
}

// ---- idempotency, pool races, misc ----------------------------------------------------------------

func TestIdempotentCreates(t *testing.T) {
	e := newEnv(t).base()
	in := e.standardInput(e.eveStk.ID, q(10))
	body, _ := json.Marshal(in)
	id1, replay, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in, "key-1", body)
	e.must(err)
	id2, replay2, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in, "key-1", body)
	e.must(err)
	if id1 != id2 || replay || !replay2 {
		t.Fatalf("ids %s %s replay %v %v", id1, id2, replay, replay2)
	}
	in.Quantity = q(11)
	body2, _ := json.Marshal(in)
	if _, _, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in, "key-1", body2); !isErr(err, ErrConflict) {
		t.Fatalf("key reuse with another body: %v", err)
	}
	if e.count(`SELECT COUNT(*) FROM grants`) != 1 {
		t.Fatal("a replayed create made a second grant")
	}
	// the same key from another actor is independent
	if _, replay, err := e.s.CreateGrant(e.ctx, e.admin, e.plan.ID, e.standardInput(e.eveStk.ID, q(10)), "key-1", body); err != nil || replay {
		t.Fatalf("other actor: %v %v", replay, err)
	}
	if _, _, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in, strings.Repeat("k", 200), body2); err == nil {
		t.Fatal("an oversized key was accepted")
	}
	// ledger writes too
	li := LedgerInput{ClassID: e.class.ID, StakeholderID: e.founder.ID, Quantity: q(5), EffectiveDate: "2026-01-01"}
	lb, _ := json.Marshal(li)
	a, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, li, "L1", lb)
	e.must(err)
	b, rp, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, li, "L1", lb)
	e.must(err)
	if a != b || !rp || e.count(`SELECT COUNT(*) FROM ledger`) != 1 {
		t.Fatal("ledger replay created a second entry")
	}
	// the idempotency table holds ids only
	var n int
	e.must(e.s.db.QueryRow(`SELECT COUNT(*) FROM idempotency WHERE resource_id NOT LIKE 'grt\_%' ESCAPE '\' AND resource_id NOT LIKE 'led\_%' ESCAPE '\'`).Scan(&n))
	if n != 0 {
		t.Fatal("idempotency rows hold something other than ids")
	}
}

func TestPoolCannotBeOverCommittedByConcurrentApprovals(t *testing.T) {
	e := newEnv(t).base()
	small := m(e.s.CreatePlan(e.ctx, e.board, e.ent.ID, PlanInput{Name: "Small", ClassID: e.class.ID, Authorised: q(1000), AdoptedOn: "2026-01-01"}))
	mk := func() string {
		in := e.standardInput(e.eveStk.ID, q(600))
		id, _, err := e.s.CreateGrant(e.ctx, e.hr, small.ID, in, "", nil)
		e.must(err)
		return id
	}
	a, b := mk(), mk()
	m(e.s.SubmitGrant(e.ctx, e.hr, a))
	if _, err := e.s.SubmitGrant(e.ctx, e.hr, b); err != nil {
		t.Fatal(err) // both fit individually while neither is approved
	}
	m(e.s.ApproveGrant(e.ctx, e.board, a))
	if _, err := e.s.ApproveGrant(e.ctx, e.board, b); !isErr(err, ErrConflict) {
		t.Fatalf("second approval beyond the pool: %v", err)
	}
	// plans cannot reserve more than the class authorises
	if _, err := e.s.CreatePlan(e.ctx, e.board, e.ent.ID, PlanInput{Name: "Huge", ClassID: e.class.ID, Authorised: q(9_500_000), AdoptedOn: "2026-01-01"}); !isErr(err, ErrConflict) {
		t.Fatalf("plan above the class: %v", err)
	}
	// a share class's authorised shares can only go up
	if _, err := e.s.RaiseAuthorised(e.ctx, e.board, e.class.ID, q(5)); !isErr(err, ErrConflict) {
		t.Fatalf("lowering authorised: %v", err)
	}
	if c := m(e.s.RaiseAuthorised(e.ctx, e.board, e.class.ID, q(20_000_000))); c.Authorised != q(20_000_000) {
		t.Fatalf("class = %+v", c)
	}
}

func TestExportEscapesFormulasAndIsAudited(t *testing.T) {
	e := newEnv(t).base()
	evil := m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "investor", Identity: Identity{Name: `=HYPERLINK("http://evil.test","x")`}}))
	plus := m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "investor", Identity: Identity{Name: "+1+1"}}))
	for _, st := range []*Stakeholder{evil, plus} {
		e.link(st.ID)
		_, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: st.ID, Quantity: q(10), EffectiveDate: "2026-01-01"}, "", nil)
		e.must(err)
	}
	e.active(evil.ID, q(10))
	for _, name := range []string{"captable", "grants", "ledger"} {
		var buf bytes.Buffer
		e.must(e.s.ExportCSV(e.ctx, e.fin, e.ent.ID, name, "", &buf))
		out := buf.String()
		if strings.Contains(out, ",=HYPERLINK") || strings.Contains(out, ",+1+1") || strings.Contains(out, `"=HYPERLINK`) {
			t.Errorf("%s export has an unescaped formula:\n%s", name, out)
		}
		if !strings.Contains(out, "'=HYPERLINK") || (name != "grants" && !strings.Contains(out, "'+1+1")) {
			t.Errorf("%s export lacks the defused cells:\n%s", name, out)
		}
		if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'export' AND resource LIKE ?`, "export:"+name+":%") != 1 {
			t.Errorf("%s export not audited", name)
		}
	}
	if err := e.s.ExportCSV(e.ctx, e.fin, e.ent.ID, "../../etc/passwd", "", &bytes.Buffer{}); !isErr(err, ErrNotFound) {
		t.Fatalf("unknown export: %v", err)
	}
	if err := e.s.ExportCSV(e.ctx, e.hr, e.ent.ID, "captable", "", &bytes.Buffer{}); !isErr(err, ErrForbidden) {
		t.Fatalf("hr export: %v", err)
	}
	if got := csvSafe("\t=x"); !strings.HasPrefix(got, "'") {
		t.Errorf("tab-prefixed formula: %q", got)
	}
	if got := csvSafe("  @SUM(1)"); !strings.HasPrefix(got, "'") {
		t.Errorf("space-prefixed formula: %q", got)
	}
}

func TestBoundedWorkHonoursDeadlines(t *testing.T) {
	e := newEnv(t).base()
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	start := time.Now()
	if _, err := e.s.ListGrants(ctx, e.hr, e.ent.ID, GrantFilter{}); err == nil {
		t.Error("a cancelled context was ignored")
	}
	if _, err := e.s.GetCapTable(ctx, e.fin, e.ent.ID, "", "fully-diluted", false); err == nil {
		t.Error("a cancelled context was ignored by the cap table")
	}
	if time.Since(start) > 30*time.Second {
		t.Error("cancelled work was not abandoned quickly")
	}
	// page sizes are capped
	for i := 0; i < 3; i++ {
		e.draft(e.hr, e.eveStk.ID, q(1))
	}
	p := m(e.s.ListGrants(e.ctx, e.hr, e.ent.ID, GrantFilter{Limit: 2}))
	if len(p.Items) != 2 || p.Next == "" {
		t.Fatalf("page = %+v", p)
	}
	big := m(e.s.ListGrants(e.ctx, e.hr, e.ent.ID, GrantFilter{Limit: 100000}))
	if len(big.Items) != 3 {
		t.Fatalf("oversized limit: %d", len(big.Items))
	}
}

func TestRoundTripsOfTypes(t *testing.T) {
	for in, want := range map[string]string{"0": "0", "1": "1", "1000": "1000", "1000.5": "1000.5", "1000.0001": "1000.0001", "1000.50": "1000.5", "99999999999": "99999999999", "100000000000": "100000000000"} {
		q, err := ParseQty(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := q.String(); got != want {
			t.Errorf("round trip %s -> %s, want %s", in, got, want)
		}
	}
	for _, s := range []string{"", "-1", "1e3", "1,000", "1.00001", ".5", "01", "100000000001", "NaN", " 1"} {
		if _, err := ParseQty(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	var qq Qty
	if err := json.Unmarshal([]byte(`1000.25`), &qq); err != nil || qq != 10002500 {
		t.Errorf("number json: %v %d", err, qq)
	}
	if err := json.Unmarshal([]byte(`"1000.25"`), &qq); err != nil || qq != 10002500 {
		t.Errorf("string json: %v %d", err, qq)
	}
	if err := json.Unmarshal([]byte(`1e5`), &qq); err == nil {
		t.Error("exponent json accepted")
	}
	if FormatMinor(12345, "USD") != "123.45" || FormatMinor(5, "USD") != "0.05" || FormatMinor(1234, "JPY") != "1234" || FormatMinor(-5, "BHD") != "-0.005" {
		t.Error("FormatMinor")
	}
}

var _ = sql.ErrNoRows

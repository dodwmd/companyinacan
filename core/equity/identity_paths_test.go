package equity

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Specs/051: a company of one or two people must always have a way to record equity, and no way
// to approve its own. These tests try every assignment of the people there are to the steps.

// staff is one human of a small company: finance, hr and admin with a token (and a board seat below).
func (e *idEnv) staff(sub string) Actor { return e.as("user:" + sub) }

func (e *idEnv) boardSeat(sub string) Actor {
	return e.actor(Principal{Subject: "user:" + sub, Roles: []string{RoleBoard}})
}

type grantPath struct{ creator, confirmer, drafter, approver string }

// smallCompany is one service in the given mode; every assignment gets its own entity in it.
func smallCompany(t *testing.T, mode string, breakGlass bool) *idEnv {
	t.Helper()
	e := newIDEnv(t, func(o *Options) {
		if breakGlass {
			o.BreakGlassUntil = time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC)
		}
	})
	if mode == ModeAttestation {
		e.s.directory = nil
		e.refresh()
	}
	return e
}

// tryGrant runs one assignment to an approved grant; "svc" is the service account. It reports
// whether every step was accepted.
func tryGrant(e *idEnv, holderPerson string, p grantPath) bool {
	e.t.Helper()
	who := func(name string) Actor {
		if name == "svc" {
			return e.svc
		}
		return e.staff(name)
	}
	boss := Actor{Subject: "board-setup", Roles: []string{RoleBoard}}
	e.ent = m(e.s.CreateEntity(e.ctx, boss, EntityInput{Name: "Example Pty Ltd", Currency: "USD"}))
	class := m(e.s.CreateShareClass(e.ctx, boss, e.ent.ID, ShareClassInput{Name: "Common", Kind: "common", ParMinor: 1, Currency: "USD", Authorised: q(10_000)}))
	plan := m(e.s.CreatePlan(e.ctx, boss, e.ent.ID, PlanInput{Name: "Plan", ClassID: class.ID, Authorised: q(1000), AdoptedOn: "2026-01-01"}))
	person := holderPerson
	if e.s.directory == nil {
		person = ""
	}
	st, err := e.holder(who(p.creator), person, "Holder")
	if err != nil {
		return false
	}
	if person == "" {
		_, err = e.s.AttestExternal(e.ctx, who(p.confirmer), st.ID)
	} else {
		_, err = e.s.VerifyPerson(e.ctx, who(p.confirmer), st.ID)
	}
	if err != nil {
		return false
	}
	id, _, err := e.s.CreateGrant(e.ctx, who(p.drafter), plan.ID, e.standardInput(st.ID, q(10)), "", nil)
	if err != nil {
		return false
	}
	if _, err := e.s.SubmitGrant(e.ctx, who(p.drafter), id); err != nil {
		return false
	}
	if p.approver == "svc" {
		_, err = e.s.ApproveGrant(e.ctx, e.svc, id)
	} else {
		_, err = e.s.ApproveGrant(e.ctx, e.boardSeat(p.approver), id)
	}
	return err == nil
}

// paths lists every assignment of the given actors that ends in an approved grant, and fails the
// test on any in which the holder or the drafter approved, or the service account did.
func paths(t *testing.T, mode string, breakGlass bool, holder string, actors []string) []grantPath {
	t.Helper()
	person := map[string]string{"ann": personAnn, "ben": personBen, "cat": personCat, "emp": "pers_0000000000000000000000e9"}[holder]
	var out []grantPath
	e := smallCompany(t, mode, breakGlass)
	for _, c := range actors {
		for _, v := range actors {
			for _, d := range actors {
				for _, a := range actors {
					p := grantPath{c, v, d, a}
					if !tryGrant(e, person, p) {
						continue
					}
					out = append(out, p)
					// without a directory nothing says which account the holder is
					if p.approver == "svc" || p.approver == p.drafter || (mode == ModeDirectory && p.approver == holder) {
						t.Errorf("%s break-glass=%v holder=%s: self-approval path %+v", mode, breakGlass, holder, p)
					}
					if p.creator == p.confirmer {
						t.Errorf("%s: one actor established the record: %+v", mode, p)
					}
				}
			}
		}
	}
	return out
}

func TestGrantPathsForTwoFounders(t *testing.T) {
	two, withSvc := []string{"ann", "ben"}, []string{"ann", "ben", "svc"}
	for _, c := range []struct {
		mode       string
		breakGlass bool
		holder     string
		actors     []string
		want       bool
	}{
		// an employee: one founder drafts, the other approves
		{ModeDirectory, false, "emp", two, true},
		// without a directory a token carries no person and cannot draft (as before): the
		// service account drafts and a founder approves
		{ModeAttestation, false, "emp", two, false},
		{ModeAttestation, false, "emp", withSvc, true},
		// a founder: holder, drafter and approver are three roles, so two people need the
		// service account as drafter: break-glass in directory mode, as before without one
		{ModeDirectory, false, "ann", withSvc, false},
		{ModeDirectory, true, "ann", withSvc, true},
		{ModeAttestation, false, "ann", withSvc, true},
	} {
		got := paths(t, c.mode, c.breakGlass, c.holder, c.actors)
		if (len(got) > 0) != c.want {
			t.Errorf("%s break-glass=%v holder=%s %v: %d paths, want a path: %v", c.mode, c.breakGlass, c.holder, c.actors, len(got), c.want)
		}
	}
}

func TestGrantPathsForOneAdministrator(t *testing.T) {
	one, withSvc := []string{"ann"}, []string{"ann", "svc"}
	for _, mode := range []string{ModeDirectory, ModeAttestation} {
		// alone there is no path, whoever the holder is
		for _, h := range []string{"emp", "ann"} {
			if got := paths(t, mode, false, h, one); len(got) != 0 {
				t.Errorf("%s: one person alone approved: %+v", mode, got)
			}
		}
		// break-glass: the service account is the second actor for an employee's grant
		if got := paths(t, mode, true, "emp", withSvc); len(got) == 0 {
			t.Errorf("%s: no break-glass path to an employee grant", mode)
		}
	}
	// but never to one's own grant: that takes another board member, with or without break-glass
	if got := paths(t, ModeDirectory, true, "ann", withSvc); len(got) != 0 {
		t.Errorf("a single administrator approved their own grant: %+v", got)
	}
	if got := paths(t, ModeDirectory, true, "ann", []string{"ann", "svc", "cat"}); len(got) == 0 {
		t.Error("no path to a single administrator's own grant even with a second board member")
	}
}

// Exercises and ledger entries in a two-person company. An employee's exercise needs only the two
// founders. A founder's own needs the service account for the third role, in directory mode under
// break-glass, and the founder never approves, pays or issues it.
func TestTwoFounderExerciseAndLedgerPaths(t *testing.T) {
	for _, holder := range []string{"emp", "ann"} {
		e := newIDEnv(t, func(o *Options) { o.BreakGlassUntil = time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC) })
		const personEmp = "pers_0000000000000000000000e9"
		e.dir.subjects["user:emp"] = personEmp
		boss := Actor{Subject: "board-setup", Roles: []string{RoleBoard}}
		e.plan = m(e.s.CreatePlan(e.ctx, boss, e.ent.ID, PlanInput{Name: "Plan", ClassID: e.class.ID, Authorised: q(10_000), AdoptedOn: "2026-01-01"}))
		person := map[string]string{"emp": personEmp, "ann": personAnn}[holder]
		stk := m(e.holder(e.ben, person, "Holder"))
		m(e.s.VerifyPerson(e.ctx, e.ann, stk.ID))
		if holder == "ann" {
			// ann confirmed her own record, which ben entered
			e.must(e.issue(e.ben, stk.ID))
			wantErr(t, e.issue(e.ann, stk.ID), ErrSeparation, "own issuance")
		}
		// the grant: ben drafts an employee's, the service account a founder's
		drafter, approverSub := e.ben, "ann"
		if holder == "ann" {
			drafter, approverSub = e.svc, "ben"
		}
		id, _, err := e.s.CreateGrant(e.ctx, drafter, e.plan.ID, e.standardInput(stk.ID, q(4800)), "", nil)
		e.must(err)
		m(e.s.SubmitGrant(e.ctx, drafter, id))
		m(e.s.ApproveGrant(e.ctx, e.boardSeat(approverSub), id))
		m(e.s.ActivateGrant(e.ctx, drafter, id, ""))

		e.advance("2027-01-20")
		e.refresh()
		self := e.actor(Principal{Subject: "user:" + holder, Roles: []string{RoleEmployee}})
		ex, _, err := e.s.RequestExercise(e.ctx, self, id, q(1), "", nil)
		e.must(err)
		first, second := e.ann, e.ben
		if holder == "ann" {
			_, err = e.s.ApproveExercise(e.ctx, e.ann, ex)
			wantErr(t, err, ErrSeparation, "approving own exercise")
			first, second = e.ben, e.svc
		}
		m(e.s.ApproveExercise(e.ctx, first, ex))
		m(e.s.RecordPayment(e.ctx, first, ex, PaymentInput{AmountMinor: 125, PaidOn: "2027-01-20"}))
		_, err = e.s.IssueShares(e.ctx, first, ex)
		wantErr(t, err, ErrSeparation, "the payer issuing")
		m(e.s.IssueShares(e.ctx, second, ex))
	}
}

// A record attested, or labelled with a person id, while there was no directory is not trusted
// once there is one.
func TestSwitchingToDirectoryModeDistrustsUncheckedPersonIDs(t *testing.T) {
	e := newIDEnv(t)
	dir := e.s.directory
	e.s.directory = nil
	e.refresh()
	madeUp := m(e.holder(e.ann, personNone, "Made Up"))
	m(e.s.AttestExternal(e.ctx, e.ben, madeUp.ID))
	external := m(e.holder(e.ann, "", "Outside Investor"))
	m(e.s.AttestExternal(e.ctx, e.ben, external.ID))
	e.must(e.issue(e.ann, madeUp.ID))

	e.s.directory = dir
	e.refresh()
	wantErr(t, e.issue(e.ann, madeUp.ID), ErrConflict, "a made-up person id attested without a directory")
	items := m(e.s.IdentityReview(e.ctx, e.ann))
	if len(items) != 1 || items[0].StakeholderID != madeUp.ID || items[0].Reason != ReviewUnconfirmedLink {
		t.Fatalf("review = %+v", items)
	}
	var inv *InvalidError
	if _, err := e.s.VerifyPerson(e.ctx, e.ben, madeUp.ID); !errors.As(err, &inv) {
		t.Fatalf("confirming a made-up id: %v", err)
	}
	e.must(e.issue(e.ann, external.ID)) // no person id to check: the attestation stands
}

// Migration 004 clears the attestations of 003 and the records show up for review.
func TestMigrationClearsOldAttestations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	kr := m(ParseKeyring("k1 " + hexKeyA + "\n"))
	s := m(Open(path, Options{Keyring: kr}))
	// rebuild the schema as it was at version 3
	for _, stmt := range []string{
		`DROP TRIGGER stakeholders_verify_sod`,
		`ALTER TABLE stakeholders DROP COLUMN person_linked_by`, `ALTER TABLE stakeholders DROP COLUMN person_verified_by`,
		`ALTER TABLE stakeholders DROP COLUMN person_verified_on`, `ALTER TABLE stakeholders DROP COLUMN identity_by`,
		`ALTER TABLE stakeholders DROP COLUMN erased_by`, `DELETE FROM schema_migrations WHERE version = 4`,
		`INSERT INTO entities (id, name, currency, created_at) VALUES ('ent_1', 'E', 'USD', 1)`,
		`INSERT INTO stakeholders (id, entity_id, kind, person_id, created_by, created_at, updated_at, external_attested_by, external_attested_on)
			VALUES ('stk_1', 'ent_1', 'founder', NULL, 'svc-can-core', 1, 1, 'alice', '2026-01-01')`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = m(Open(path, Options{Keyring: kr}))
	defer func() { _ = s.Close() }()
	items := m(s.reviewItems(t.Context()))
	if len(items) != 1 || items[0].Reason != ReviewUnattested {
		t.Fatalf("review after upgrade = %+v", items)
	}
}

// Shares left on an erased record can still be cancelled or moved, by someone other than who
// erased it, and the record is listed until then.
func TestErasedHolderHasAnHonestPath(t *testing.T) {
	e := newIDEnv(t)
	old := m(e.holder(e.ann, "", "Gone"))
	m(e.s.AttestExternal(e.ctx, e.ben, old.ID))
	other := m(e.holder(e.ann, "", "Stays"))
	m(e.s.AttestExternal(e.ctx, e.ben, other.ID))
	e.must(e.issue(e.ann, old.ID))
	m(e.s.RequestErasure(e.ctx, e.ann, old.ID))
	if e.count(`SELECT COUNT(*) FROM stakeholders WHERE id = ? AND external_attested_by IS NULL AND person_verified_by IS NULL AND erased_by = 'user:ann'`, old.ID) != 1 {
		t.Fatal("erasure did not clear the confirmation or record who erased")
	}
	items := m(e.s.IdentityReview(e.ctx, e.ann))
	if len(items) != 1 || items[0].Reason != ReviewErasedHolder {
		t.Fatalf("review = %+v", items)
	}
	move := func(by Actor) error {
		_, _, err := e.s.TransferShares(e.ctx, by, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: old.ID, ToID: other.ID, Quantity: q(4), EffectiveDate: "2026-02-01"}, "", nil)
		return err
	}
	cancel := func(by Actor) error {
		_, _, err := e.s.CancelShares(e.ctx, by, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: old.ID, Quantity: q(6), EffectiveDate: "2026-02-01"}, "", nil)
		return err
	}
	wantErr(t, move(e.ann), ErrSeparation, "the eraser moving the shares")
	wantErr(t, cancel(e.ann), ErrSeparation, "the eraser cancelling the shares")
	wantErr(t, cancel(e.svc), ErrIdentityRequired, "the service account")
	wantErr(t, e.issue(e.ben, old.ID), ErrConflict, "issuing to an erased record")
	e.must(move(e.ben))
	e.must(cancel(e.ben))
	if items := m(e.s.IdentityReview(e.ctx, e.ann)); len(items) != 0 {
		t.Fatalf("review after the shares left = %+v", items)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'ledger.erased_holder'`); n != 2 {
		t.Fatalf("erased holder writes on record: %d", n)
	}
}

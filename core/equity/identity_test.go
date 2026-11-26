package equity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	personAnn  = "pers_0000000000000000000000c1"
	personBen  = "pers_0000000000000000000000c2"
	personCat  = "pers_0000000000000000000000c3"
	personNone = "pers_0000000000000000000000ff" // not in the directory
)

type fakeDir struct {
	subjects map[string]string
	down     bool
}

func (f *fakeDir) PersonForSubject(_ context.Context, sub string) (string, error) {
	if f.down {
		return "", errDirectory
	}
	return f.subjects[sub], nil
}

func (f *fakeDir) PersonExists(_ context.Context, id string) (bool, error) {
	if f.down {
		return false, errDirectory
	}
	return id != personNone, nil
}

// idEnv is a directory-mode service with two founders (ann and ben, both finance and admin), a
// third person (cat) and the service account.
type idEnv struct {
	*env
	dir           *fakeDir
	ann, ben, cat Actor
	svc           Actor
}

func (e *idEnv) actor(p Principal) Actor { return m(e.s.ResolveActor(e.ctx, &p)) }

func (e *idEnv) as(sub string) Actor {
	return e.actor(Principal{Subject: sub, Roles: []string{RoleFinance, RoleAdmin, RoleHR}, CanCore: true})
}

func (e *idEnv) refresh() {
	e.s.subjects.m = nil
	e.ann, e.ben, e.cat = e.as("user:ann"), e.as("user:ben"), e.as("user:cat")
	e.svc = e.actor(Principal{Subject: "svc-can-core", Roles: []string{RoleHR, RoleFinance, RoleAdmin}, Service: true})
}

func newIDEnv(t *testing.T, opts ...optFn) *idEnv {
	t.Helper()
	dir := &fakeDir{subjects: map[string]string{"user:ann": personAnn, "user:ben": personBen, "user:cat": personCat}}
	base := newEnv(t, append([]optFn{func(o *Options) { o.Directory = dir }}, opts...)...)
	e := &idEnv{env: base, dir: dir}
	e.refresh()
	boss := Actor{Subject: "board-x", Roles: []string{RoleBoard}}
	e.ent = m(e.s.CreateEntity(e.ctx, boss, EntityInput{Name: "Example Pty Ltd", Currency: "USD"}))
	e.class = m(e.s.CreateShareClass(e.ctx, boss, e.ent.ID, ShareClassInput{Name: "Common", Kind: "common", ParMinor: 1, Currency: "USD", Authorised: q(10_000_000)}))
	return e
}

func (e *idEnv) holder(by Actor, person, name string) (*Stakeholder, error) {
	return e.s.CreateStakeholder(e.ctx, by, e.ent.ID, StakeholderInput{Kind: "founder", PersonID: person, Identity: Identity{Name: name}})
}

func (e *idEnv) issue(by Actor, stk string) error {
	_, _, err := e.s.IssueNewShares(e.ctx, by, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: stk, Quantity: q(10), EffectiveDate: "2026-01-01"}, "", nil)
	return err
}

func wantErr(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: %v, want %v", what, err, want)
	}
}

func TestIdentityTwoFoundersEstablishEachOther(t *testing.T) {
	e := newIDEnv(t)
	// nobody registers themself
	_, err := e.holder(e.ann, personAnn, "Ann")
	wantErr(t, err, ErrSeparation, "self-registration")
	annStk := m(e.holder(e.ben, personAnn, "Ann"))
	benStk := m(e.holder(e.ann, personBen, "Ben"))
	// unconfirmed: no ledger writes, whoever asks
	wantErr(t, e.issue(e.ben, annStk.ID), ErrConflict, "issue to an unconfirmed holder")
	_, err = e.s.VerifyPerson(e.ctx, e.ben, annStk.ID)
	wantErr(t, err, ErrSeparation, "the creator confirming")
	if _, err := e.s.db.Exec(`UPDATE stakeholders SET person_verified_by = created_by WHERE id = ?`, annStk.ID); err == nil {
		t.Fatal("the database accepted the creator as confirmer")
	}
	// the holder confirms what the other founder entered
	if st := m(e.s.VerifyPerson(e.ctx, e.ann, annStk.ID)); !st.PersonVerified {
		t.Fatal("not verified")
	}
	m(e.s.VerifyPerson(e.ctx, e.ben, benStk.ID))
	// each issues the other's shares, never their own
	e.must(e.issue(e.ben, annStk.ID))
	e.must(e.issue(e.ann, benStk.ID))
	wantErr(t, e.issue(e.ann, annStk.ID), ErrSeparation, "own shares")
	_, err = e.s.RequestErasure(e.ctx, e.ann, annStk.ID)
	wantErr(t, err, ErrSeparation, "own erasure")
}

func TestIdentitySecondRecordAndMadeUpID(t *testing.T) {
	e := newIDEnv(t)
	_, err := e.holder(e.ann, personNone, "Nobody")
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("made-up person id: %v", err)
	}
	// a second record with no person: ann cannot use it or attest it alone
	shell := m(e.s.CreateStakeholder(e.ctx, e.ann, e.ent.ID, StakeholderInput{Kind: "investor", Identity: Identity{Name: "Ann Holdings"}}))
	wantErr(t, e.issue(e.ann, shell.ID), ErrConflict, "shell")
	_, err = e.s.AttestExternal(e.ctx, e.ann, shell.ID)
	wantErr(t, err, ErrSeparation, "self-attestation")
	_, err = e.s.AttestExternal(e.ctx, e.svc, shell.ID)
	wantErr(t, err, ErrIdentityRequired, "the service account as second actor")
	// a record borrowing cat's id is not usable by its creator alone either
	borrowed := m(e.holder(e.ann, personCat, "Ann Again"))
	wantErr(t, e.issue(e.ann, borrowed.ID), ErrConflict, "borrowed id")
	_, err = e.s.VerifyPerson(e.ctx, e.ann, borrowed.ID)
	wantErr(t, err, ErrSeparation, "self-confirmation")
	// nobody links a record to themself or away from themself
	_, err = e.s.LinkPerson(e.ctx, e.ann, shell.ID, personAnn)
	wantErr(t, err, ErrSeparation, "linking to self")
	annStk := m(e.holder(e.ben, personAnn, "Ann"))
	_, err = e.s.LinkPerson(e.ctx, e.ann, annStk.ID, personNone)
	wantErr(t, err, ErrSeparation, "unlinking self")
	// whoever links cannot confirm
	m(e.s.LinkPerson(e.ctx, e.ben, shell.ID, "pers_0000000000000000000000c4"))
	_, err = e.s.VerifyPerson(e.ctx, e.ben, shell.ID)
	wantErr(t, err, ErrSeparation, "the linker confirming")
}

func TestIdentityChangeClearsConfirmation(t *testing.T) {
	e := newIDEnv(t)
	ext := m(e.s.CreateStakeholder(e.ctx, e.ann, e.ent.ID, StakeholderInput{Kind: "investor", Identity: Identity{Name: "Ivy"}}))
	m(e.s.AttestExternal(e.ctx, e.ben, ext.ID))
	e.must(e.issue(e.ann, ext.ID))
	e.must(e.s.SetIdentity(e.ctx, e.ben, ext.ID, Identity{Name: "Someone Else"}))
	wantErr(t, e.issue(e.ann, ext.ID), ErrConflict, "after an identity change")
	_, err := e.s.AttestExternal(e.ctx, e.ben, ext.ID)
	wantErr(t, err, ErrSeparation, "whoever changed the identity attesting it")
	m(e.s.AttestExternal(e.ctx, e.cat, ext.ID))
	e.must(e.issue(e.ann, ext.ID))
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'stakeholder.confirmation.cleared'`) != 1 {
		t.Fatal("the cleared confirmation is not on record")
	}
	// relinking clears too; the holder never writes their own identity
	catStk := m(e.holder(e.ann, personCat, "Cat"))
	m(e.s.VerifyPerson(e.ctx, e.ben, catStk.ID))
	wantErr(t, e.s.SetIdentity(e.ctx, e.cat, catStk.ID, Identity{Name: "Cat B"}), ErrSeparation, "own identity")
	if st := m(e.s.LinkPerson(e.ctx, e.ann, catStk.ID, "pers_0000000000000000000000c4")); st.PersonVerified {
		t.Fatal("a relink kept the confirmation")
	}
}

func TestIdentityCallersFailClosed(t *testing.T) {
	e := newIDEnv(t)
	annStk := m(e.holder(e.ben, personAnn, "Ann"))
	// an account the directory does not know: reads by role work, writes about people do not
	stranger := e.as("user:ann2")
	m(e.s.ListStakeholders(e.ctx, stranger, e.ent.ID))
	_, err := e.holder(stranger, "", "x")
	wantErr(t, err, ErrIdentityRequired, "unlinked account")
	wantErr(t, e.issue(stranger, annStk.ID), ErrIdentityRequired, "unlinked ledger write")
	wantErr(t, e.issue(e.svc, annStk.ID), ErrIdentityRequired, "service account ledger write")
	// a claim the directory does not confirm is not believed
	liar := e.actor(Principal{Subject: "user:ann", Roles: []string{RoleFinance}, PersonID: personBen})
	wantErr(t, e.issue(liar, annStk.ID), ErrIdentityRequired, "disagreeing claim")

	e.dir.down = true
	e.refresh()
	m(e.s.ListStakeholders(e.ctx, e.ben, e.ent.ID))
	wantErr(t, e.issue(e.ben, annStk.ID), ErrIdentityUnavailable, "directory down")
	_, err = e.s.RequestErasure(e.ctx, e.ben, annStk.ID)
	wantErr(t, err, ErrIdentityUnavailable, "erasure with the directory down")
	if who := e.s.WhoAmI(e.ben); who.DirectoryAvailable || who.PersonID != "" || who.Mode != ModeDirectory {
		t.Fatalf("whoami = %+v", who)
	}

	api := &API{Svc: e.s, Auth: &StaticTokens{}, Log: nil}
	tokens := &StaticTokens{}
	tokens.Add("token-of-ben-000000000000", Principal{Subject: "user:ben", Roles: []string{RoleAdmin}})
	api.Auth = tokens
	r := httptest.NewRequest(http.MethodPost, "/v1/stakeholders/"+annStk.ID+"/erasure", nil)
	r.Header.Set("Authorization", "Bearer token-of-ben-000000000000")
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("erasure over HTTP with the directory down: %d %s", w.Code, w.Body)
	}
}

// A person linked in the directory, signing in with a personal access token, administers grants
// (before, every such token was refused); never their own.
func TestIdentityLinkedTokenAdministersGrants(t *testing.T) {
	e := newIDEnv(t)
	boss := Actor{Subject: "board-x", Roles: []string{RoleBoard}}
	e.plan = m(e.s.CreatePlan(e.ctx, boss, e.ent.ID, PlanInput{Name: "Plan", ClassID: e.class.ID, Authorised: q(1000), AdoptedOn: "2026-01-01"}))
	catStk := m(e.holder(e.ben, personCat, "Cat"))
	in := e.standardInput(catStk.ID, q(100))
	id, _, err := e.s.CreateGrant(e.ctx, e.ann, e.plan.ID, in, "", nil)
	e.must(err)
	_, err = e.s.SubmitGrant(e.ctx, e.ann, id)
	wantErr(t, err, ErrConflict, "submit before the link is confirmed")
	m(e.s.VerifyPerson(e.ctx, e.ann, catStk.ID))
	m(e.s.SubmitGrant(e.ctx, e.ann, id))
	_, _, err = e.s.CreateGrant(e.ctx, e.cat, e.plan.ID, in, "", nil)
	wantErr(t, err, ErrSeparation, "own grant")
	// ann drafted and submitted: she cannot approve, ben (who entered the record) can
	annBoard, benBoard := e.ann, e.ben
	annBoard.Roles, annBoard.CanCore = []string{RoleBoard}, false
	benBoard.Roles, benBoard.CanCore = []string{RoleBoard}, false
	_, err = e.s.ApproveGrant(e.ctx, annBoard, id)
	wantErr(t, err, ErrSeparation, "the drafter approving")
	m(e.s.ApproveGrant(e.ctx, benBoard, id))
}

// Records made before verified identities are not trusted, not lost, and listed.
func TestIdentityExistingRecordsAreReportedForReview(t *testing.T) {
	e := newIDEnv(t)
	legacy := m(e.holder(e.ben, personAnn, "Ann"))
	_, err := e.s.db.Exec(`UPDATE stakeholders SET created_by = 'svc-can-core', person_linked_by = NULL WHERE id = ?`, legacy.ID)
	if err == nil {
		t.Fatal("created_by is frozen; the test needs another way")
	}
	rec := "stk_0000000000000000000000aa"
	_, err = e.s.db.Exec(`INSERT INTO stakeholders (id, entity_id, kind, person_id, created_by, created_at, updated_at) VALUES (?,?,?,?,?,1,1)`,
		rec, e.ent.ID, "founder", personCat, "svc-can-core")
	e.must(err)
	items := m(e.s.IdentityReview(e.ctx, e.svc))
	if len(items) != 2 || items[0].Reason != ReviewUnconfirmedLink {
		t.Fatalf("review = %+v", items)
	}
	// still readable by staff, but not by the person it names and not yet writable
	catSelf := e.actor(Principal{Subject: "user:cat", Roles: []string{RoleEmployee}})
	_, err = e.s.GetStakeholder(e.ctx, catSelf, rec)
	wantErr(t, err, ErrNotFound, "self read of an unconfirmed record")
	m(e.s.GetStakeholder(e.ctx, e.ben, rec))
	wantErr(t, e.issue(e.ben, rec), ErrConflict, "legacy record")
	// made by the service account: the holder alone must not confirm it
	_, err = e.s.VerifyPerson(e.ctx, e.cat, rec)
	wantErr(t, err, ErrSeparation, "holder confirming a service-made record")
	m(e.s.VerifyPerson(e.ctx, e.ben, rec))
	m(e.s.GetStakeholder(e.ctx, catSelf, rec))
	e.must(e.issue(e.ben, rec))
}

func TestIdentityBreakGlass(t *testing.T) {
	until := time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC)
	e := newIDEnv(t, func(o *Options) { o.BreakGlassUntil = until })
	founder := m(e.holder(e.svc, personAnn, "Ann"))
	e.must(e.issue(e.svc, founder.ID)) // a single-admin company's first issuance
	wantErr(t, e.issue(e.ann, founder.ID), ErrSeparation, "break-glass is the service account's only")
	other := m(e.holder(e.ann, personBen, "Ben"))
	wantErr(t, e.issue(e.ann, other.ID), ErrConflict, "no waiver for a person")
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'break_glass:ledger.write'`) != 1 {
		t.Fatal("break-glass use is not on record")
	}
	e.advance("2026-03-12")
	e.refresh()
	wantErr(t, e.issue(e.svc, founder.ID), ErrIdentityRequired, "after the window")
}

func TestIdentityBreakGlassSetting(t *testing.T) {
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	if u, err := parseBreakGlass("2026-03-12", now); err != nil || !u.Equal(time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("date: %v %v", u, err)
	}
	for _, ok := range []string{"2026-03-17T09:00:00Z", "2026-03-16"} { // exactly seven days; the last whole day inside them
		if _, err := parseBreakGlass(ok, now); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"yes", "2027-01-01", "2026-03-17T09:00:01Z", "2026-03-17"} {
		if _, err := parseBreakGlass(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParseConfig([]byte("keyring_file: /k\ndirectory: {url: \"http://people-api:8081\"}\n")); err == nil {
		t.Error("plain http to a non-loopback directory accepted")
	}
	c := m(ParseConfig([]byte("keyring_file: /k\ndirectory: {url: \"http://people-api:8081\", allow_insecure_http: true}\n")))
	if _, err := SecretsFromEnv(func(k string) string {
		return map[string]string{"CAN_EQUITY_API_TOKEN": "0123456789012345678901234"}[k]
	}, c); err == nil {
		t.Error("a directory without a token accepted")
	}
}

// Without the people module a person_id proves nothing: a second actor's attestation does.
func TestIdentityAttestationModeDoesNotTrustPersonID(t *testing.T) {
	e := newEnv(t).base()
	st := m(e.s.CreateStakeholder(e.ctx, e.fin, e.ent.ID, StakeholderInput{Kind: "founder", PersonID: personNone, Identity: Identity{Name: "Made Up"}}))
	_, _, err := e.s.IssueNewShares(e.ctx, e.fin, e.ent.ID, LedgerInput{ClassID: e.class.ID, StakeholderID: st.ID, Quantity: q(1), EffectiveDate: "2026-01-01"}, "", nil)
	wantErr(t, err, ErrConflict, "made-up id")
	_, err = e.s.AttestExternal(e.ctx, e.fin, st.ID)
	wantErr(t, err, ErrSeparation, "self-attestation")
	_, err = e.s.VerifyPerson(e.ctx, e.fin2, st.ID)
	wantErr(t, err, ErrConflict, "verify without a directory")
	m(e.s.AttestExternal(e.ctx, e.fin2, st.ID))
}

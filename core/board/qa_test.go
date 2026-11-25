package board

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func httptestRequest(tok string) *http.Request {
	r, _ := http.NewRequest("GET", "http://x/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func TestSecretaryCannotReadLiveVotesAndWithdrawKeepsThem(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	res := e.proposed(mt, e.sec)
	m(e.s.OpenVote(e.ctx, e.sec, res.ID))
	m(e.s.CastVote(e.ctx, e.d1, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, res.ID, VoteInput{Choice: "against"}))
	if _, err := e.s.ListVotes(e.ctx, e.sec, res.ID); !isErr(err, ErrForbidden) {
		t.Errorf("secretary reading live votes: %v", err)
	}
	// a reason is required once voting is open
	if _, err := e.s.WithdrawResolution(e.ctx, e.sec, res.ID, ""); err == nil {
		t.Error("withdrawn mid-vote without a reason")
	}
	v := m(e.s.WithdrawResolution(e.ctx, e.sec, res.ID, "the supplier withdrew its offer"))
	if v.Status != ResWithdrawn || v.Result == nil || v.Result.For != 1 || v.Result.Against != 1 || v.WithdrawReason == "" {
		t.Fatalf("%+v", v)
	}
	if votes := m(e.s.ListVotes(e.ctx, e.sec, res.ID)); len(votes) != 2 {
		t.Errorf("the votes cast before the withdrawal are gone: %d", len(votes))
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'withdraw' AND resource = ?`, "resolution:"+res.ID) != 1 {
		t.Error("the withdrawal was not audited")
	}
	for _, q := range []string{`DELETE FROM resolution_withdrawals`, `UPDATE resolution_withdrawals SET withdrawn_by = 'x'`} {
		if _, err := e.s.db.Exec(q); err == nil {
			t.Errorf("the database accepted: %s", q)
		}
	}
	// the same for a secret ballot: the ballots are finalised, shuffled, and the staging is gone
	id, _, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, ResolutionInput{MeetingID: mt.ID, SecretBallot: true, Title: "t", Text: "x"}, "", nil)
	e.must(err)
	m(e.s.ProposeResolution(e.ctx, e.sec, id))
	m(e.s.OpenVote(e.ctx, e.sec, id))
	m(e.s.CastVote(e.ctx, e.d1, id, VoteInput{Choice: "for"}))
	m(e.s.WithdrawResolution(e.ctx, e.sec, id, "no longer needed"))
	if e.count(`SELECT COUNT(*) FROM ballots WHERE resolution_id = ?`, id) != 1 || e.count(`SELECT COUNT(*) FROM ballot_staging`) != 0 || e.count(`SELECT COUNT(*) FROM receipt_staging`) != 0 {
		t.Error("the staged ballot was not finalised on withdrawal")
	}
}

func TestRequestsAfterWithdrawalAndConcurrentRequests(t *testing.T) {
	e := newEnv(t).base()
	req := ResolutionRequest{Type: "equity.grant", Ref: "grt_0123456789abcdef01234567", BodyID: e.body.ID, Requester: "hr-alice"}
	first := m(e.s.RequestResolution(e.ctx, e.integ, req))
	m(e.s.WithdrawResolution(e.ctx, e.sec, first.ResolutionID, ""))
	second := m(e.s.RequestResolution(e.ctx, e.integ, req))
	if second.ResolutionID == first.ResolutionID || second.Outcome != "pending" {
		t.Errorf("a request after a withdrawal must create a fresh resolution: %+v", second)
	}
	// two identical requests racing: the unique index lets one create, the other gets the same row (not a 500)
	req.Ref = "grt_aaaaaaaaaaaaaaaaaaaaaaaa"
	done := make(chan *ResolutionOutcome, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			o, err := e.s.RequestResolution(e.ctx, e.integ, req)
			done <- o
			errs <- err
		}()
	}
	a, b := <-done, <-done
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if a.ResolutionID != b.ResolutionID || e.count(`SELECT COUNT(*) FROM resolutions WHERE external_ref = ? AND status != 'withdrawn'`, req.Ref) != 1 {
		t.Errorf("%+v %+v", a, b)
	}
	// the integration subject reads only the outcomes of requests it created
	other := Actor{Subject: "svc-other", Roles: []string{RoleIntegration}}
	if _, err := e.s.OutcomeByRef(e.ctx, other, req.Type, req.Ref); !isErr(err, ErrNotFound) {
		t.Errorf("another integration subject read the outcome: %v", err)
	}
}

func TestMessagesDoNotEchoRequestValues(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	probe := "<script>alert(1)</script>"
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: accepted", what)
			return
		}
		if strings.Contains(err.Error(), probe) {
			t.Errorf("%s echoes the request value: %v", what, err)
		}
	}
	_, err := e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, probe, "", false)
	check("as_of", err)
	_, err = e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: probe, Office: "director", EffectiveOn: "2026-01-01"})
	check("party_ref", err)
	_, err = e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: "p", Office: "director", EffectiveOn: probe})
	check("effective_on", err)
	_, err = e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: probe, PartyRef: "p", Office: "director", EffectiveOn: "2026-01-01"})
	check("kind", err)
	_, err = e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, "", probe, false)
	check("known_at", err)
	check("purge class", e.s.PurgeSubject(e.ctx, pD1, probe))
	// the conflict messages of the register replay name no party and no date
	m(e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: "party-x", Office: "director", EffectiveOn: "2026-01-01"}))
	_, err = e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: "party-x", Office: "director", EffectiveOn: "2026-01-02"})
	if err == nil || strings.Contains(err.Error(), "party-x") || strings.Contains(err.Error(), "2026") {
		t.Errorf("%v", err)
	}
	// attendance after approval: a constant message, not the raw trigger text
	mt := e.meeting(pD1, pD2, pD3)
	m(e.s.SetMeetingStatus(e.ctx, e.sec, mt.ID, "held"))
	min := m(e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "x"))
	m(e.s.ApproveMinutes(e.ctx, e.d2, min.ID))
	_, err = e.s.RecordAttendance(e.ctx, e.sec, mt.ID, pD4, "present")
	if err == nil || strings.Contains(err.Error(), "append") || strings.Contains(err.Error(), "ABORT") || strings.Contains(err.Error(), "trigger") {
		t.Errorf("%v", err)
	}
}

func TestEntityRecordsAreScopedToTheirBodies(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity() // links the base body to this entity
	other := m(e.s.CreateLegalEntity(e.ctx, e.sec, LegalEntityInput{Name: "Other Pty Ltd"}))
	m(e.s.RecordEntry(e.ctx, e.sec, other.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD2, Office: "director", EffectiveOn: "2026-01-01"}))
	if _, err := e.s.GetLegalEntity(e.ctx, e.d1, le.ID); err != nil {
		t.Error(err)
	}
	if _, err := e.s.GetLegalEntity(e.ctx, e.d1, other.ID); !isErr(err, ErrNotFound) {
		t.Errorf("a director read an entity their body is not tied to: %v", err)
	}
	if _, err := e.s.ViewRegister(e.ctx, e.d1, other.ID, RegDirectors, "", "", false); !isErr(err, ErrNotFound) {
		t.Errorf("a director read another entity's register: %v", err)
	}
	if l := m(e.s.ListLegalEntities(e.ctx, e.d1)); len(l) != 1 || l[0].ID != le.ID {
		t.Errorf("%+v", l)
	}
	if l := m(e.s.ListLegalEntities(e.ctx, e.sec)); len(l) != 2 {
		t.Errorf("the secretary sees %d entities", len(l))
	}
	if l := m(e.s.ListLegalEntities(e.ctx, e.emp)); len(l) != 0 {
		t.Errorf("an employee sees %d entities", len(l))
	}
}

func TestAdminTokenIsSeparateFromTheIntegrationToken(t *testing.T) {
	get := func(k string) string {
		return map[string]string{"CAN_BOARD_API_TOKEN": strings.Repeat("a", 30), "CAN_BOARD_ADMIN_TOKEN": strings.Repeat("a", 30)}[k]
	}
	cfg, _ := ParseConfig([]byte("keyring_file: /k\n"))
	if _, err := SecretsFromEnv(get, cfg); err == nil {
		t.Error("the admin token may not equal the integration token")
	}
	ok := func(k string) string {
		return map[string]string{"CAN_BOARD_API_TOKEN": strings.Repeat("a", 30), "CAN_BOARD_ADMIN_TOKEN": strings.Repeat("b", 30)}[k]
	}
	if _, err := SecretsFromEnv(ok, cfg); err != nil {
		t.Error(err)
	}
}

func TestIntegrationAccountIsNotRateLimitedPerSubject(t *testing.T) {
	a := newAPIEnv(t)
	for i := 0; i < 300; i++ {
		if code, _, _ := a.do(tokInteg, "GET", "/v1/resolution-outcomes?type=equity.grant&ref=nothing", nil); code == 429 {
			t.Fatalf("the service account was throttled at request %d", i)
		}
	}
	limited := false
	for i := 0; i < 300; i++ {
		if code, _, _ := a.do(tokD1, "GET", "/v1/bodies", nil); code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("an ordinary subject was never limited")
	}
}

func TestSecretCastWritesNoPerVoterAuditRow(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	id, _, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, ResolutionInput{MeetingID: mt.ID, SecretBallot: true, Title: "t", Text: "x"}, "", nil)
	e.must(err)
	m(e.s.ProposeResolution(e.ctx, e.sec, id))
	m(e.s.OpenVote(e.ctx, e.sec, id))
	before := e.count(`SELECT COUNT(*) FROM audit_log`)
	for _, a := range []Actor{e.d3, e.d1, e.d2} {
		e.clock = e.clock.Add(time.Minute)
		if v := m(e.s.CastVote(e.ctx, a, id, VoteInput{Choice: "for"})); !v.Voted {
			t.Error("the cast response does not confirm the vote")
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log`); n != before {
		t.Errorf("%d audit rows were written by secret casts (cast-ordered timestamps)", n-before)
	}
	m(e.s.CloseVote(e.ctx, e.sec, id))
	if e.count(`SELECT COUNT(DISTINCT at) FROM audit_log WHERE action = 'voted' AND resource = ?`, "resolution:"+id) != 1 {
		t.Error("the voted markers carry distinct (cast-ordered) timestamps")
	}
}

func TestSSOCannotGrantServiceRoles(t *testing.T) {
	got := knownRoles([]string{RoleDirector, RoleAdmin, RoleIntegration, RoleSecretary, "owner"})
	if strings.Join(got, ",") != "director,secretary" {
		t.Errorf("%v", got)
	}
	st := &StaticTokens{}
	st.Add("tok-static-000000000000000000", Principal{Subject: "svc", Roles: []string{RoleAdmin, RoleIntegration}})
	p, err := st.Authenticate(httptestRequest("tok-static-000000000000000000"))
	if err != nil || len(p.Roles) != 2 {
		t.Errorf("%v %v", p, err)
	}
}

func TestRequestsCapUnfinishedDrafts(t *testing.T) {
	e := newEnv(t).base()
	for i := 0; i < maxOpenRequests; i++ {
		_, err := e.s.db.Exec(`INSERT INTO resolutions (id, body_id, kind, status, proposer_ref, pass_num, pass_den, pass_inclusive, external_type, external_ref, created_by, created_at, updated_at) VALUES (?,?,'meeting','draft','x',1,2,0,'equity.grant',?,?,1,1)`,
			newID("res"), e.body.ID, "ref-"+time.Duration(i).String(), canonSubject(e.integ.Subject))
		e.must(err)
	}
	_, err := e.s.RequestResolution(e.ctx, e.integ, ResolutionRequest{Type: "equity.grant", Ref: "grt_0123456789abcdef01234567", BodyID: e.body.ID, Requester: "hr-alice"})
	if !isErr(err, ErrConflict) {
		t.Errorf("%v", err)
	}
}

package board

import (
	"strings"
	"testing"
)

func TestInvisibleIs404ForEveryRecordType(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	res := e.proposed(mt, e.sec)
	min := m(e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "Minutes: the budget was discussed."))
	m(e.s.AddPackItem(e.ctx, e.sec, mt.ID, PackInput{DocRef: "doc:budget-fy27", Title: "FY27 budget"}))
	act := m(e.s.CreateAction(e.ctx, e.sec, mt.ID, ActionInput{Text: "Circulate the budget", AssigneeID: pD2}))

	// a director who is not a member of this body, an employee and a bare director token see nothing
	other := Actor{Subject: "sso-x", Roles: []string{RoleDirector}, PersonID: "pers_00000000000000000000d0ff"}
	bare := Actor{Subject: "sso-bare", Roles: []string{RoleDirector}}
	for name, a := range map[string]Actor{"non-member director": other, "bare director": bare, "employee": e.emp} {
		checks := map[string]func() error{
			"body":       func() error { _, err := e.s.GetBody(e.ctx, a, e.body.ID); return err },
			"meeting":    func() error { _, err := e.s.GetMeeting(e.ctx, a, mt.ID); return err },
			"resolution": func() error { _, err := e.s.GetResolution(e.ctx, a, res.ID); return err },
			"minutes":    func() error { _, err := e.s.GetMinutes(e.ctx, a, min.ID); return err },
			"pack":       func() error { _, err := e.s.GetPack(e.ctx, a, mt.ID); return err },
			"action":     func() error { _, err := e.s.GetAction(e.ctx, a, act.ID); return err },
			"agenda":     func() error { _, err := e.s.ListAgenda(e.ctx, a, mt.ID); return err },
			"members":    func() error { _, err := e.s.ListMembers(e.ctx, a, e.body.ID, ""); return err },
			"votes":      func() error { _, err := e.s.ListVotes(e.ctx, a, res.ID); return err },
		}
		for what, f := range checks {
			if err := f(); !isErr(err, ErrNotFound) {
				t.Errorf("%s reading %s: %v, want not found", name, what, err)
			}
		}
	}
	// a missing id and a malformed one answer the same as an invisible one
	for _, id := range []string{"mtg_ffffffffffffffffffffffff", "nonsense"} {
		if _, err := e.s.GetMeeting(e.ctx, other, id); !isErr(err, ErrNotFound) {
			t.Errorf("GetMeeting(%q) = %v", id, err)
		}
	}
	// lists are filtered, not refused
	if l := m(e.s.ListBodies(e.ctx, other)); len(l) != 0 {
		t.Errorf("a non-member sees %d bodies", len(l))
	}
	if l := m(e.s.ListBodies(e.ctx, e.d1)); len(l) != 1 {
		t.Errorf("a member sees %d bodies", len(l))
	}
	// the observer sees the body and the meeting but neither the resolution, the pack nor the draft minutes
	if _, err := e.s.GetMeeting(e.ctx, e.obs, mt.ID); err != nil {
		t.Error(err)
	}
	for _, f := range []func() error{
		func() error { _, err := e.s.GetResolution(e.ctx, e.obs, res.ID); return err },
		func() error { _, err := e.s.GetPack(e.ctx, e.obs, mt.ID); return err },
		func() error { _, err := e.s.GetMinutes(e.ctx, e.obs, min.ID); return err },
	} {
		if err := f(); !isErr(err, ErrNotFound) {
			t.Errorf("observer: %v, want not found", err)
		}
	}
	// the admin sees the structure but no board content
	if _, err := e.s.GetResolution(e.ctx, e.adm, res.ID); err != nil {
		t.Fatal(err)
	}
	r := m(e.s.GetResolution(e.ctx, e.adm, res.ID))
	if r.Text != "" || r.Title != "" || !r.TextWithheld {
		t.Errorf("admin saw the resolution text: %+v", r)
	}
	if _, err := e.s.GetMinutes(e.ctx, e.adm, min.ID); !isErr(err, ErrNotFound) {
		t.Errorf("admin read draft minutes: %v", err)
	}
	if _, err := e.s.GetPack(e.ctx, e.adm, mt.ID); !isErr(err, ErrNotFound) {
		t.Errorf("admin read the pack: %v", err)
	}
}

func TestMeetingNoticeIsDataAndEventsCarryIdsOnly(t *testing.T) {
	e := newEnv(t).base()
	_, err := e.s.ScheduleMeeting(e.ctx, e.sec, e.body.ID, MeetingInput{Title: "Short", StartsOn: "2026-03-12"})
	if err == nil || !strings.Contains(err.Error(), "7 days notice") {
		t.Fatalf("short notice: %v", err)
	}
	mt := m(e.s.ScheduleMeeting(e.ctx, e.sec, e.body.ID, MeetingInput{Title: "Short but waived", StartsOn: "2026-03-12", WaiverRef: "doc:waiver-2026-03"}))
	if mt.ShortNoticeWaiverRef == "" || mt.NoticeDaysRequired != 7 {
		t.Errorf("%+v", mt)
	}
	ok := m(e.s.ScheduleMeeting(e.ctx, e.sec, e.body.ID, MeetingInput{Title: "Proper", StartsOn: "2026-03-20", WaiverRef: "ignored"}))
	if ok.ShortNoticeWaiverRef != "" {
		t.Error("a waiver is recorded only when notice was short")
	}
	// a director who is not the chair cannot schedule; the chair can
	if _, err := e.s.ScheduleMeeting(e.ctx, e.d2, e.body.ID, MeetingInput{Title: "x", StartsOn: "2026-03-30"}); !isErr(err, ErrForbidden) {
		t.Errorf("member scheduling: %v", err)
	}
	if _, err := e.s.ScheduleMeeting(e.ctx, e.d1, e.body.ID, MeetingInput{Title: "Chair's meeting", StartsOn: "2026-03-30"}); err != nil {
		t.Errorf("chair scheduling: %v", err)
	}
	evs := m(e.s.AllEvents(e.ctx))
	if len(evs) != 3 {
		t.Fatalf("%d events", len(evs))
	}
	for _, ev := range evs {
		if ev.Type != EventMeetingScheduled || ev.Source != "board" {
			t.Errorf("%+v", ev)
		}
		for k := range ev.Data {
			if k != "meeting_id" && k != "body_id" && k != "starts_on" {
				t.Errorf("event data key %q: events carry ids and the date only", k)
			}
		}
	}
}

func TestQuorumIsExactAndBlocksTheVote(t *testing.T) {
	e := newEnv(t).base() // five voting members, quorum 1/2 rounded up = 3
	mt := e.meeting(pD1, pD2)
	res := e.proposed(mt, e.sec)
	q := m(e.s.Quorum(e.ctx, e.sec, mt.ID))
	if q.Eligible != 5 || q.Present != 2 || q.Required != 3 || q.Met {
		t.Errorf("%+v", q)
	}
	if _, err := e.s.OpenVote(e.ctx, e.sec, res.ID); err == nil || !isErr(err, ErrConflict) || !strings.Contains(err.Error(), "no quorum") {
		t.Fatalf("open without quorum: %v", err)
	}
	m(e.s.RecordAttendance(e.ctx, e.sec, mt.ID, pD3, "remote"))
	if q := m(e.s.Quorum(e.ctx, e.sec, mt.ID)); !q.Met || q.Present != 3 {
		t.Errorf("%+v", q)
	}
	v := m(e.s.OpenVote(e.ctx, e.sec, res.ID))
	if v.Status != ResVoting || v.Eligible != 5 || v.QuorumRequired != 3 || v.QuorumPresent != 3 {
		t.Errorf("%+v", v)
	}
	// a person who is not recorded present cannot vote; quorum lost mid-vote stops further votes
	if _, err := e.s.CastVote(e.ctx, e.d4, res.ID, VoteInput{Choice: "for"}); !isErr(err, ErrConflict) {
		t.Errorf("absent director voting: %v", err)
	}
	m(e.s.RecordAttendance(e.ctx, e.sec, mt.ID, pD3, "apology")) // d3 leaves
	if _, err := e.s.CastVote(e.ctx, e.d1, res.ID, VoteInput{Choice: "for"}); err == nil || !strings.Contains(err.Error(), "quorum is lost") {
		t.Errorf("vote after quorum loss: %v", err)
	}
}

func TestRollCallPassesAndTheTallyIsImmutable(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3, pD4)
	res := e.proposed(mt, e.d2) // d2 proposes
	m(e.s.OpenVote(e.ctx, e.d1, res.ID))
	m(e.s.CastVote(e.ctx, e.d1, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d3, res.ID, VoteInput{Choice: "against"}))
	m(e.s.CastVote(e.ctx, e.d4, res.ID, VoteInput{Choice: "abstain"}))
	if _, err := e.s.CastVote(e.ctx, e.d4, res.ID, VoteInput{Choice: "for"}); !isErr(err, ErrConflict) {
		t.Errorf("second vote: %v", err)
	}
	// no running tally leaks while voting is open, and per-voter detail is not readable yet by a member
	if v := m(e.s.GetResolution(e.ctx, e.d3, res.ID)); v.Result != nil || !v.Voted {
		t.Errorf("%+v", v)
	}
	if _, err := e.s.ListVotes(e.ctx, e.d3, res.ID); !isErr(err, ErrForbidden) {
		t.Errorf("member reading votes while open: %v", err)
	}
	if _, err := e.s.CloseVote(e.ctx, e.d2, res.ID); !isErr(err, ErrForbidden) {
		t.Errorf("a non-chair member closing: %v", err)
	}
	done := m(e.s.CloseVote(e.ctx, e.d1, res.ID))
	if done.Status != ResPassed || done.Result == nil || done.Result.For != 2 || done.Result.Against != 1 || done.Result.Abstain != 1 || done.Result.Reason != "tally" {
		t.Fatalf("%+v", done)
	}
	votes := m(e.s.ListVotes(e.ctx, e.d3, res.ID))
	if len(votes) != 4 {
		t.Errorf("%d votes", len(votes))
	}
	// the event carries ids and the result only
	var passed []Event
	for _, ev := range m(e.s.AllEvents(e.ctx)) {
		if ev.Type == EventResolutionPassed {
			passed = append(passed, ev)
		}
	}
	if len(passed) != 1 {
		t.Fatalf("%d passed events", len(passed))
	}
	d := passed[0].Data
	if d["resolution_id"] != res.ID || d["body_id"] != e.body.ID || d["result"] != "passed" || d["meeting_id"] != mt.ID {
		t.Errorf("%v", d)
	}
	for k := range d {
		switch k {
		case "resolution_id", "body_id", "meeting_id", "result", "passed_on":
		default:
			t.Errorf("event data key %q", k)
		}
	}
	// immutability is enforced by the database, not just by the service
	for _, q := range []string{
		`UPDATE resolutions SET result_for = 9 WHERE id = '` + res.ID + `'`,
		`UPDATE resolutions SET status = 'failed' WHERE id = '` + res.ID + `'`,
		`UPDATE votes SET choice = 'for' WHERE resolution_id = '` + res.ID + `'`,
		`DELETE FROM votes WHERE resolution_id = '` + res.ID + `'`,
		`INSERT INTO votes (id, resolution_id, voter_person, voter_ref, choice, method, cast_at) VALUES ('vot_x', '` + res.ID + `', '` + pD5 + `', 'x', 'for', 'roll_call', 1)`,
		`DELETE FROM resolutions WHERE id = '` + res.ID + `'`,
		`INSERT INTO resolution_versions (resolution_id, version, sens_title, sens_text, content_sha256, created_by, created_at) VALUES ('` + res.ID + `', 9, 'x', 'x', 'x', 'x', 1)`,
	} {
		if _, err := e.s.db.Exec(q); err == nil {
			t.Errorf("the database accepted: %s", q)
		}
	}
	if _, err := e.s.WithdrawResolution(e.ctx, e.sec, res.ID, ""); !isErr(err, ErrConflict) {
		t.Errorf("withdrawing a decided resolution: %v", err)
	}
	if _, err := e.s.AmendResolution(e.ctx, e.d2, res.ID, AmendInput{Title: "x", Text: "y"}); !isErr(err, ErrConflict) {
		t.Errorf("amending a decided resolution: %v", err)
	}
}

func TestThresholdsAreDataAndExact(t *testing.T) {
	e := newEnv(t)
	body := m(e.s.CreateBody(e.ctx, e.sec, BodyInput{Name: "Two thirds board", Kind: "board", QuorumNum: 1, QuorumDen: 2, PassNum: 2, PassDen: 3, PassInclusive: true}))
	e.body = body
	for _, p := range []string{pD1, pD2, pD3} {
		m(e.s.AddMember(e.ctx, e.sec, body.ID, MemberInput{PersonID: p, Role: map[bool]string{true: "chair", false: "member"}[p == pD1], StartDate: "2025-01-01"}))
	}
	mt := e.meeting(pD1, pD2, pD3)
	res := e.proposed(mt, e.sec)
	m(e.s.OpenVote(e.ctx, e.sec, res.ID))
	m(e.s.CastVote(e.ctx, e.d1, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d3, res.ID, VoteInput{Choice: "against"}))
	if v := m(e.s.CloseVote(e.ctx, e.sec, res.ID)); v.Status != ResPassed { // 2 of 3 cast is exactly two thirds: inclusive
		t.Errorf("%+v", v)
	}
	// strict at the same count fails
	res2 := m(func() (*Resolution, error) {
		id, _, err := e.s.CreateResolution(e.ctx, e.sec, body.ID, ResolutionInput{MeetingID: mt.ID, Title: "Strict", Text: "t", PassNum: 2, PassDen: 3, PassInclusive: boolPtr(false)}, "", nil)
		if err != nil {
			return nil, err
		}
		if _, err := e.s.ProposeResolution(e.ctx, e.sec, id); err != nil {
			return nil, err
		}
		return e.s.GetResolution(e.ctx, e.sec, id)
	}())
	m(e.s.OpenVote(e.ctx, e.sec, res2.ID))
	m(e.s.CastVote(e.ctx, e.d1, res2.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, res2.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d3, res2.ID, VoteInput{Choice: "against"}))
	if v := m(e.s.CloseVote(e.ctx, e.sec, res2.ID)); v.Status != ResFailed || v.Result.Reason != "tally" {
		t.Errorf("%+v", v)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestRecusalConflictsAndSubjects(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3, pD4, pD5)
	// d5 is the subject (for example the grantee of an option grant who sits on the board): recused automatically
	res := e.proposed(mt, e.sec, pD5)
	// d4 declares an interest and recuses; d3 declares a disclosed interest and may vote
	if _, err := e.s.DeclareConflict(e.ctx, e.d4, res.ID, ConflictInput{Treatment: "recuse", Nature: "brother-in-law is the supplier"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.DeclareConflict(e.ctx, e.d3, res.ID, ConflictInput{Treatment: "disclosed", Nature: "owns shares in the supplier"}); err != nil {
		t.Fatal(err)
	}
	// a director declares only for themselves
	if _, err := e.s.DeclareConflict(e.ctx, e.d2, res.ID, ConflictInput{PersonID: pD1, Treatment: "recuse"}); !isErr(err, ErrForbidden) {
		t.Errorf("declaring for someone else: %v", err)
	}
	// the nature is sealed: the chair and the secretary read it, another member does not, the recused person reads their own
	if c := m(e.s.GetResolution(e.ctx, e.d1, res.ID)).Conflicts; len(c) != 2 || c[0].Nature == "" {
		t.Errorf("chair: %+v", c)
	}
	if c := m(e.s.GetResolution(e.ctx, e.d2, res.ID)).Conflicts; len(c) != 2 || c[0].Nature != "" || c[1].Nature != "" {
		t.Errorf("member: %+v", c)
	}
	// the recused and the subject see that the resolution exists but not its text
	for _, a := range []Actor{e.d4, e.d5} {
		r := m(e.s.GetResolution(e.ctx, a, res.ID))
		if !r.TextWithheld || r.Text != "" {
			t.Errorf("%s read the deliberation: %+v", a.Subject, r)
		}
	}
	v := m(e.s.OpenVote(e.ctx, e.sec, res.ID))
	if v.Eligible != 3 { // d1, d2, d3
		t.Errorf("eligible = %d, want 3", v.Eligible)
	}
	// quorum excludes the recused and the subject: 3 eligible, required 2
	if v.QuorumRequired != 2 {
		t.Errorf("quorum required = %d, want 2", v.QuorumRequired)
	}
	for _, a := range []Actor{e.d4, e.d5} {
		if _, err := e.s.CastVote(e.ctx, a, res.ID, VoteInput{Choice: "for"}); !isErr(err, ErrForbidden) {
			t.Errorf("%s voting: %v", a.Subject, err)
		}
	}
	if _, err := e.s.DeclareConflict(e.ctx, e.d1, res.ID, ConflictInput{Treatment: "recuse"}); !isErr(err, ErrConflict) {
		t.Errorf("declaring after voting opened: %v", err)
	}
	m(e.s.CastVote(e.ctx, e.d3, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, res.ID, VoteInput{Choice: "for"}))
	if r := m(e.s.CloseVote(e.ctx, e.sec, res.ID)); r.Status != ResPassed {
		t.Errorf("%+v", r)
	}
	// abstain treatment: may only abstain
	res2 := e.proposed(mt, e.sec)
	m(e.s.DeclareConflict(e.ctx, e.d2, res2.ID, ConflictInput{Treatment: "abstain"}))
	m(e.s.OpenVote(e.ctx, e.sec, res2.ID))
	if _, err := e.s.CastVote(e.ctx, e.d2, res2.ID, VoteInput{Choice: "for"}); !isErr(err, ErrConflict) {
		t.Errorf("abstain-treatment director voting for: %v", err)
	}
	if _, err := e.s.CastVote(e.ctx, e.d2, res2.ID, VoteInput{Choice: "abstain"}); err != nil {
		t.Error(err)
	}
	// when nobody is left to vote the vote cannot open
	res3 := e.proposed(mt, e.sec, pD1, pD2, pD3, pD4, pD5)
	if _, err := e.s.OpenVote(e.ctx, e.sec, res3.ID); !isErr(err, ErrConflict) {
		t.Errorf("open with every member a subject: %v", err)
	}
}

func TestProposerIsNeverTheSoleApprover(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	// d2 proposes and is the only one for; the others abstain: it cannot pass
	res := e.proposed(mt, e.d2)
	m(e.s.OpenVote(e.ctx, e.sec, res.ID))
	m(e.s.CastVote(e.ctx, e.d2, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d1, res.ID, VoteInput{Choice: "abstain"}))
	m(e.s.CastVote(e.ctx, e.d3, res.ID, VoteInput{Choice: "abstain"}))
	if v := m(e.s.CloseVote(e.ctx, e.sec, res.ID)); v.Status != ResFailed || v.Result.Reason != "proposer_sole_approver" {
		t.Errorf("%+v", v)
	}
	// the same person under a different subject spelling (case, zero-width, compatibility characters) is still the proposer
	prop := Actor{Subject: "Sso-D2", Roles: []string{RoleDirector}, PersonID: ""}
	_ = prop
	res2 := e.proposed(mt, Actor{Subject: "ＳＳＯ-d2", Roles: []string{RoleSecretary}}) // proposer subject, folded
	m(e.s.OpenVote(e.ctx, e.sec, res2.ID))
	m(e.s.CastVote(e.ctx, e.d2, res2.ID, VoteInput{Choice: "for"})) // sso-d2 is the same person as "ＳＳＯ-d2"
	m(e.s.CastVote(e.ctx, e.d1, res2.ID, VoteInput{Choice: "abstain"}))
	if v := m(e.s.CloseVote(e.ctx, e.sec, res2.ID)); v.Status != ResFailed || v.Result.Reason != "proposer_sole_approver" {
		t.Errorf("folded subject: %+v", v)
	}
	// with one other person for, it passes
	res3 := e.proposed(mt, e.d2)
	m(e.s.OpenVote(e.ctx, e.sec, res3.ID))
	m(e.s.CastVote(e.ctx, e.d2, res3.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d3, res3.ID, VoteInput{Choice: "for"}))
	if v := m(e.s.CloseVote(e.ctx, e.sec, res3.ID)); v.Status != ResPassed {
		t.Errorf("%+v", v)
	}
	// only the proposer amends, proposes, or edits: another director cannot
	res4 := m(func() (string, error) {
		id, _, err := e.s.CreateResolution(e.ctx, e.d2, e.body.ID, ResolutionInput{MeetingID: mt.ID, Title: "t", Text: "x"}, "", nil)
		return id, err
	}())
	if _, err := e.s.AmendResolution(e.ctx, e.d3, res4, AmendInput{Title: "t", Text: "changed"}); !isErr(err, ErrForbidden) {
		t.Errorf("another director amending: %v", err)
	}
	if _, err := e.s.ProposeResolution(e.ctx, e.sec, res4); !isErr(err, ErrForbidden) {
		t.Errorf("the secretary proposing a director's draft: %v", err)
	}
}

func TestWrittenConsent(t *testing.T) {
	e := newEnv(t)
	body := m(e.s.CreateBody(e.ctx, e.sec, BodyInput{Name: "Small board", Kind: "board"}))
	e.body = body
	for _, p := range []string{pD1, pD2, pD3} {
		m(e.s.AddMember(e.ctx, e.sec, body.ID, MemberInput{PersonID: p, Role: map[bool]string{true: "chair", false: "member"}[p == pD1], StartDate: "2025-01-01"}))
	}
	draft := func(by Actor, deadline string) string {
		id, _, err := e.s.CreateResolution(e.ctx, by, body.ID, ResolutionInput{Kind: "consent", Title: "Approve the lease", Text: "RESOLVED to sign the lease.", ConsentDeadline: deadline}, "", nil)
		e.must(err)
		m(e.s.ProposeResolution(e.ctx, by, id))
		m(e.s.OpenVote(e.ctx, by, id))
		return id
	}
	// every eligible member signs: it passes by itself, with no close step
	id := draft(e.sec, "2026-03-20")
	m(e.s.CastVote(e.ctx, e.d1, id, VoteInput{Choice: "for", SignatureRef: "sig:env-1"}))
	m(e.s.CastVote(e.ctx, e.d2, id, VoteInput{Choice: "for", SignatureRef: "sig:env-2"}))
	if v := m(e.s.GetResolution(e.ctx, e.sec, id)); v.Status != ResVoting {
		t.Fatalf("%+v", v)
	}
	if _, err := e.s.CastVote(e.ctx, e.d3, id, VoteInput{Choice: "for"}); err == nil {
		t.Error("a signature without a signature reference was accepted")
	}
	if _, err := e.s.CastVote(e.ctx, e.d3, id, VoteInput{Choice: "abstain", SignatureRef: "sig:x"}); err == nil {
		t.Error("a consent cannot abstain")
	}
	v := m(e.s.CastVote(e.ctx, e.d3, id, VoteInput{Choice: "for", SignatureRef: "sig:env-3"}))
	if v.Status != ResPassed || v.Result.Reason != "all_signed" || v.Result.For != 3 {
		t.Fatalf("%+v", v)
	}
	if votes := m(e.s.ListVotes(e.ctx, e.sec, id)); len(votes) != 3 || votes[0].Method != "consent" || votes[0].SignatureRef == "" {
		t.Errorf("%+v", votes)
	}
	// one decline fails it at once
	id2 := draft(e.sec, "2026-03-20")
	m(e.s.CastVote(e.ctx, e.d1, id2, VoteInput{Choice: "for", SignatureRef: "sig:a"}))
	if v := m(e.s.CastVote(e.ctx, e.d2, id2, VoteInput{Choice: "against", SignatureRef: "sig:b"})); v.Status != ResFailed || v.Result.Reason != "declined" {
		t.Errorf("%+v", v)
	}
	// a deadline that passes without every signature fails it (the sweep closes it)
	id3 := draft(e.sec, "2026-03-12")
	m(e.s.CastVote(e.ctx, e.d1, id3, VoteInput{Choice: "for", SignatureRef: "sig:a"}))
	if _, err := e.s.CloseVote(e.ctx, e.sec, id3); !isErr(err, ErrConflict) {
		t.Errorf("closing a consent before its deadline: %v", err)
	}
	e.advance("2026-03-13")
	n := m(e.s.ApplyDue(e.ctx))
	if n != 1 {
		t.Errorf("ApplyDue = %d", n)
	}
	if v := m(e.s.GetResolution(e.ctx, e.sec, id3)); v.Status != ResFailed || v.Result.Reason != "deadline" || v.Result.For != 1 {
		t.Errorf("%+v", v)
	}
	// the secretary proposes a consent the only eligible signer also proposed: still not enough
	solo := m(e.s.CreateBody(e.ctx, e.sec, BodyInput{Name: "One-director board", Kind: "board"}))
	m(e.s.AddMember(e.ctx, e.sec, solo.ID, MemberInput{PersonID: pD4, Role: "chair", StartDate: "2025-01-01"}))
	sid, _, err := e.s.CreateResolution(e.ctx, e.d4, solo.ID, ResolutionInput{Kind: "consent", Title: "t", Text: "x", ConsentDeadline: "2026-03-30"}, "", nil)
	e.must(err)
	m(e.s.ProposeResolution(e.ctx, e.d4, sid))
	m(e.s.OpenVote(e.ctx, e.d4, sid))
	if v := m(e.s.CastVote(e.ctx, e.d4, sid, VoteInput{Choice: "for", SignatureRef: "sig:solo"})); v.Status != ResFailed || v.Result.Reason != "proposer_sole_approver" {
		t.Errorf("%+v", v)
	}
	// a consent cannot be a secret ballot
	if _, _, err := e.s.CreateResolution(e.ctx, e.sec, body.ID, ResolutionInput{Kind: "consent", SecretBallot: true, Title: "t", Text: "x", ConsentDeadline: "2026-03-30"}, "", nil); err == nil {
		t.Error("a signed consent as a secret ballot was accepted")
	}
}

func TestSecretBallotCannotBeLinkedToAPerson(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3, pD4)
	id, _, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, ResolutionInput{MeetingID: mt.ID, SecretBallot: true, Title: "Remove the auditor", Text: "RESOLVED to remove the auditor."}, "", nil)
	e.must(err)
	m(e.s.ProposeResolution(e.ctx, e.sec, id))
	m(e.s.OpenVote(e.ctx, e.sec, id))
	m(e.s.CastVote(e.ctx, e.d3, id, VoteInput{Choice: "against"})) // cast out of person order on purpose
	m(e.s.CastVote(e.ctx, e.d1, id, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, id, VoteInput{Choice: "for"}))
	if _, err := e.s.CastVote(e.ctx, e.d3, id, VoteInput{Choice: "for"}); !isErr(err, ErrConflict) {
		t.Errorf("a second secret ballot: %v", err)
	}
	// while voting is open the ballots are only staged; nothing is in the final tables and no per-vote audit row exists
	if e.count(`SELECT COUNT(*) FROM votes WHERE resolution_id = ?`, id) != 0 || e.count(`SELECT COUNT(*) FROM ballots WHERE resolution_id = ?`, id) != 0 ||
		e.count(`SELECT COUNT(*) FROM ballot_staging WHERE resolution_id = ?`, id) != 3 || e.count(`SELECT COUNT(*) FROM receipt_staging WHERE resolution_id = ?`, id) != 3 {
		t.Error("a secret ballot is staged while voting is open")
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE action IN ('vote', 'voted') AND resource = ?`, "resolution:"+id) != 0 {
		t.Error("a per-vote audit row exists for a secret ballot")
	}
	if v := m(e.s.GetResolution(e.ctx, e.d1, id)); !v.Voted {
		t.Error("a voter cannot see that their ballot was received")
	}
	// nobody reads per-voter detail of a secret ballot, before or after the close
	for _, a := range []Actor{e.sec, e.d1, e.aud} {
		if _, err := e.s.ListVotes(e.ctx, a, id); err == nil {
			t.Errorf("%s read the votes of a secret ballot", a.Subject)
		}
	}
	// two ballots for: it passes; the tally is computed from the sealed ballots
	v := m(e.s.CloseVote(e.ctx, e.sec, id))
	if v.Status != ResPassed || v.Result.For != 2 || v.Result.Against != 1 {
		t.Errorf("%+v", v)
	}
	// at close the ballots and receipts are written shuffled and the staging is gone; the audit trail holds one
	// "voted" marker per voter, sorted by person id, with no per-vote order
	if e.count(`SELECT COUNT(*) FROM ballots WHERE resolution_id = ?`, id) != 3 || e.count(`SELECT COUNT(*) FROM vote_receipts WHERE resolution_id = ?`, id) != 3 ||
		e.count(`SELECT COUNT(*) FROM ballot_staging`) != 0 || e.count(`SELECT COUNT(*) FROM receipt_staging`) != 0 {
		t.Error("staging was not moved to the final tables")
	}
	rows, err2 := e.s.db.Query(`SELECT actor FROM audit_log WHERE action = 'voted' AND resource = ? ORDER BY seq`, "resolution:"+id)
	e.must(err2)
	var voted []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		voted = append(voted, a)
	}
	_ = rows.Close()
	if strings.Join(voted, ",") != pD1+","+pD2+","+pD3 {
		t.Errorf("voted markers: %v (want one per voter, sorted by person id, regardless of the order they voted in)", voted)
	}
	for _, a := range []Actor{e.sec, e.d1, e.aud} {
		if _, err := e.s.ListVotes(e.ctx, a, id); err == nil {
			t.Errorf("%s read the votes of a closed secret ballot", a.Subject)
		}
	}
	// a single "for" ballot cannot pass: the proposer might be the only one (conservative rule)
	id2, _, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, ResolutionInput{MeetingID: mt.ID, SecretBallot: true, Title: "t", Text: "x"}, "", nil)
	e.must(err)
	m(e.s.ProposeResolution(e.ctx, e.sec, id2))
	m(e.s.OpenVote(e.ctx, e.sec, id2))
	m(e.s.CastVote(e.ctx, e.d1, id2, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, id2, VoteInput{Choice: "abstain"}))
	if v := m(e.s.CloseVote(e.ctx, e.sec, id2)); v.Status != ResFailed || v.Result.Reason != "proposer_sole_approver" {
		t.Errorf("%+v", v)
	}
}

func TestAmendmentsCreateVersions(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	id, _, err := e.s.CreateResolution(e.ctx, e.d2, e.body.ID, ResolutionInput{MeetingID: mt.ID, Title: "Budget", Text: "version one"}, "", nil)
	e.must(err)
	v := m(e.s.AmendResolution(e.ctx, e.d2, id, AmendInput{Title: "Budget (revised)", Text: "version two"}))
	if v.Version != 2 || v.Text != "version two" {
		t.Errorf("%+v", v)
	}
	m(e.s.ProposeResolution(e.ctx, e.d2, id))
	v = m(e.s.AmendResolution(e.ctx, e.d2, id, AmendInput{Title: "Budget (final)", Text: "version three"}))
	if v.Version != 3 || e.count(`SELECT COUNT(*) FROM resolution_versions WHERE resolution_id = ?`, id) != 3 {
		t.Errorf("%+v", v)
	}
	m(e.s.OpenVote(e.ctx, e.sec, id))
	if _, err := e.s.AmendResolution(e.ctx, e.d2, id, AmendInput{Title: "x", Text: "too late"}); !isErr(err, ErrConflict) {
		t.Errorf("amending while voting: %v", err)
	}
	// the sealed text is bound to its row: swapping two versions' ciphertext is detected
	var a, b string
	e.must(e.s.db.QueryRow(`SELECT sens_text FROM resolution_versions WHERE resolution_id = ? AND version = 1`, id).Scan(&a))
	if _, err := e.s.db.Exec(`UPDATE resolution_versions SET sens_text = ? WHERE resolution_id = ? AND version = 3`, a, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetResolution(e.ctx, e.sec, id); err == nil {
		t.Error("a ciphertext moved to another version opened")
	}
	_ = b
}

func TestMinutesLifecycleAndSeparationOfDuties(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	if _, err := e.s.DraftMinutes(e.ctx, e.d1, mt.ID, "x"); !isErr(err, ErrForbidden) {
		t.Errorf("a director drafting: %v", err)
	}
	min := m(e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "Present: d1, d2, d3. The budget was approved."))
	if _, err := e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "another"); !isErr(err, ErrConflict) {
		t.Errorf("a second open draft: %v", err)
	}
	// drafts are readable by the secretary and the members who attended, not by absentees or observers
	if g := m(e.s.GetMinutes(e.ctx, e.d2, min.ID)); g.Content == "" {
		t.Error("an attending director cannot read the draft")
	}
	if _, err := e.s.GetMinutes(e.ctx, e.d5, min.ID); !isErr(err, ErrNotFound) {
		t.Errorf("an absent member reading the draft: %v", err)
	}
	// the meeting must be held first, and the secretary cannot approve
	if _, err := e.s.ApproveMinutes(e.ctx, e.d2, min.ID); !isErr(err, ErrConflict) {
		t.Errorf("approving before the meeting is marked held: %v", err)
	}
	m(e.s.SetMeetingStatus(e.ctx, e.sec, mt.ID, "held"))
	if _, err := e.s.ApproveMinutes(e.ctx, e.sec, min.ID); !isErr(err, ErrForbidden) {
		t.Errorf("the secretary approving: %v", err)
	}
	if _, err := e.s.ApproveMinutes(e.ctx, e.d5, min.ID); !isErr(err, ErrNotFound) {
		t.Errorf("an absent director approving: %v", err)
	}
	// an attending director who did not draft it approves
	got := m(e.s.ApproveMinutes(e.ctx, e.d2, min.ID))
	if got.Status != "approved" || got.Content == "" || got.ApprovedOn != "2026-03-10" {
		t.Fatalf("%+v", got)
	}
	// approved minutes are immutable: by the service and by the database
	if _, err := e.s.EditMinutes(e.ctx, e.sec, min.ID, "changed"); !isErr(err, ErrConflict) {
		t.Errorf("editing approved minutes: %v", err)
	}
	for _, q := range []string{
		`UPDATE minutes SET content_sha256 = 'x' WHERE id = '` + min.ID + `'`,
		`UPDATE minutes SET approved_by = 'someone' WHERE id = '` + min.ID + `'`,
		`UPDATE minutes SET status = 'draft' WHERE id = '` + min.ID + `'`,
		`DELETE FROM minutes WHERE id = '` + min.ID + `'`,
		`UPDATE attendance SET status = 'absent' WHERE meeting_id = '` + mt.ID + `'`,
	} {
		if _, err := e.s.db.Exec(q); err == nil {
			t.Errorf("the database accepted: %s", q)
		}
	}
	if _, err := e.s.RecordAttendance(e.ctx, e.sec, mt.ID, pD4, "present"); err == nil {
		t.Error("attendance changed after the minutes were approved")
	}
	// the observer reads the approved minutes
	if g := m(e.s.GetMinutes(e.ctx, e.obs, min.ID)); g.Content == "" {
		t.Error("observer cannot read approved minutes")
	}
	// a correction is a new version, approved again; the earlier approved version stays on record
	v2 := m(e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "Present: d1, d2, d3. The budget was approved. Corrected: d3 joined remotely."))
	if v2.Version != 2 {
		t.Errorf("%+v", v2)
	}
	if _, err := e.s.GetMinutes(e.ctx, e.obs, v2.ID); !isErr(err, ErrNotFound) {
		t.Errorf("observer reading a draft correction: %v", err)
	}
	m(e.s.ApproveMinutes(e.ctx, e.d3, v2.ID))
	if g := m(e.s.GetMinutes(e.ctx, e.d1, min.ID)); g.Status != "superseded" || g.Content == "" {
		t.Errorf("%+v", g)
	}
	if l := m(e.s.ListMinutes(e.ctx, e.obs, mt.ID)); len(l) != 2 {
		t.Errorf("observer sees %d versions", len(l))
	}

	// separation of duties: nobody approves what they drafted, by person id or by a folded subject
	mt2 := e.meeting(pD1, pD2, pD3)
	m(e.s.SetMeetingStatus(e.ctx, e.sec, mt2.ID, "held"))
	dual := Actor{Subject: "Dual-Dee", Roles: []string{RoleSecretary}, PersonID: pD3}
	d3m := m(e.s.DraftMinutes(e.ctx, dual, mt2.ID, "drafted by someone who is also director d3"))
	if _, err := e.s.ApproveMinutes(e.ctx, e.d3, d3m.ID); !isErr(err, ErrSeparation) {
		t.Errorf("approving as the same person id: %v", err)
	}
	folded := Actor{Subject: "ＤＵＡＬ-dee", Roles: []string{RoleDirector}, PersonID: pD2}
	if _, err := e.s.ApproveMinutes(e.ctx, folded, d3m.ID); !isErr(err, ErrSeparation) {
		t.Errorf("approving as the same folded subject: %v", err)
	}
	if _, err := e.s.ApproveMinutes(e.ctx, e.d1, d3m.ID); err != nil {
		t.Errorf("approving as a different person: %v", err)
	}
}

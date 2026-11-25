package board

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadsFailClosedWhenAuditIsUnavailable(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	res := e.proposed(mt, e.sec)
	min := m(e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "secret minutes"))
	m(e.s.AddPackItem(e.ctx, e.sec, mt.ID, PackInput{DocRef: "doc:p", Title: "pack"}))
	le := m(e.s.CreateLegalEntity(e.ctx, e.sec, LegalEntityInput{Name: "Example Pty Ltd", RegNumber: "123 456 789"}))
	m(e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD1, Office: "director", EffectiveOn: "2026-01-01", Detail: map[string]string{"name": "Dee One"}}))

	e.s.audit = failingAuditor{}
	reads := map[string]func() (any, error){
		"body":        func() (any, error) { return e.s.GetBody(e.ctx, e.sec, e.body.ID) },
		"bodies":      func() (any, error) { return e.s.ListBodies(e.ctx, e.sec) },
		"meeting":     func() (any, error) { return e.s.GetMeeting(e.ctx, e.sec, mt.ID) },
		"resolution":  func() (any, error) { return e.s.GetResolution(e.ctx, e.sec, res.ID) },
		"minutes":     func() (any, error) { return e.s.GetMinutes(e.ctx, e.sec, min.ID) },
		"pack":        func() (any, error) { return e.s.GetPack(e.ctx, e.sec, mt.ID) },
		"identifiers": func() (any, error) { return e.s.GetIdentifiers(e.ctx, e.sec, le.ID) },
		"register":    func() (any, error) { return e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, "", "", true) },
		"csv":         func() (any, error) { return e.s.RegisterCSV(e.ctx, e.sec, le.ID, RegDirectors, "") },
		"resolutions": func() (any, error) { return e.s.ListResolutions(e.ctx, e.sec, e.body.ID) },
	}
	for name, f := range reads {
		v, err := f()
		if err == nil {
			t.Errorf("%s: the read succeeded although it could not be audited: %+v", name, v)
		}
	}
	// writes join the audit entry in one transaction: no entry, no change
	before := e.count(`SELECT COUNT(*) FROM bodies`)
	if _, err := e.s.CreateBody(e.ctx, e.sec, BodyInput{Name: "Audit committee", Kind: "committee"}); err == nil {
		t.Error("a write succeeded without an audit entry")
	}
	if e.count(`SELECT COUNT(*) FROM bodies`) != before {
		t.Error("the unaudited write was committed")
	}
	if _, err := e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "ceased", PartyRef: pD1, Office: "director", EffectiveOn: "2026-02-01"}); err == nil {
		t.Error("a register entry was written without an audit entry")
	}
	if e.count(`SELECT COUNT(*) FROM register_entries`) != 1 {
		t.Error("the unaudited register entry was committed")
	}
}

func TestAuditIsAppendOnlyAndVerifiable(t *testing.T) {
	e := newEnv(t).base()
	e.meeting(pD1)
	if _, err := e.s.db.Exec(`UPDATE audit_log SET actor = 'x'`); err == nil {
		t.Error("audit_log accepted an update")
	}
	if _, err := e.s.db.Exec(`DELETE FROM audit_log`); err == nil {
		t.Error("audit_log accepted a delete")
	}
	e.must(e.s.VerifyAudit(e.ctx))
	// the log records ids and field names, never text
	rows, _ := e.s.db.Query(`SELECT actor, action, resource, fields FROM audit_log`)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a, b, c, d string
		_ = rows.Scan(&a, &b, &c, &d)
		for _, x := range []string{a, b, c, d} {
			if strings.Contains(x, "Q1 board meeting") || strings.Contains(x, "Board of Directors") {
				t.Errorf("audit row holds content: %q", x)
			}
		}
	}
	// a refused access is audited (rate limited), and does not reveal why
	before := e.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied'`)
	_, _ = e.s.GetBody(e.ctx, e.emp, e.body.ID)
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied'`) != before+1 {
		t.Error("a denied read was not audited")
	}
	// and a flood of denials is capped
	for i := 0; i < 100; i++ {
		_, _ = e.s.GetBody(e.ctx, e.emp, e.body.ID)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied'`); n > before+denyAuditPerMinute+2 {
		t.Errorf("%d denial rows: the audit trail can be flooded", n)
	}
}

func TestSensitiveColumnsAreSealedAndBoundToTheirRow(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	e.proposed(mt, e.sec)
	min := m(e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "Confidential discussion of the acquisition of Contoso"))
	m(e.s.AddPackItem(e.ctx, e.sec, mt.ID, PackInput{DocRef: "doc:cfo", Title: "Contoso valuation memo", Note: "do not forward"}))
	le := m(e.s.CreateLegalEntity(e.ctx, e.sec, LegalEntityInput{Name: "Example Pty Ltd", RegNumber: "ACN 000 111 222", TaxID: "TAX-9999"}))
	m(e.s.RecordEntry(e.ctx, e.sec, le.ID, RegMembers, EntryInput{Kind: "allotted", PartyRef: "party-fran", ClassRef: "cls_000000000000000000000001", Quantity: 100, EffectiveOn: "2026-01-02", Detail: map[string]string{"name": "Fran Founder", "address": "1 Example Street"}}))
	m(e.s.AddContact(e.ctx, e.sec, le.ID, ContactInput{Kind: "registered_agent", Name: "Agent Smith", Address: "9 Agent Road", EffectiveFrom: "2026-01-01"}))
	m(e.s.s2checkpoint())
	raw, err := os.ReadFile(e.path)
	e.must(err)
	for _, secret := range []string{"RESOLVED that the FY27", "Approve the budget", "acquisition of Contoso", "Contoso valuation memo", "do not forward", "ACN 000 111 222", "TAX-9999", "Fran Founder", "1 Example Street", "Agent Smith", "9 Agent Road"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("plaintext %q is in the database file", secret)
		}
	}
	// a sealed value moved to another row does not open
	var other string
	e.must(e.s.db.QueryRow(`SELECT sens_title FROM pack_items`).Scan(&other))
	if _, err := e.s.db.Exec(`UPDATE minutes SET sens_content = ? WHERE id = ?`, other, min.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetMinutes(e.ctx, e.sec, min.ID); err == nil {
		t.Error("a ciphertext copied to another row and column opened")
	}
	// and a value cannot be tampered with undetected: replacing the ciphertext with another key's output fails
	other2, _ := (&Keyring{active: "x", keys: map[string][]byte{"x": bytes.Repeat([]byte{7}, 32)}}).Seal("forged", min.ID+":content")
	if _, err := e.s.db.Exec(`UPDATE minutes SET sens_content = ? WHERE id = ?`, other2, min.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetMinutes(e.ctx, e.sec, min.ID); err == nil {
		t.Error("a forged ciphertext opened")
	}
}

// s2checkpoint flushes the write-ahead log so the file on disk holds everything written.
func (s *Service) s2checkpoint() (int, error) {
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return 0, err
}

func TestKeyRotationAndCompareAndSwap(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	res := e.proposed(mt, e.sec)
	min := m(e.s.DraftMinutes(e.ctx, e.sec, mt.ID, "minutes under key one"))
	m(e.s.CreateLegalEntity(e.ctx, e.sec, LegalEntityInput{Name: "Example Pty Ltd", RegNumber: "RN-1"}))
	m(e.s.OpenVote(e.ctx, e.sec, res.ID))
	m(e.s.CastVote(e.ctx, e.d1, res.ID, VoteInput{Choice: "for"}))
	// a ballot too
	id2, _, _ := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, ResolutionInput{MeetingID: mt.ID, SecretBallot: true, Title: "t", Text: "x"}, "", nil)
	m(e.s.ProposeResolution(e.ctx, e.sec, id2))
	m(e.s.OpenVote(e.ctx, e.sec, id2))
	m(e.s.CastVote(e.ctx, e.d1, id2, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d2, id2, VoteInput{Choice: "for"}))

	two, err := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	e.must(err)
	e.s.keys = two
	// a concurrent write between the read and the compare-and-swap is not overwritten with stale data
	once := false
	e.s.rotateHook = func() {
		if once {
			return
		}
		once = true
		sealed, _ := two.Seal("minutes written during rotation", min.ID+":content")
		_, _ = e.s.db.Exec(`UPDATE minutes SET sens_content = ?, content_sha256 = ? WHERE id = ?`, sealed, contentHash(min.ID, "minutes written during rotation"), min.ID)
	}
	n, err := e.s.RotateKeys(e.ctx)
	e.must(err)
	if n < 5 {
		t.Errorf("re-sealed %d values", n)
	}
	if again := m(e.s.RotateKeys(e.ctx)); again != 0 {
		t.Errorf("a second rotation re-sealed %d values", again)
	}
	if g := m(e.s.GetMinutes(e.ctx, e.sec, min.ID)); g.Content != "minutes written during rotation" {
		t.Errorf("the concurrent write was lost: %q", g.Content)
	}
	// the old key is no longer needed
	only, _ := ParseKeyring("k2 " + hexKeyB + "\n")
	e.s.keys = only
	if g := m(e.s.GetResolution(e.ctx, e.sec, res.ID)); g.Text == "" {
		t.Error("resolution text does not open under the new key alone")
	}
	if v := m(e.s.CloseVote(e.ctx, e.sec, id2)); v.Result.For != 2 {
		t.Errorf("sealed ballots do not open under the new key alone: %+v", v)
	}
}

func TestIdempotencyAndReplayRereadsUnderCurrentRights(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1)
	in := ResolutionInput{MeetingID: mt.ID, Title: "Once", Text: "only once"}
	body, _ := json.Marshal(in)
	id1, replay, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, in, "key-1", body)
	e.must(err)
	id2, replay2, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, in, "key-1", body)
	e.must(err)
	if id1 != id2 || replay || !replay2 || e.count(`SELECT COUNT(*) FROM resolutions`) != 1 {
		t.Errorf("%s %s %v %v", id1, id2, replay, replay2)
	}
	other, _ := json.Marshal(ResolutionInput{MeetingID: mt.ID, Title: "Different", Text: "x"})
	if _, _, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, ResolutionInput{MeetingID: mt.ID, Title: "Different", Text: "x"}, "key-1", other); !isErr(err, ErrConflict) {
		t.Errorf("same key, other body: %v", err)
	}
	if _, _, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, in, strings.Repeat("k", 200), body); err == nil {
		t.Error("an over-long key was accepted")
	}
	// the key belongs to the actor: another actor with the same key creates their own
	id3, replay3, err := e.s.CreateResolution(e.ctx, e.d1, e.body.ID, in, "key-1", body)
	e.must(err)
	if id3 == id1 || replay3 {
		t.Error("an idempotency key leaked across actors")
	}
}

func TestExpiredDeadlinesFailRatherThanHang(t *testing.T) {
	e := newEnv(t).base()
	ctx, cancel := context.WithTimeout(e.ctx, time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := e.s.GetBody(ctx, e.sec, e.body.ID); err == nil {
		t.Error("a read with an expired deadline succeeded")
	}
	if _, err := e.s.CreateBody(ctx, e.sec, BodyInput{Name: "late", Kind: "committee"}); err == nil {
		t.Error("a write with an expired deadline succeeded")
	}
}

func TestErasureRetainsTheLegalRecordAndPseudonymisesTheRest(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	res := e.proposed(mt, e.sec)
	m(e.s.DeclareConflict(e.ctx, e.d4, res.ID, ConflictInput{Treatment: "disclosed", Nature: "holds shares in the supplier"}))
	m(e.s.OpenVote(e.ctx, e.sec, res.ID))
	m(e.s.CastVote(e.ctx, e.d2, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CastVote(e.ctx, e.d3, res.ID, VoteInput{Choice: "for"}))
	m(e.s.CloseVote(e.ctx, e.sec, res.ID))
	le := m(e.s.CreateLegalEntity(e.ctx, e.sec, LegalEntityInput{Name: "Example Pty Ltd"}))
	m(e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD4, Office: "director", EffectiveOn: "2025-01-01",
		Detail: map[string]string{"name": "Dee Four", "address": "4 Private Lane", "date_of_birth": "1980-02-03"}}))
	m(e.s.AddContact(e.ctx, e.sec, le.ID, ContactInput{Kind: "registered_agent", Name: "Dee Four", Address: "4 Private Lane", PersonRef: pD4, EffectiveFrom: "2025-01-01"}))

	if _, err := e.s.RequestErasure(e.ctx, e.sec, pD4); !isErr(err, ErrForbidden) {
		t.Errorf("secretary erasing: %v", err)
	}
	if _, err := e.s.RequestErasure(e.ctx, e.adm, pD4); !isErr(err, ErrConflict) {
		t.Errorf("erasing a current member: %v", err)
	}
	var memberID string
	e.must(e.s.db.QueryRow(`SELECT id FROM body_members WHERE person_id = ?`, pD4).Scan(&memberID))
	m(e.s.EndMember(e.ctx, e.sec, e.body.ID, memberID, "2026-03-01"))
	e.must(e.s.SetLegalHold(e.ctx, e.adm, pD4, true, "litigation"))
	if _, err := e.s.RequestErasure(e.ctx, e.adm, pD4); !isErr(err, ErrConflict) {
		t.Errorf("erasing under legal hold: %v", err)
	}
	e.must(e.s.SetLegalHold(e.ctx, e.adm, pD4, false, ""))
	er := m(e.s.RequestErasure(e.ctx, e.adm, pD4))
	if len(er.Retained) != 2 || er.Retained[0].Class != ClassResolutions || er.Retained[1].Class != ClassRegister {
		t.Errorf("%+v", er)
	}
	// the nature of the declared interest is gone; the conflict row (the record that one was declared) stays
	if n := e.count(`SELECT COUNT(*) FROM conflicts WHERE person_id = ? AND sens_nature IS NULL`, pD4); n != 1 {
		t.Error("the sealed nature of the interest was not purged")
	}
	// the register keeps the name the law requires; the address and date of birth are gone
	v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, "", "", true))
	if len(v.Entries) != 1 || v.Entries[0].Detail["name"] != "Dee Four" || v.Entries[0].Detail["address"] != "" || v.Entries[0].Detail["date_of_birth"] != "" {
		t.Errorf("%+v", v.Entries)
	}
	if c := m(e.s.ListContacts(e.ctx, e.sec, le.ID, "all")); c[0].Address != "" || c[0].Name != "" {
		t.Errorf("%+v", c)
	}
	// the votes of the others and the minutes are untouched; tombstones are listed for the E08 ledger
	if e.count(`SELECT COUNT(*) FROM votes`) != 2 {
		t.Error("votes were purged")
	}
	tomb := m(e.s.ListErasures(e.ctx, e.adm, 0))
	if len(tomb) != 2 || tomb[0].Subject != pD4 {
		t.Errorf("%+v", tomb)
	}
	// the hook is idempotent and replayable; legal-record classes refuse
	e.must(e.s.PurgeSubject(e.ctx, pD4, ClassPerson))
	e.must(e.s.PurgeSubject(e.ctx, pD4, ClassPrivate))
	if err := e.s.PurgeSubject(e.ctx, pD4, ClassMinutes); !isErr(err, ErrConflict) {
		t.Errorf("purging minutes per person: %v", err)
	}
	if err := e.s.PurgeSubject(e.ctx, "not-a-person", ClassPerson); err == nil {
		t.Error("a malformed subject was accepted")
	}
	e.must(e.s.VerifyAudit(e.ctx))
}

func TestRetentionSweepHonoursLegalHold(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	res := e.proposed(mt, e.sec)
	m(e.s.DeclareConflict(e.ctx, e.d2, res.ID, ConflictInput{Treatment: "disclosed", Nature: "interest A"}))
	m(e.s.DeclareConflict(e.ctx, e.d3, res.ID, ConflictInput{Treatment: "disclosed", Nature: "interest B"}))
	m(e.s.AddPackItem(e.ctx, e.sec, mt.ID, PackInput{DocRef: "doc:old", Title: "old pack"}))
	e.must(e.s.SetLegalHold(e.ctx, e.adm, pD3, true, "dispute"))
	e.advance("2034-01-01") // more than 7 years later
	rep := m(e.s.RunRetention(e.ctx))
	if rep.Purged[ClassPrivate] != 1 || rep.Skipped[ClassPrivate] != 1 || rep.Purged[ClassPacks] != 1 {
		t.Errorf("%+v", rep)
	}
	// minutes, resolutions and registers are never swept
	for _, c := range []string{ClassMinutes, ClassResolutions, ClassRegister, ClassAudit} {
		if !e.s.retention[c].Legal {
			t.Errorf("%s is not legal retention", c)
		}
	}
	if _, err := ParseConfig([]byte("keyring_file: /k\nretention:\n  board-minutes: 7y\n")); err == nil {
		t.Error("config accepted a purge period for minutes")
	}
	if _, err := ParseConfig([]byte("keyring_file: /k\nretention:\n  board-packs: 3y\n  entity-person: 5y\n")); err != nil {
		t.Error(err)
	}
}

func TestKeyringFileRules(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "keyring")
	e := os.WriteFile(good, []byte("k1 "+hexKeyA+"\n"), 0o600)
	if e != nil {
		t.Fatal(e)
	}
	if _, err := LoadKeyring(good); err != nil {
		t.Error(err)
	}
	loose := filepath.Join(dir, "loose")
	_ = os.WriteFile(loose, []byte("k1 "+hexKeyA+"\n"), 0o644)
	if _, err := LoadKeyring(loose); err == nil {
		t.Error("a world-readable keyring was accepted")
	}
	link := filepath.Join(dir, "link")
	_ = os.Symlink(good, link)
	if _, err := LoadKeyring(link); err == nil {
		t.Error("a symlinked keyring was accepted")
	}
}

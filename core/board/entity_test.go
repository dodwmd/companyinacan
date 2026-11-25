package board

import (
	"io"
	"strings"
	"testing"
	"time"
)

func (e *env) entity() *LegalEntity {
	e.t.Helper()
	return m(e.s.CreateLegalEntity(e.ctx, e.sec, LegalEntityInput{Name: "Example Pty Ltd", Jurisdiction: "AU", EntityType: "private company", IncorporatedOn: "2020-07-01", FiscalYearEnd: "06-30",
		RegNumber: "000 111 222", TaxID: "TAX-1", EquityEntityRef: "ent_0123456789abcdef01234567"}))
}

func TestEntityVisibilityAndSealedIdentifiers(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	// the secretary, officer, audit-viewer, a director (member of any body) and the admin see the entity
	for _, a := range []Actor{e.sec, e.off, e.aud, e.d1, e.adm} {
		if _, err := e.s.GetLegalEntity(e.ctx, a, le.ID); err != nil {
			t.Errorf("%s: %v", a.Subject, err)
		}
	}
	for _, a := range []Actor{e.emp, e.obs} {
		if _, err := e.s.GetLegalEntity(e.ctx, a, le.ID); !isErr(err, ErrNotFound) {
			t.Errorf("%s: %v", a.Subject, err)
		}
	}
	// a director who sits on no body sees nothing
	if _, err := e.s.GetLegalEntity(e.ctx, Actor{Subject: "sso-x", Roles: []string{RoleDirector}, PersonID: "pers_00000000000000000000d0ff"}, le.ID); !isErr(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
	// identifiers: secretary, audit-viewer, officer, director; never the admin
	for _, a := range []Actor{e.sec, e.aud, e.off, e.d1} {
		id, err := e.s.GetIdentifiers(e.ctx, a, le.ID)
		if err != nil || id.RegNumber != "000 111 222" || id.TaxID != "TAX-1" {
			t.Errorf("%s: %+v %v", a.Subject, id, err)
		}
	}
	if _, err := e.s.GetIdentifiers(e.ctx, e.adm, le.ID); !isErr(err, ErrForbidden) {
		t.Errorf("admin identifiers: %v", err)
	}
	// the admin may create the record but not write the identifiers
	if _, err := e.s.CreateLegalEntity(e.ctx, e.adm, LegalEntityInput{Name: "Admin made", RegNumber: "x"}); !isErr(err, ErrForbidden) {
		t.Errorf("%v", err)
	}
	if _, err := e.s.CreateLegalEntity(e.ctx, e.adm, LegalEntityInput{Name: "Admin made"}); err != nil {
		t.Error(err)
	}
	if _, err := e.s.CreateLegalEntity(e.ctx, e.d1, LegalEntityInput{Name: "Director made"}); !isErr(err, ErrForbidden) {
		t.Errorf("%v", err)
	}
	if _, err := e.s.CreateLegalEntity(e.ctx, e.sec, LegalEntityInput{Name: "x", FiscalYearEnd: "13-45"}); err == nil {
		t.Error("a bad fiscal year end was accepted")
	}
}

func TestDirectorsRegisterAsOfViewsAndHistory(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	rec := func(in EntryInput) *Entry { return m(e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, in)) }
	rec(EntryInput{Kind: "appointed", PartyRef: pD1, Office: "director", Title: "Chair", EffectiveOn: "2024-01-01", Detail: map[string]string{"name": "Dee One", "address": "1 Lane"}, ResolutionRef: "res_000000000000000000000001"})
	rec(EntryInput{Kind: "appointed", PartyRef: pD2, Office: "director", EffectiveOn: "2025-06-01", Detail: map[string]string{"name": "Dee Two"}})
	rec(EntryInput{Kind: "appointed", PartyRef: pD2, Office: "secretary", EffectiveOn: "2025-06-01", Detail: map[string]string{"name": "Dee Two"}})
	rec(EntryInput{Kind: "ceased", PartyRef: pD2, Office: "director", EffectiveOn: "2026-01-31"})
	state := func(asOf string) []string {
		v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, asOf, "", false))
		var out []string
		for _, l := range v.State {
			out = append(out, l.PartyRef[len(l.PartyRef)-4:]+":"+l.Office+":"+l.Name)
		}
		return out
	}
	if got := strings.Join(state("2024-06-01"), ","); got != "d001:director:Dee One" {
		t.Errorf("2024-06-01: %s", got)
	}
	if got := strings.Join(state("2025-12-31"), ","); got != "d001:director:Dee One,d002:director:Dee Two,d002:secretary:Dee Two" {
		t.Errorf("2025-12-31: %s", got)
	}
	if got := strings.Join(state("2026-02-01"), ","); got != "d001:director:Dee One,d002:secretary:Dee Two" {
		t.Errorf("2026-02-01: %s", got)
	}
	if got := state("2023-01-01"); len(got) != 0 {
		t.Errorf("before anyone: %v", got)
	}
	// rules: no double appointment, no ceasing what is not held
	if _, err := e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD1, Office: "director", EffectiveOn: "2026-02-01"}); !isErr(err, ErrConflict) {
		t.Errorf("appointing twice: %v", err)
	}
	if _, err := e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "ceased", PartyRef: pD3, Office: "director", EffectiveOn: "2026-02-01"}); !isErr(err, ErrConflict) {
		t.Errorf("ceasing an unheld office: %v", err)
	}
	// a backdated entry changes the as-of view but not what had been recorded by an earlier date
	e.advance("2026-03-20")
	rec(EntryInput{Kind: "appointed", PartyRef: pD3, Office: "director", EffectiveOn: "2026-02-15", Detail: map[string]string{"name": "Dee Three"}})
	if got := state("2026-03-01"); !strings.Contains(strings.Join(got, ","), "d003") {
		t.Errorf("the backdated entry is missing from the as-of view: %v", got)
	}
	known := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, "2026-03-01", "2026-03-10", false))
	for _, l := range known.State {
		if strings.HasSuffix(l.PartyRef, "d003") {
			t.Error("known_at 2026-03-10 shows an entry recorded on 2026-03-20")
		}
	}
	// corrections: a later entry that names the mistaken one replaces it in the views; the ledger keeps both
	wrong := rec(EntryInput{Kind: "appointed", PartyRef: pD4, Office: "officer", Title: "CFO", EffectiveOn: "2026-03-01", Detail: map[string]string{"name": "Dee Four"}})
	fixed := rec(EntryInput{Kind: "appointed", PartyRef: pD4, Office: "officer", Title: "COO", EffectiveOn: "2026-03-01", CorrectsID: wrong.ID, Detail: map[string]string{"name": "Dee Four"}})
	v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, "2026-03-20", "", true))
	var cfo, coo bool
	for _, l := range v.State {
		cfo = cfo || l.Title == "CFO"
		coo = coo || l.Title == "COO"
	}
	if cfo || !coo || len(v.Entries) != 7 {
		t.Errorf("cfo=%v coo=%v entries=%d", cfo, coo, len(v.Entries))
	}
	if _, err := e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD5, Office: "officer", EffectiveOn: "2026-03-01", CorrectsID: wrong.ID}); !isErr(err, ErrConflict) {
		t.Errorf("correcting twice: %v", err)
	}
	_ = fixed
	// append-only, in the database too
	for _, q := range []string{`UPDATE register_entries SET quantity = 5`, `UPDATE register_entries SET effective_on = '2000-01-01'`, `DELETE FROM register_entries`, `UPDATE register_entries SET party_ref = 'x'`} {
		if _, err := e.s.db.Exec(q); err == nil {
			t.Errorf("the database accepted: %s", q)
		}
	}
	// read access: officer and audit-viewer and director see it; the observer, the employee and the admin do not
	for _, a := range []Actor{e.off, e.aud, e.d1} {
		if _, err := e.s.ViewRegister(e.ctx, a, le.ID, RegDirectors, "", "", false); err != nil {
			t.Errorf("%s: %v", a.Subject, err)
		}
	}
	for _, a := range []Actor{e.obs, e.emp, e.adm} {
		if _, err := e.s.ViewRegister(e.ctx, a, le.ID, RegDirectors, "", "", false); !isErr(err, ErrNotFound) {
			t.Errorf("%s: %v", a.Subject, err)
		}
	}
	// personal details are for the secretary and the person themself; the officer sees the name only
	ov := m(e.s.ViewRegister(e.ctx, e.off, le.ID, RegDirectors, "", "", true))
	for _, en := range ov.Entries {
		if en.Detail["address"] != "" {
			t.Errorf("the officer saw an address: %+v", en)
		}
	}
	sv := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, "", "", true))
	if sv.Entries[0].Detail["address"] != "1 Lane" {
		t.Errorf("the secretary did not see the address: %+v", sv.Entries[0])
	}
	if _, err := e.s.ViewRegister(e.ctx, e.sec, le.ID, "unknown", "", "", false); !isErr(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestMembersRegisterNeverGoesNegative(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	const cls = "cls_000000000000000000000001"
	rec := func(in EntryInput) (*Entry, error) { return e.s.RecordEntry(e.ctx, e.sec, le.ID, RegMembers, in) }
	if _, err := rec(EntryInput{Kind: "allotted", PartyRef: "party-a", ClassRef: cls, Quantity: 1000, EffectiveOn: "2026-01-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec(EntryInput{Kind: "transferred", PartyRef: "party-a", ClassRef: cls, Quantity: -400, EffectiveOn: "2026-02-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec(EntryInput{Kind: "transferred", PartyRef: "party-b", ClassRef: cls, Quantity: 400, EffectiveOn: "2026-02-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec(EntryInput{Kind: "transferred", PartyRef: "party-a", ClassRef: cls, Quantity: -700, EffectiveOn: "2026-02-15"}); !isErr(err, ErrConflict) {
		t.Errorf("over-transfer: %v", err)
	}
	// a backdated transfer that would have left the holder negative on an earlier date is refused too
	if _, err := rec(EntryInput{Kind: "transferred", PartyRef: "party-a", ClassRef: cls, Quantity: -900, EffectiveOn: "2026-01-15"}); !isErr(err, ErrConflict) {
		t.Errorf("backdated over-transfer: %v", err)
	}
	if _, err := rec(EntryInput{Kind: "ceased", PartyRef: "party-a", EffectiveOn: "2026-03-01"}); !isErr(err, ErrConflict) {
		t.Errorf("ceasing with shares: %v", err)
	}
	if _, err := rec(EntryInput{Kind: "allotted", PartyRef: "party-a", ClassRef: cls, Quantity: 0, EffectiveOn: "2026-03-01"}); err == nil {
		t.Error("a zero allotment was accepted")
	}
	if _, err := rec(EntryInput{Kind: "allotted", PartyRef: "party-a", Quantity: 5, EffectiveOn: "2026-03-01"}); err == nil {
		t.Error("an allotment without a class was accepted")
	}
	v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegMembers, "2026-03-01", "", false))
	if len(v.State) != 2 || v.State[0].Quantity != 600 || v.State[1].Quantity != 400 {
		t.Errorf("%+v", v.State)
	}
	if v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegMembers, "2026-01-31", "", false)); len(v.State) != 1 || v.State[0].Quantity != 1000 {
		t.Errorf("as of 2026-01-31: %+v", v.State)
	}
	// dates in the future are not "what happened"
	if _, err := rec(EntryInput{Kind: "allotted", PartyRef: "party-c", ClassRef: cls, Quantity: 1, EffectiveOn: "2027-01-01"}); err == nil {
		t.Error("a future entry was accepted")
	}
}

func TestChargesRegister(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	rec := func(in EntryInput) error { _, err := e.s.RecordEntry(e.ctx, e.sec, le.ID, RegCharges, in); return err }
	e.must(rec(EntryInput{Kind: "created", PartyRef: "chg-001", Title: "Fixed charge", EffectiveOn: "2026-01-01", Detail: map[string]string{"terms": "over all assets", "name": "Example Bank"}}))
	if err := rec(EntryInput{Kind: "created", PartyRef: "chg-001", EffectiveOn: "2026-01-02"}); !isErr(err, ErrConflict) {
		t.Errorf("registering a charge twice: %v", err)
	}
	e.must(rec(EntryInput{Kind: "satisfied", PartyRef: "chg-001", EffectiveOn: "2026-02-01"}))
	if err := rec(EntryInput{Kind: "satisfied", PartyRef: "chg-001", EffectiveOn: "2026-02-02"}); !isErr(err, ErrConflict) {
		t.Errorf("satisfying twice: %v", err)
	}
	if v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegCharges, "2026-01-15", "", false)); len(v.State) != 1 {
		t.Errorf("%+v", v.State)
	}
	if v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegCharges, "2026-03-01", "", false)); len(v.State) != 0 {
		t.Errorf("%+v", v.State)
	}
}

func TestAttestationSeparationOfDuties(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	// d2's own appointment, recorded by the secretary
	en := m(e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD2, Office: "director", EffectiveOn: "2026-01-01", Detail: map[string]string{"name": "Dee Two"}}))
	if _, err := e.s.AttestEntry(e.ctx, e.d2, en.ID); !isErr(err, ErrSeparation) {
		t.Errorf("attesting an entry that names you: %v", err)
	}
	if _, err := e.s.AttestEntry(e.ctx, e.sec, en.ID); !isErr(err, ErrForbidden) {
		t.Errorf("the secretary attesting: %v", err)
	}
	// a person who is both secretary and officer cannot attest what they recorded
	dual := Actor{Subject: "Sec-Sam", Roles: []string{RoleSecretary, RoleOfficer}, PersonID: pSe}
	en2 := m(e.s.RecordEntry(e.ctx, dual, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD3, Office: "director", EffectiveOn: "2026-01-02"}))
	if _, err := e.s.AttestEntry(e.ctx, Actor{Subject: "SEC-SAM", Roles: []string{RoleOfficer}, PersonID: pSe}, en2.ID); !isErr(err, ErrSeparation) {
		t.Errorf("attesting what you recorded: %v", err)
	}
	if _, err := e.s.AttestEntry(e.ctx, e.d1, en.ID); err != nil {
		t.Errorf("a director who is neither the subject nor the recorder: %v", err)
	}
	if _, err := e.s.AttestEntry(e.ctx, e.off, en.ID); !isErr(err, ErrConflict) {
		t.Errorf("attesting twice: %v", err)
	}
	if v := m(e.s.ViewRegister(e.ctx, e.sec, le.ID, RegDirectors, "", "", false)); !v.State[0].Attested {
		t.Errorf("%+v", v.State)
	}
	if _, err := e.s.AttestEntry(e.ctx, e.obs, en.ID); !isErr(err, ErrNotFound) {
		t.Errorf("observer attesting: %v", err)
	}
	// the attestation is write-once in the database
	if _, err := e.s.db.Exec(`UPDATE register_entries SET attested_by = 'someone-else' WHERE id = ?`, en.ID); err == nil {
		t.Error("an attestation was rewritten")
	}
}

func TestContactsAreEffectiveDated(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	m(e.s.AddContact(e.ctx, e.sec, le.ID, ContactInput{Kind: "registered_office", Address: "1 Old Street", EffectiveFrom: "2020-07-01"}))
	m(e.s.AddContact(e.ctx, e.sec, le.ID, ContactInput{Kind: "registered_office", Address: "2 New Street", EffectiveFrom: "2026-01-01"}))
	if _, err := e.s.AddContact(e.ctx, e.sec, le.ID, ContactInput{Kind: "registered_office", Address: "backwards", EffectiveFrom: "2025-01-01"}); !isErr(err, ErrConflict) {
		t.Errorf("%v", err)
	}
	at := func(d string) string {
		l := m(e.s.ListContacts(e.ctx, e.sec, le.ID, d))
		if len(l) != 1 {
			return "?"
		}
		return l[0].Address
	}
	if at("2025-06-01") != "1 Old Street" || at("2026-02-01") != "2 New Street" {
		t.Errorf("%s %s", at("2025-06-01"), at("2026-02-01"))
	}
	if l := m(e.s.ListContacts(e.ctx, e.sec, le.ID, "all")); len(l) != 2 || l[0].EffectiveTo != "2025-12-31" {
		t.Errorf("%+v", l)
	}
	if _, err := e.s.db.Exec(`UPDATE entity_contacts SET effective_from = '2000-01-01'`); err == nil {
		t.Error("contact history was rewritten")
	}
	// a person-linked contact is private: the officer sees that it exists but not the details
	m(e.s.AddContact(e.ctx, e.sec, le.ID, ContactInput{Kind: "registered_agent", Name: "Agent A", Address: "3 Agent Street", PersonRef: pD2, EffectiveFrom: "2026-01-01"}))
	for _, c := range m(e.s.ListContacts(e.ctx, e.off, le.ID, "")) {
		if c.Kind == "registered_agent" && (c.Address != "" || !c.Withheld) {
			t.Errorf("%+v", c)
		}
	}
	for _, c := range m(e.s.ListContacts(e.ctx, e.d2, le.ID, "")) { // the agent themself
		if c.Kind == "registered_agent" && c.Address == "" {
			t.Errorf("the person cannot see their own contact: %+v", c)
		}
	}
}

func TestFilingsCalendarAndReminders(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	ob := m(e.s.CreateObligation(e.ctx, e.sec, le.ID, ObligationInput{Name: "Annual return", Authority: "Registry", FirstDueOn: "2026-03-31", Recurrence: "quarterly", LeadDays: []int{30, 7, 0}}))
	if ob.ID == "" {
		t.Fatal("no obligation")
	}
	// 21 days before: the 30-day point has been reached: one reminder
	e.advance("2026-03-10")
	if n := m(e.s.ApplyDue(e.ctx)); n != 1 {
		t.Errorf("ApplyDue = %d", n)
	}
	if n := m(e.s.ApplyDue(e.ctx)); n != 0 {
		t.Errorf("a second sweep announced %d again", n)
	}
	e.advance("2026-03-25")
	if n := m(e.s.ApplyDue(e.ctx)); n != 1 { // the 7-day point
		t.Errorf("ApplyDue = %d", n)
	}
	e.advance("2026-04-05") // service was down over the due date: one announcement of the day point, not three
	if n := m(e.s.ApplyDue(e.ctx)); n != 1 {
		t.Errorf("ApplyDue = %d", n)
	}
	var due []Event
	for _, ev := range m(e.s.AllEvents(e.ctx)) {
		if ev.Type == EventFilingDue {
			due = append(due, ev)
		}
	}
	if len(due) != 3 || due[0].Source != "entity" {
		t.Fatalf("%d events %+v", len(due), due)
	}
	for _, ev := range due {
		for k := range ev.Data {
			switch k {
			case "filing_id", "obligation_id", "entity_id", "due_on", "days_before":
			default:
				t.Errorf("event data key %q", k)
			}
		}
	}
	if due[0].Data["days_before"] != float64(30) && due[0].Data["days_before"] != 30 {
		t.Errorf("%v", due[0].Data)
	}
	fs := m(e.s.ListFilings(e.ctx, e.sec, le.ID, "pending"))
	if len(fs) != 1 || !fs[0].Overdue || fs[0].DueOn != "2026-03-31" {
		t.Fatalf("%+v", fs)
	}
	// filing records the receipt and creates the next quarter
	if _, err := e.s.MarkFiling(e.ctx, e.emp, fs[0].ID, FilingResult{Status: "filed"}); !isErr(err, ErrNotFound) {
		t.Errorf("employee: %v", err)
	}
	if _, err := e.s.MarkFiling(e.ctx, e.d1, fs[0].ID, FilingResult{Status: "filed"}); !isErr(err, ErrForbidden) {
		t.Errorf("director: %v", err)
	}
	if _, err := e.s.MarkFiling(e.ctx, e.off, fs[0].ID, FilingResult{Status: "filed", FiledOn: "2026-04-04", DocRef: "doc:receipt-q1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.MarkFiling(e.ctx, e.sec, fs[0].ID, FilingResult{Status: "waived"}); !isErr(err, ErrConflict) {
		t.Errorf("re-marking: %v", err)
	}
	all := m(e.s.ListFilings(e.ctx, e.sec, le.ID, ""))
	if len(all) != 2 || all[1].DueOn != "2026-06-30" || all[1].Status != "pending" {
		t.Errorf("%+v", all)
	}
	if _, err := e.s.db.Exec(`UPDATE filings SET status = 'pending' WHERE status = 'filed'`); err == nil {
		t.Error("a recorded filing was reopened")
	}
	// the observer and the employee do not see the calendar
	if _, err := e.s.ListFilings(e.ctx, e.obs, le.ID, ""); !isErr(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestMonthEndClamp(t *testing.T) {
	d := func(s string) time.Time { return m(time.Parse(dateLayout, s)) }
	cases := []struct {
		anchor string
		months int
		want   string
	}{
		{"2026-01-31", 1, "2026-02-28"}, {"2026-01-31", 2, "2026-03-31"}, {"2024-01-31", 1, "2024-02-29"}, {"2026-11-30", 3, "2027-02-28"},
		{"2026-05-15", 12, "2027-05-15"}, {"2026-12-31", 2, "2027-02-28"}, {"2026-01-31", 13, "2027-02-28"},
	}
	for _, c := range cases {
		if got := addMonthsClamped(d(c.anchor), c.months).Format(dateLayout); got != c.want {
			t.Errorf("%s + %d months = %s, want %s", c.anchor, c.months, got, c.want)
		}
	}
}

func TestCSVExportsDefuseFormulas(t *testing.T) {
	e := newEnv(t).base()
	mt := e.meeting(pD1, pD2, pD3)
	id, _, err := e.s.CreateResolution(e.ctx, e.sec, e.body.ID, ResolutionInput{MeetingID: mt.ID, Title: `=HYPERLINK("http://evil.example","x")`, Text: "x"}, "", nil)
	e.must(err)
	w := m(e.s.ResolutionsCSV(e.ctx, e.sec, e.body.ID))
	var sb strings.Builder
	e.must(w(&sb))
	if strings.Contains(sb.String(), "\n"+id+",meeting,draft,=") || !strings.Contains(sb.String(), `'=HYPERLINK`) {
		t.Errorf("formula not defused:\n%s", sb.String())
	}
	// a person who may not read the text gets no title in the export
	w = m(e.s.ResolutionsCSV(e.ctx, e.adm, e.body.ID))
	sb.Reset()
	e.must(w(&sb))
	if strings.Contains(sb.String(), "HYPERLINK") {
		t.Error("the admin's export holds a resolution title")
	}
	le := e.entity()
	m(e.s.RecordEntry(e.ctx, e.sec, le.ID, RegDirectors, EntryInput{Kind: "appointed", PartyRef: pD1, Office: "director", Title: "+cmd|' /C calc'!A0", EffectiveOn: "2026-01-01", Detail: map[string]string{"name": "@SUM(1+1)"}}))
	cw := m(e.s.RegisterCSV(e.ctx, e.sec, le.ID, RegDirectors, ""))
	sb.Reset()
	e.must(cw(&sb))
	out := sb.String()
	if !strings.Contains(out, "'@SUM(1+1)") || !strings.Contains(out, "'+cmd") {
		t.Errorf("register CSV not defused:\n%s", out)
	}
	if _, err := e.s.RegisterCSV(e.ctx, e.obs, le.ID, RegDirectors, ""); !isErr(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
	_ = io.Discard
}

func TestDocumentsAndShareClassLinksAreOpaqueRefs(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	d := m(e.s.AddDocument(e.ctx, e.sec, le.ID, DocumentInput{Kind: "constitution", DocRef: "doc:constitution-2020", Title: "Constitution"}))
	if _, err := e.s.AddDocument(e.ctx, e.sec, le.ID, DocumentInput{DocRef: "doc:constitution-2020", Title: "again"}); !isErr(err, ErrConflict) {
		t.Errorf("%v", err)
	}
	if _, err := e.s.AddDocument(e.ctx, e.sec, le.ID, DocumentInput{DocRef: "../../etc/passwd", Title: "x"}); err == nil {
		t.Error("a path-like ref was accepted")
	}
	if l := m(e.s.ListDocuments(e.ctx, e.sec, le.ID)); len(l) != 1 || l[0].ID != d.ID || l[0].Title != "Constitution" {
		t.Errorf("%+v", l)
	}
	m(e.s.LinkShareClass(e.ctx, e.sec, le.ID, "cls_0123456789abcdef01234567"))
	if _, err := e.s.LinkShareClass(e.ctx, e.sec, le.ID, "cls_0123456789abcdef01234567"); !isErr(err, ErrConflict) {
		t.Errorf("%v", err)
	}
	if l := m(e.s.ListShareClassLinks(e.ctx, e.off, le.ID)); len(l) != 1 {
		t.Errorf("%+v", l)
	}
}

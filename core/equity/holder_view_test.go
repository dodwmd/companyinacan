package equity

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// A draft cancelled before anybody approved it was never an offer: its holder does not see it.
func TestCancelledDraftsAreNotShownToHolders(t *testing.T) {
	e := newEnv(t).base()
	draft := e.draft(e.hr, e.eveStk.ID, q(100))
	m(e.s.CancelGrant(e.ctx, e.hr, draft, "typo"))
	pending := e.draft(e.hr, e.eveStk.ID, q(200))
	m(e.s.SubmitGrant(e.ctx, e.hr, pending))
	m(e.s.CancelGrant(e.ctx, e.hr, pending, "withdrawn"))
	approved := e.approved(e.eveStk.ID, q(300))
	m(e.s.CancelGrant(e.ctx, e.hr, approved, "left"))

	for _, id := range []string{draft, pending} {
		if _, err := e.s.GetGrant(e.ctx, e.eve, id); !isErr(err, ErrNotFound) {
			t.Errorf("GetGrant: %v", err)
		}
		if _, err := e.s.Vesting(e.ctx, e.eve, id, ""); !isErr(err, ErrNotFound) {
			t.Errorf("Vesting: %v", err)
		}
		if _, _, err := e.s.MyPosition(e.ctx, e.eve, id); !isErr(err, ErrNotFound) {
			t.Errorf("MyPosition: %v", err)
		}
		if _, _, err := e.s.RequestExercise(e.ctx, e.eve, id, q(1), "", nil); !isErr(err, ErrNotFound) {
			t.Errorf("RequestExercise: %v", err)
		}
		m(e.s.GetGrant(e.ctx, e.hr, id)) // staff still see it
	}
	p := m(e.s.MyGrants(e.ctx, e.eve, GrantFilter{}))
	if len(p.Items) != 1 || p.Items[0].ID != approved {
		t.Errorf("MyGrants = %+v", p.Items)
	}
	if pos := m(e.s.MyPositions(e.ctx, e.eve, 0)); len(pos.Items) != 1 || pos.Items[0].Grant.ID != approved {
		t.Errorf("MyPositions = %+v", pos.Items)
	}
}

// Exercise lists are paged, never silently cut short.
func TestExerciseListsArePaged(t *testing.T) {
	old := exercisePageSize
	exercisePageSize = 2
	t.Cleanup(func() { exercisePageSize = old })
	e := newEnv(t).base()
	grant := e.active(e.eveStk.ID, q(4800))
	e.advance("2027-02-01")
	var want []string
	for range 5 {
		id, _, err := e.s.RequestExercise(e.ctx, e.eve, grant, q(1), "", nil)
		e.must(err)
		want = append(want, id)
	}
	walk := func(page func(after int64) *ExercisePage) {
		t.Helper()
		var got []string
		var after int64
		for {
			p := page(after)
			for _, x := range p.Items {
				got = append(got, x.ID)
			}
			if p.Next == "" {
				break
			}
			after = m(strconv.ParseInt(p.Next, 10, 64))
		}
		if len(got) != len(want) {
			t.Fatalf("%d of %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order: %v", got)
			}
		}
	}
	walk(func(a int64) *ExercisePage {
		return m(e.s.ListExercises(e.ctx, e.fin, e.ent.ID, ExerciseFilter{After: a}))
	})
	walk(func(a int64) *ExercisePage { return m(e.s.MyExercises(e.ctx, e.eve, a)) })

	// A cursor survives a VACUUM that renumbers the implicit rowids. A gap makes VACUUM do so here.
	first := m(e.s.MyExercises(e.ctx, e.eve, 0))
	for _, stmt := range []string{`UPDATE exercises SET rowid = rowid + 1000`, `VACUUM`} {
		if _, err := e.s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	after := m(strconv.ParseInt(first.Next, 10, 64))
	if next := m(e.s.MyExercises(e.ctx, e.eve, after)); len(next.Items) != 2 || next.Items[0].ID != want[2] || next.Items[1].ID != want[3] {
		t.Fatalf("page after VACUUM: %+v", next.Items)
	}
	if all := m(e.s.GrantExercises(e.ctx, e.hr, grant)); len(all) != 5 {
		t.Fatalf("grant page: %d", len(all))
	}
	if _, err := e.s.GrantExercises(e.ctx, e.eve, grant); !isErr(err, ErrForbidden) {
		t.Fatalf("holder: %v", err)
	}

	u := &uiEnv{env: e, st: &StaticTokens{}}
	u.h = (&UI{Svc: e.s, Auth: MultiAuth{u.st}, Now: e.s.now}).Handler()
	has(t, u.login(t, e.fin).get("/company/entities/"+e.ent.ID+"/exercises", http.StatusOK), "/exercises?status=&amp;after=")
	has(t, u.login(t, e.eve).get("/me", http.StatusOK), "/me?ex_after=")
}

// The grants export is the current state, so neither its file name nor its audit entry claims a date.
func TestGrantsExportClaimsNoAsOfDate(t *testing.T) {
	u := newUIEnv(t)
	u.base()
	u.draft(u.hr, u.eveStk.ID, q(100))
	c := u.login(t, u.fin)
	path := "/company/entities/" + u.ent.ID + "/exports"
	for name, file := range map[string]string{"grants": "equity-grants.csv", "ledger": "equity-ledger-2026-01-01.csv"} {
		w := c.post(path, http.StatusOK, "name", name, "as_of", "2026-01-01")
		if got := w.Header().Get("Content-Disposition"); got != "attachment; filename="+file {
			t.Errorf("%s: %s", name, got)
		}
	}
	var buf bytes.Buffer
	u.must(u.s.ExportCSV(u.ctx, u.fin, u.ent.ID, "grants", "2026-01-01", &buf))
	if n := u.count(`SELECT COUNT(*) FROM audit_log WHERE resource = ?`, "export:grants:"+u.ent.ID+"@2026-01-01"); n != 0 {
		t.Errorf("the audit entry claims the as-of date")
	}
}

// The confirmer is shown the directory's name for the person when the directory gives one.
func TestVerifyPageNamesTheDirectoryPerson(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/people" && r.URL.Query().Get("sso_subject") == "user:ben":
			_, _ = w.Write([]byte(`{"items":[{"id":"` + personBen + `","sso_subject":"user:ben"}]}`))
		case r.URL.Path == "/v1/people":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			_, _ = w.Write([]byte(`{"id":"` + personCat + `","given_name":"Catherine","preferred_name":"Cat","family_name":"<Example>"}`))
		}
	}))
	defer srv.Close()
	dir := &HTTPDirectory{BaseURL: srv.URL, Token: "directory-token-0123456789abcdef"}
	if n, err := dir.PersonName(context.Background(), personCat); err != nil || n != "Cat <Example>" {
		t.Fatalf("%q %v", n, err)
	}
	u := newUIEnv(t, func(o *Options) { o.Directory = dir })
	boss := Actor{Subject: "board-x", Roles: []string{RoleBoard}}
	u.ent = m(u.s.CreateEntity(u.ctx, boss, EntityInput{Name: "Example Pty Ltd", Currency: "USD"}))
	ann := m(u.s.ResolveActor(u.ctx, &Principal{Subject: "user:ben", Roles: []string{RoleHR}}))
	ann.Subject = "user:ann" // the record is entered by somebody else
	st := m(u.s.CreateStakeholder(u.ctx, ann, u.ent.ID, StakeholderInput{Kind: "employee", PersonID: personCat, Identity: Identity{Name: "Cat Holder"}}))
	c := u.login(t, Actor{Subject: "user:ben", Roles: []string{RoleHR}})
	has(t, c.get("/company/stakeholders/"+st.ID, http.StatusOK), "<strong>Cat &lt;Example&gt;</strong>", `value="`+personCat+`"`)
}

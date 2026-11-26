package equity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

func TestEquityRolesFromPermissions(t *testing.T) {
	doc, ps := config.Load("../../modules/equity/module.yaml")
	if doc == nil {
		t.Fatalf("manifest: %v", ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	for manifestRole, want := range map[string]string{
		"employee": "employee", "manager": "employee", "hr": "employee,hr",
		"finance": "finance", "board": "board", "admin": "finance,admin",
	} {
		if got := strings.Join(rolesFromPermissions(m.Roles[manifestRole]), ","); got != want {
			t.Errorf("%s %v -> %q, want %q", manifestRole, m.Roles[manifestRole], got, want)
		}
	}
	// Only the board role holds equity.board, and nothing without it is the board.
	for name, perms := range m.Roles {
		if got := slices.Contains(perms, permBoard); got != (name == "board") {
			t.Errorf("role %s holds %s = %v", name, permBoard, got)
		}
	}
	// No subset of any role's permissions gains a role.
	all := m.Permissions
	pick := func(mask int) []string {
		var out []string
		for i, p := range all {
			if mask&(1<<i) != 0 {
				out = append(out, p)
			}
		}
		return out
	}
	for full := range 1 << len(all) {
		most := rolesFromPermissions(pick(full))
		if slices.Contains(most, RoleBoard) && !slices.Contains(pick(full), permBoard) {
			t.Fatalf("%v is the board", pick(full))
		}
		for sub := range 1 << len(all) {
			if sub&full != sub {
				continue
			}
			for _, r := range rolesFromPermissions(pick(sub)) {
				if !slices.Contains(most, r) {
					t.Fatalf("%v gains %q over its superset %v", pick(sub), r, pick(full))
				}
			}
		}
	}
}

func TestPersonalAccessTokensThroughCanCore(t *testing.T) {
	core := fakecore.New(t, "equity")
	auth := &CanAuth{In: &modclient.Introspector{API: core.Client(t)}}
	const tok = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	core.AddToken(tok, fakecore.Token{Subject: "user:fin", Permissions: []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read"}})
	core.AddToken(tok+"B", fakecore.Token{Subject: "user:board", Permissions: []string{"equity.read", "equity.approve", "equity.sensitive-read", "equity.admin"}})
	core.AddToken(tok+"F", fakecore.Token{Subject: "user:eve", Permissions: []string{"equity.self-read"}, Module: "jobs"})
	who := func(token string) (*Principal, error) {
		r := httptest.NewRequest(http.MethodGet, "/v1/entities", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return auth.Authenticate(r)
	}
	p, err := who(tok)
	if err != nil || p.Subject != "user:fin" || p.PersonID != "" || strings.Join(p.Roles, ",") != "finance" {
		t.Fatalf("%+v %v", p, err)
	}
	for name, token := range map[string]string{"board permissions": tok + "B", "another module's": tok + "F", "unknown": tok + "?"} {
		if p, err := who(token); err == nil {
			t.Errorf("%s: %+v", name, p)
		}
	}
	core.SetDown(true)
	core.AddToken(tok+"D", fakecore.Token{Subject: "user:new", Permissions: []string{"equity.self-read"}})
	if _, err := who(tok + "D"); err == nil {
		t.Fatal("accepted while can-core is down")
	}
}

// A can-core token says nothing about which person it is, so the actions whose separation of duties
// needs that are refused, whatever its roles; the same roles with a person, or the service account
// (not a can-core token), are not affected.
func TestCanCoreTokensWithoutAPersonAreRefusedPersonBoundActions(t *testing.T) {
	e := newEnv(t)
	roles := []string{RoleHR, RoleFinance, RoleAdmin}
	p, err := e.s.ResolveActor(e.ctx, &Principal{Subject: "user:alice", Roles: roles, CanCore: true})
	if err != nil {
		t.Fatal(err)
	}
	linked := p
	linked.PersonID = personBob
	service := Actor{Subject: "svc-can-core", Roles: roles}
	board, boardLinked := p, linked
	board.Roles, boardLinked.Roles = []string{RoleBoard}, []string{RoleBoard}
	if Decide(board, ActGrantApprove, Fact{}) || !Decide(boardLinked, ActGrantApprove, Fact{}) {
		t.Errorf("grant approval: the board without a person %v, with one %v", Decide(board, ActGrantApprove, Fact{}), Decide(boardLinked, ActGrantApprove, Fact{}))
	}
	for _, act := range []Action{ActGrantDraft, ActGrantAdminister, ActExerciseApprove, ActExercisePay, ActExerciseIssue} {
		if Decide(p, act, Fact{}) {
			t.Errorf("%s: allowed to a token without a person", act)
		}
		if !Decide(linked, act, Fact{}) || !Decide(service, act, Fact{}) {
			t.Errorf("%s: refused to a linked person or the service account", act)
		}
	}
	for _, act := range []Action{ActStructureManage, ActGrantList, ActCapTable} {
		if !Decide(p, act, Fact{}) {
			t.Errorf("%s: should stay allowed", act)
		}
	}
}

func TestACanCoreTokenCannotDraftAGrantThroughTheAPI(t *testing.T) {
	core := fakecore.New(t, "equity")
	const tok = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	core.AddToken(tok, fakecore.Token{Subject: "user:alice", Permissions: []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read", "equity.admin", "equity.self-read"}})
	e := newEnv(t)
	api := &API{Svc: e.s, Auth: &CanAuth{In: &modclient.Introspector{API: core.Client(t)}}}
	h := api.Handler()
	for _, path := range []string{"/v1/plans/plan_000000000000000000000000/grants", "/v1/grants/grant_000000000000000000000000/activate",
		"/v1/exercises/ex_000000000000000000000000/approve", "/v1/exercises/ex_000000000000000000000000/payment", "/v1/exercises/ex_000000000000000000000000/issue"} {
		body := `{}`
		if strings.HasSuffix(path, "/payment") {
			body = `{"paid_on":"2026-03-01","reference":"x"}`
		}
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d %.200s, want 403", path, w.Code, w.Body)
		}
	}
}

// With can-core credentials and the directory, the board approves a grant drafted by HR; the holder,
// the drafter, the administrator and the service account do not.
func TestCanCoreBoardApprovesWithSeparationOfDuties(t *testing.T) {
	e := newIDEnv(t)
	core := fakecore.New(t, "equity")
	auth := &CanAuth{In: &modclient.Introspector{API: core.Client(t)}}
	const personDee = "pers_00000000000000000000c0d1"
	e.dir.subjects["user:dee"] = personDee
	boardPerms := []string{permBoard, "equity.read", "equity.approve", "equity.sensitive-read", "equity.admin"}
	hrPerms := []string{"equity.read", "equity.write", "equity.sensitive-read", "equity.self-read"}
	adminPerms := []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read", "equity.admin"}
	actor := func(name, user string, perms []string) Actor {
		t.Helper()
		tok := "canpat_0123456789abcdefghijklmnopqrstuvwxyz" + name
		core.AddToken(tok, fakecore.Token{Subject: "user:" + user, Permissions: perms})
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		p, err := auth.Authenticate(r)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return e.actor(*p)
	}
	ann := actor("ann", "ann", hrPerms)                                  // drafts
	benBoard := actor("benboard", "ben", boardPerms)                     // approves
	catBoard := actor("catboard", "cat", boardPerms)                     // holds the grant
	annBoard := actor("annboard", "ann", append(hrPerms, boardPerms...)) // the drafter, also a director
	admin := actor("deeadmin", "dee", adminPerms)
	nobody := actor("stranger", "stranger", boardPerms) // no directory person

	boss := Actor{Subject: "board-x", Roles: []string{RoleBoard}}
	e.plan = m(e.s.CreatePlan(e.ctx, boss, e.ent.ID, PlanInput{Name: "2026 Plan", ClassID: e.class.ID, Authorised: q(1_000_000), AdoptedOn: "2026-01-01"}))
	stk := m(e.s.CreateStakeholder(e.ctx, ann, e.ent.ID, StakeholderInput{Kind: "employee", PersonID: personCat, Identity: Identity{Name: "Cat"}}))
	m(e.s.VerifyPerson(e.ctx, benBoard, stk.ID))
	id := e.draft(ann, stk.ID, q(100))
	m(e.s.SubmitGrant(e.ctx, ann, id))

	for name, c := range map[string]struct {
		a    Actor
		want error
	}{
		"the holder":                        {catBoard, ErrSeparation},
		"the drafter":                       {annBoard, ErrSeparation},
		"an administrator":                  {admin, ErrForbidden},
		"the service account":               {e.svc, ErrForbidden},
		"a director with no directory link": {nobody, ErrIdentityRequired},
	} {
		if _, err := e.s.ApproveGrant(e.ctx, c.a, id); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if g := m(e.s.ApproveGrant(e.ctx, benBoard, id)); g.Status != GrantApproved {
		t.Fatalf("status %s", g.Status)
	}
}

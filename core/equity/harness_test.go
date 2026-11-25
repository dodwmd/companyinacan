package equity

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const (
	hexKeyA = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	hexKeyB = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"

	personEve = "pers_0000000000000000000000e1"
	personMax = "pers_0000000000000000000000a2"
	personBob = "pers_0000000000000000000000b3"
)

// env is a fixture: a service with a controllable clock and one actor per role.
type env struct {
	t     *testing.T
	s     *Service
	path  string
	clock time.Time
	ctx   context.Context

	hr, hr2, board, board2, fin, fin2, admin Actor
	eve, max                                 Actor // employee (own record); max also holds the manager role

	ent     *Entity
	class   *ShareClass
	plan    *Plan
	eveStk  *Stakeholder
	founder *Stakeholder
}

type optFn func(*Options)

func newEnv(t *testing.T, opts ...optFn) *env {
	t.Helper()
	kr, err := ParseKeyring("k1 " + hexKeyA + "\n")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, clock: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC), ctx: context.Background()}
	e.path = filepath.Join(t.TempDir(), "equity.db")
	o := Options{Keyring: kr, Now: func() time.Time { return e.clock }}
	for _, f := range opts {
		f(&o)
	}
	e.s, err = Open(e.path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.s.Close() })
	e.hr = Actor{Subject: "hr-alice", Roles: []string{RoleHR}}
	e.hr2 = Actor{Subject: "hr-hana", Roles: []string{RoleHR}}
	e.board = Actor{Subject: "board-bob", Roles: []string{RoleBoard}, PersonID: personBob}
	e.board2 = Actor{Subject: "board-bea", Roles: []string{RoleBoard}}
	e.fin = Actor{Subject: "fin-frank", Roles: []string{RoleFinance}}
	e.fin2 = Actor{Subject: "fin-fay", Roles: []string{RoleFinance}}
	e.admin = Actor{Subject: "svc-admin", Roles: []string{RoleAdmin, RoleHR, RoleFinance}}
	e.eve = Actor{Subject: "sso-eve", Roles: []string{RoleEmployee}, PersonID: personEve}
	e.max = Actor{Subject: "sso-max", Roles: []string{RoleEmployee, RoleManager}, PersonID: personMax}
	return e
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// m unwraps a (value, error) pair; an error fails the test (the panic is reported by go test).
func m[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// base builds an entity with a common class (10M), a 1M plan, an employee stakeholder (eve) and a founder.
func (e *env) base() *env {
	e.ent = m(e.s.CreateEntity(e.ctx, e.board, EntityInput{Name: "Example Pty Ltd", Jurisdiction: "AU", Currency: "USD"}))
	e.class = m(e.s.CreateShareClass(e.ctx, e.board, e.ent.ID, ShareClassInput{Name: "Common", Kind: "common", ParMinor: 1, Currency: "USD", Authorised: q(10_000_000)}))
	e.plan = m(e.s.CreatePlan(e.ctx, e.board, e.ent.ID, PlanInput{Name: "2026 Plan", ClassID: e.class.ID, Authorised: q(1_000_000), AdoptedOn: "2026-01-01"}))
	e.eveStk = m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "employee", PersonID: personEve, Identity: Identity{Name: "Eve Example", Email: "eve@example.test"}}))
	e.founder = m(e.s.CreateStakeholder(e.ctx, e.fin, e.ent.ID, StakeholderInput{Kind: "founder", Identity: Identity{Name: "Fran Founder"}}))
	return e
}

func (e *env) standardInput(stk string, qty Qty) GrantInput {
	return GrantInput{StakeholderID: stk, Type: "NSO", Quantity: qty, StrikeMinor: 125, Currency: "USD", GrantDate: "2026-01-15", VestStart: "2026-01-15",
		Schedule: Schedule{Kind: "periodic", Months: 48, CliffMonths: 12, Interval: "monthly"}, ExpiryDate: "2036-01-15", PTEPDays: 90, Notes: "board minute 14"}
}

func (e *env) draft(by Actor, stk string, qty Qty) string {
	e.t.Helper()
	in := e.standardInput(stk, qty)
	body, _ := json.Marshal(in)
	id, _, err := e.s.CreateGrant(e.ctx, by, e.plan.ID, in, "", body)
	e.must(err)
	return id
}

// approved takes a grant to approved: drafted and submitted by hr, approved by the board.
func (e *env) approved(stk string, qty Qty) string {
	e.t.Helper()
	id := e.draft(e.hr, stk, qty)
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	m(e.s.ApproveGrant(e.ctx, e.board, id))
	return id
}

func (e *env) active(stk string, qty Qty) string {
	e.t.Helper()
	id := e.approved(stk, qty)
	m(e.s.ActivateGrant(e.ctx, e.hr, id, ""))
	return id
}

func (e *env) advance(date string) {
	e.t.Helper()
	d, err := time.Parse(dateLayout, date)
	e.must(err)
	e.clock = d.Add(9 * time.Hour)
}

func isErr(err error, target error) bool { return errors.Is(err, target) }

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	e.must(e.s.db.QueryRow(query, args...).Scan(&n))
	return n
}

// failingAuditor refuses every write: reads of sensitive data must then fail closed.
type failingAuditor struct{}

func (failingAuditor) Record(context.Context, AuditEvent) error {
	return errors.New("audit unavailable")
}
func (failingAuditor) RecordBatch(context.Context, []AuditEvent) error {
	return errors.New("audit unavailable")
}

// fakeEngine is an in-memory approval engine.
type fakeEngine struct {
	reqs    []ApprovalRequest
	outcome string
	decider string
}

func (f *fakeEngine) Request(_ context.Context, r ApprovalRequest) (string, error) {
	f.reqs = append(f.reqs, r)
	return "apr_" + r.Ref, nil
}

func (f *fakeEngine) Status(context.Context, string) (ApprovalStatus, error) {
	return ApprovalStatus{Outcome: f.outcome, DecidedBy: f.decider}, nil
}

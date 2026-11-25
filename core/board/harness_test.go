package board

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const (
	hexKeyA = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	hexKeyB = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"

	pD1 = "pers_00000000000000000000d001"
	pD2 = "pers_00000000000000000000d002"
	pD3 = "pers_00000000000000000000d003"
	pD4 = "pers_00000000000000000000d004"
	pD5 = "pers_00000000000000000000d005"
	pOf = "pers_00000000000000000000f001"
	pOb = "pers_00000000000000000000b001"
	pAu = "pers_00000000000000000000a001"
	pEm = "pers_00000000000000000000e001"
	pSe = "pers_00000000000000000000c001"
)

// env is a fixture: a service with a controllable clock and one actor per role.
type env struct {
	t     *testing.T
	s     *Service
	path  string
	clock time.Time
	ctx   context.Context

	sec, adm, d1, d2, d3, d4, d5, off, obs, aud, emp, integ Actor

	body *Body
}

type optFn func(*Options)

func newEnv(t *testing.T, opts ...optFn) *env {
	t.Helper()
	kr, err := ParseKeyring("k1 " + hexKeyA + "\n")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, clock: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC), ctx: context.Background()}
	e.path = filepath.Join(t.TempDir(), "board.db")
	o := Options{Keyring: kr, Now: func() time.Time { return e.clock }}
	for _, f := range opts {
		f(&o)
	}
	e.s, err = Open(e.path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.s.Close() })
	e.sec = Actor{Subject: "sec-sam", Roles: []string{RoleSecretary}, PersonID: pSe}
	e.adm = Actor{Subject: "svc-admin", Roles: []string{RoleAdmin}}
	e.d1 = Actor{Subject: "sso-d1", Roles: []string{RoleDirector}, PersonID: pD1}
	e.d2 = Actor{Subject: "sso-d2", Roles: []string{RoleDirector}, PersonID: pD2}
	e.d3 = Actor{Subject: "sso-d3", Roles: []string{RoleDirector}, PersonID: pD3}
	e.d4 = Actor{Subject: "sso-d4", Roles: []string{RoleDirector}, PersonID: pD4}
	e.d5 = Actor{Subject: "sso-d5", Roles: []string{RoleDirector}, PersonID: pD5}
	e.off = Actor{Subject: "sso-off", Roles: []string{RoleOfficer}, PersonID: pOf}
	e.obs = Actor{Subject: "sso-obs", Roles: []string{RoleObserver}, PersonID: pOb}
	e.aud = Actor{Subject: "sso-aud", Roles: []string{RoleAuditViewer}, PersonID: pAu}
	e.emp = Actor{Subject: "sso-emp", Roles: []string{RoleEmployee}, PersonID: pEm}
	e.integ = Actor{Subject: "svc-can-core", Roles: []string{RoleIntegration}}
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

func (e *env) advance(date string) {
	e.t.Helper()
	d, err := time.Parse(dateLayout, date)
	e.must(err)
	e.clock = d.Add(9 * time.Hour)
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	e.must(e.s.db.QueryRow(query, args...).Scan(&n))
	return n
}

func isErr(err error, target error) bool { return errors.Is(err, target) }

// base builds a board of five (d1 chair, d2..d5 members), an observer, with quorum "more than half"
// (1/2 rounded up: 3 of 5) and a simple-majority pass rule.
func (e *env) base() *env {
	e.body = m(e.s.CreateBody(e.ctx, e.sec, BodyInput{Name: "Board of Directors", Kind: "board", NoticeDays: 7}))
	for i, p := range []struct{ id, role string }{{pD1, "chair"}, {pD2, "member"}, {pD3, "member"}, {pD4, "member"}, {pD5, "member"}, {pOb, "observer"}} {
		m(e.s.AddMember(e.ctx, e.sec, e.body.ID, MemberInput{PersonID: p.id, Role: p.role, StartDate: "2025-01-01"}))
		_ = i
	}
	return e
}

// meeting schedules and "holds" a meeting today with the given people present.
func (e *env) meeting(present ...string) *Meeting {
	e.t.Helper()
	mt := m(e.s.ScheduleMeeting(e.ctx, e.sec, e.body.ID, MeetingInput{Title: "Q1 board meeting", StartsOn: "2026-03-10", NoticeGivenOn: "2026-02-20", Mode: "hybrid",
		Invitees: []Invitee{{PersonID: pOf, Capacity: "officer"}}}))
	for _, p := range present {
		m(e.s.RecordAttendance(e.ctx, e.sec, mt.ID, p, "present"))
	}
	return mt
}

// proposed drafts and proposes a resolution at a meeting (proposer: the secretary unless by is given).
func (e *env) proposed(mt *Meeting, by Actor, subjects ...string) *Resolution {
	e.t.Helper()
	id, _, err := e.s.CreateResolution(e.ctx, by, e.body.ID, ResolutionInput{MeetingID: mt.ID, Title: "Approve the budget", Text: "RESOLVED that the FY27 budget is approved.", Subjects: subjects}, "", nil)
	e.must(err)
	m(e.s.ProposeResolution(e.ctx, by, id))
	return m(e.s.GetResolution(e.ctx, e.sec, id))
}

// failingAuditor refuses every write: reads of sensitive data must then fail closed.
type failingAuditor struct{}

func (failingAuditor) Record(context.Context, AuditEvent) error {
	return errors.New("audit unavailable")
}
func (failingAuditor) RecordBatch(context.Context, []AuditEvent) error {
	return errors.New("audit unavailable")
}

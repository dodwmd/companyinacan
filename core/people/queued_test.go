package people

// Time off through can-core's approval engine (specs/070-approvals-inbox), against the fake can-core.

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/modapproval"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

type engineEnv struct {
	*toEnv
	core     *fakecore.Core
	zed, boo Actor
}

// newEngineEnv adds a manager (boo) and a report (zed) whose SSO subjects are can-core accounts.
func newEngineEnv(t *testing.T) *engineEnv {
	t.Helper()
	x := &engineEnv{toEnv: newToEnv(t), core: fakecore.New(t, "people")}
	mk := func(given, account, manager string) Actor {
		p, err := x.s.CreatePerson(x.ctx, x.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: given, FamilyName: "Example",
			WorkEmail: strings.ToLower(given) + "@example.com", SSOSubject: account, StartDate: "2025-01-06", ManagerID: manager})
		x.must(err)
		roles := []string{RoleEmployee}
		if manager == "" {
			roles = append(roles, RoleManager)
		}
		return Actor{Subject: account, Roles: roles, PersonID: p.ID, HasPerson: true, Status: StActive}
	}
	x.boo = mk("Boo", "user:boo", "")
	x.zed = mk("Zed", "user:zed", x.boo.PersonID)
	_, err := x.s.RunAccruals(x.ctx, x.hr)
	x.must(err)
	x.s.UseCore(x.core.Client(t), slog.New(slog.DiscardHandler))
	return x
}

func (x *engineEnv) sync() { x.t.Helper(); x.must(x.s.SyncApprovals(x.ctx)) }

func (x *engineEnv) decided(id, outcome string, by ...string) {
	x.t.Helper()
	ev, ok := x.core.DecideBy(id, outcome, by...)
	if !ok {
		x.t.Fatalf("no approval %s", id)
	}
	x.must(x.s.CanEventHandler()(x.ctx, ev))
}

func (x *engineEnv) get(id string) *LeaveRequest {
	x.t.Helper()
	r, err := x.s.GetLeaveRequest(x.ctx, x.zed, id)
	x.must(err)
	return r
}

func TestTimeOffIsDecidedByTheEngine(t *testing.T) {
	x := newEngineEnv(t)
	lr := x.submit(x.zed, x.vac, "2026-03-23", "2026-03-24")
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.boo, lr.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("approved before the engine did: %v", err)
	}
	x.sync()
	reqs := x.core.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%+v", reqs)
	}
	a := reqs[0]
	if a.Type != "timeoff" || a.Requester != "user:zed" || a.Manager != "user:boo" || a.Amount != 0 {
		t.Fatalf("%+v", a)
	}
	// Nothing about the leave leaves the module: no dates, type or note.
	if got := a.Title + string(a.Payload); strings.Contains(got, "2026-03") || strings.Contains(got, "vacation") || strings.Contains(got, "private") {
		t.Fatalf("leaked: %s", got)
	}
	if !strings.Contains(string(a.Payload), `"/timeoff/`+lr.ID+`"`) {
		t.Fatalf("no link to the module: %s", a.Payload)
	}
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.boo, lr.ID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("rejected around an open request: %v", err)
	}
	before := x.balance(x.zed, x.zed.PersonID, x.vac, "2026-12-31")
	x.decided(a.ID, "approved", "user:boo")
	if got := x.get(lr.ID); got.Status != "approved" || got.DecidedBy != x.boo.PersonID {
		t.Fatalf("%+v", got)
	}
	if after := x.balance(x.zed, x.zed.PersonID, x.vac, "2026-12-31"); after != before-4 {
		t.Fatalf("balance %d -> %d", before, after)
	}
	x.decided(a.ID, "approved", "user:boo") // redelivered
	if after := x.balance(x.zed, x.zed.PersonID, x.vac, "2026-12-31"); after != before-4 {
		t.Fatal("applied twice")
	}
}

func TestTimeOffRejectedSelfApprovedAndWithdrawn(t *testing.T) {
	x := newEngineEnv(t)
	for i, c := range []struct {
		outcome, status string
		by              []string
	}{
		{"rejected", "rejected", nil},
		{"approved", "rejected", []string{" USER:ZED"}}, // the requester under another spelling
		{"approved", "rejected", nil},                   // nobody named
		{"cancelled", "draft", nil},
		{"expired", "draft", nil},
	} {
		day := []string{"2026-04-06", "2026-04-13", "2026-04-20", "2026-04-27", "2026-05-04"}[i]
		lr := x.submit(x.zed, x.vac, day, day)
		x.sync()
		x.decided(x.core.Requests()[i].ID, c.outcome, c.by...)
		if got := x.get(lr.ID); got.Status != c.status {
			t.Fatalf("%s %v: %+v", c.outcome, c.by, got)
		}
	}
	// Cancelling in the module withdraws the request in the inbox.
	lr := x.submit(x.zed, x.vac, "2026-06-01", "2026-06-01")
	x.sync()
	n := len(x.core.Requests())
	_, err := x.s.CancelLeaveRequest(x.ctx, x.zed, lr.ID)
	x.must(err)
	x.sync()
	if got := x.core.Requests()[n-1].Status; got != "cancelled" {
		t.Fatalf("in the inbox: %s", got)
	}
}

func TestTimeOffFailsClosedAndQueuesWhatWasPending(t *testing.T) {
	x := newEngineEnv(t)
	x.core.SetDown(true)
	lr := x.submit(x.zed, x.vac, "2026-03-23", "2026-03-24") // accepted, waits
	if err := x.s.SyncApprovals(x.ctx); err == nil {
		t.Fatal("a pass against a dead can-core must report it")
	}
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.boo, lr.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("approved without the engine: %v", err)
	}
	if engine, state, _, err := x.s.ApprovalState(x.ctx, lr.ID); err != nil || !engine || state != modapproval.Queued {
		t.Fatalf("%v %q %v", engine, state, err)
	}
	// Pending from before the upgrade: no row at all.
	_, err := x.s.db.Exec(`DELETE FROM approval_queue`)
	x.must(err)
	x.core.SetDown(false)
	x.sync()
	x.sync()
	if reqs := x.core.Requests(); len(reqs) != 1 || reqs[0].Requester != "user:zed" || reqs[0].Manager != "user:boo" {
		t.Fatalf("%+v", reqs)
	}
}

// An approval that cannot be applied (no working days any more) is kept for the module's route.
func TestTimeOffApprovalThatCannotBeApplied(t *testing.T) {
	x := newEngineEnv(t)
	lr := x.submit(x.zed, x.vac, "2026-03-23", "2026-03-23")
	x.sync()
	_, err := x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "", Name: "none"})
	if err == nil {
		t.Fatal("a calendar needs a location")
	}
	_, err = x.s.db.Exec(`UPDATE people SET location='Oslo' WHERE id=?`, x.zed.PersonID)
	x.must(err)
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "Oslo", Name: "Oslo", Holidays: []Holiday{{Date: "2026-03-23", Name: "Day off"}}})
	x.must(err)
	x.decided(x.core.Requests()[0].ID, "approved", "user:boo")
	if got := x.get(lr.ID); got.Status != "pending" {
		t.Fatalf("%+v", got)
	}
	if _, state, note, _ := x.s.ApprovalState(x.ctx, lr.ID); state != modapproval.Approved || !strings.Contains(note, "working days") {
		t.Fatalf("%q %q", state, note)
	}
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "Oslo", Name: "Oslo"})
	x.must(err)
	got, err := x.s.DecideLeaveRequest(x.ctx, x.boo, lr.ID, true)
	x.must(err)
	if got.Status != "approved" {
		t.Fatalf("%+v", got)
	}
}

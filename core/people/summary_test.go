package people

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// All names are fake. The clock starts on Tuesday 2026-03-10.

func TestSummaryCountsOnlyWhatTheActorCouldSeeAndKeepsLeaveMasked(t *testing.T) {
	x := newToEnv(t)
	tm, err := x.s.CreateOrgUnit(x.ctx, x.hr, "teams", OrgInput{Name: "Platform"})
	x.must(err)
	for _, p := range []*Person{x.ann, x.ben, x.mgr} {
		_, err = x.s.UpdatePerson(x.ctx, x.hr, p.ID, PersonPatch{TeamID: sp(tm.ID)})
		x.must(err)
	}
	// Ann: approved long leave that sets the leave status. Ben: a pending request over the same days.
	lr := x.submit(x.annA, x.parental, "2026-03-16", "2026-03-27")
	_, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	x.submit(x.benA, x.vac, "2026-03-16", "2026-03-17")
	*x.clock = time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)
	_, err = x.s.ApplyDue(x.ctx)
	x.must(err)

	show := func(a Actor) string {
		t.Helper()
		st, err := x.s.Summary(x.ctx, a)
		x.must(err)
		n := func(p *int) string {
			if p == nil {
				return "-"
			}
			return strconv.Itoa(*p)
		}
		return n(st.Headcount) + "/" + n(st.OffToday)
	}
	all, _, err := x.s.ListPeople(x.ctx, x.hr, PersonFilter{Limit: 100})
	x.must(err)
	head := strconv.Itoa(len(all))
	before := len(x.aud.evs)
	for name, c := range map[string]struct {
		a    Actor
		want string
	}{
		"hr sees everyone's leave":                 {x.hr, head + "/1"},
		"a teammate sees the same as the calendar": {x.benA, head + "/1"},
		"the manager":                              {x.mgrA, head + "/1"},
		"outside the team: nothing of it":          {x.catA, head + "/0"},
		"no role at all: nothing":                  {Actor{Subject: "user:x"}, "-/-"},
		"no person record and not hr: nothing":     {Actor{Subject: "user:x", Roles: []string{RoleEmployee}}, "-/-"},
		"a token without time off: no off figure":  {Actor{Subject: x.benA.Subject, Roles: x.benA.Roles, PersonID: x.benA.PersonID, HasPerson: true, Status: StActive, Restricted: true, Perms: []string{"people.read"}}, head + "/-"},
	} {
		if got := show(c.a); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	// Counting reads no leave type, so nothing is recorded as a read of one.
	for _, ev := range x.aud.evs[before:] {
		if ev.Action == "read" && strings.HasPrefix(ev.Resource, "leave-request:") {
			t.Errorf("the summary recorded a read of %s", ev.Resource)
		}
	}
}

func TestSummaryBuilderMapsPermissionsAsSignInDoes(t *testing.T) {
	x := newToEnv(t)
	build := summaryBuilder(x.s)
	if f, err := build(x.ctx, "user:nobody", []string{"people.write"}); err != nil || len(f) != 0 {
		t.Fatalf("without people.read: %+v %v", f, err)
	}
	f, err := build(x.ctx, "user:nobody", []string{"people.read", "people.timeoff.read"})
	x.must(err)
	if len(f) != 0 { // an account no person record is linked to, and not hr
		t.Fatalf("%+v", f)
	}
	f, err = build(x.ctx, "user:nobody", []string{"people.read", "people.write", "people.timeoff.read"})
	x.must(err)
	if len(f) != 2 || f[0].Key != "people.headcount" || f[1].Key != "people.off_today" {
		t.Fatalf("%+v", f)
	}
	for _, v := range f {
		if len(v.Amounts) != 0 {
			t.Fatalf("%+v", v)
		}
	}
}

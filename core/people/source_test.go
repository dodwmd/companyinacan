package people

import "testing"

func TestHireFromAnyOtherSourceIsIgnored(t *testing.T) {
	e := newEnv(t)
	ev := hiredEvent("evt_s", "cand_0123456789ab")
	ev.Source = "forms"
	e.must(e.s.CanEventHandler()(e.ctx, ev))
	if people, _, _ := e.s.ListPeople(e.ctx, e.hr, PersonFilter{}); len(people) != 0 {
		t.Fatal("a hire handed off by a module other than jobs created a person")
	}
}

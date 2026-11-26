package people

import "testing"

func (x *toEnv) eventData(typ string) []map[string]any {
	x.t.Helper()
	all, err := x.s.AllEvents(x.ctx)
	x.must(err)
	var out []map[string]any
	for _, ev := range all {
		if ev.Type == typ {
			out = append(out, ev.Data)
		}
	}
	return out
}

// The events carry the request id and accounts only: no person id, dates or leave type.
func TestTimeOffReminderEvents(t *testing.T) {
	x := newToEnv(t)
	lr := x.submit(x.annA, x.sick, "2026-03-23", "2026-03-24")
	got := x.eventData(EventTimeOffRequested)
	if len(got) != 1 || len(got[0]) != 2 || got[0]["request_id"] != lr.ID || got[0]["approver"] != "sso-mgr" {
		t.Fatalf("requested: %v", got)
	}
	_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, false)
	x.must(err)
	got = x.eventData(EventTimeOffDecided)
	if len(got) != 1 || len(got[0]) != 2 || got[0]["request_id"] != lr.ID || got[0]["requester"] != "sso-ann" {
		t.Fatalf("decided: %v", got)
	}
}

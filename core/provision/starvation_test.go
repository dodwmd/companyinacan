package provision

import (
	"fmt"
	"testing"
	"time"
)

// Runs that can only wait (a mass offboarding with last days months away) must not occupy the
// worker's window and starve a due or urgent run, or a joiner.
func TestWaitingRunsDoNotStarveDueWork(t *testing.T) {
	h := newHarness(t, func(p *Policy) { p.MaxStepsPerTick = 25 })
	for i := 0; i < 120; i++ {
		id := fmt.Sprintf("pers_%024x", 0x1000+i)
		p := person(id, "offboarding", fmt.Sprintf("p%d@example.com", i))
		p.EndDate = "2027-03-01"
		h.dir.put(p)
		h.linkAccounts(id, "app-a")
		h.mustEvent(EventPersonStatusChanged, statusChanged(id, "active", "offboarding", "2026-06-10"))
	}
	for i := 0; i < 4; i++ { // enough passes to examine every waiting run
		h.tick()
	}
	// later arrivals: an urgent termination and a joiner
	h.dir.put(person(bob, "offboarding", "bob@example.com"))
	h.linkAccounts(bob, "app-a")
	h.mustEvent(EventPersonStatusChanged, statusChanged(bob, "active", "offboarding", "2026-06-10"))
	if err := h.eng.FlagUrgent(t.Context(), "user:hr-lead", bob); err != nil {
		t.Fatal(err)
	}
	h.dir.put(person(carol, "active", "carol@example.com"))
	h.mustEvent(EventPersonCreated, created(carol, "active"))
	before := h.a.count("disable_account")
	h.tick()
	if h.a.count("disable_account") != before+1 {
		t.Fatal("the urgent termination was not processed by the next pass")
	}
	if h.a.count("create_account") != 1 {
		t.Fatal("the joiner was not processed by the next pass")
	}
	// the waiting runs are examined again when their date arrives, not before
	var due int
	if err := h.st.db.QueryRow(`SELECT count(*) FROM runs WHERE state='scheduled' AND wake_at>?`, h.clk.now().Unix()).Scan(&due); err != nil || due < 100 {
		t.Fatalf("waiting runs not parked: %d %v", due, err)
	}
	h.clk.add(300 * 24 * time.Hour)
	h.settle()
	if h.a.count("disable_account") < 100 {
		t.Fatalf("scheduled leavers did not run on their date: %d", h.a.count("disable_account"))
	}
}

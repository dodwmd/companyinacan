package board

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/dodwmd/companyinacan/core/modclient"
)

func (e *env) filingEvents() []Event {
	e.t.Helper()
	var out []Event
	for _, ev := range m(e.s.AllEvents(e.ctx)) {
		if ev.Type == EventFilingDue {
			out = append(out, ev)
		}
	}
	return out
}

func TestFilingBacklogIsOneEventOnce(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	for i := range summaryAbove + 2 {
		m(e.s.CreateObligation(e.ctx, e.sec, le.ID, ObligationInput{Name: "Return " + strconv.Itoa(i), FirstDueOn: "2026-03-20", Recurrence: "once", LeadDays: []int{30, 0}}))
	}
	if n := m(e.s.ApplyDue(e.ctx)); n != summaryAbove+2 {
		t.Fatalf("ApplyDue = %d", n)
	}
	if n := m(e.s.ApplyDue(e.ctx)); n != 0 {
		t.Fatalf("again: %d", n)
	}
	got := e.filingEvents()
	if len(got) != 1 || len(got[0].Data) != 1 || got[0].Data["count"] != float64(summaryAbove+2) {
		t.Fatalf("%+v", got)
	}
}

// The upgrade: what was never delivered (there was no dispatcher) is not published all at once, and
// the pending filings are announced afresh.
func TestRemindersMigrationRetiresTheUndeliveredBacklog(t *testing.T) {
	e := newEnv(t).base()
	le := e.entity()
	m(e.s.CreateObligation(e.ctx, e.sec, le.ID, ObligationInput{Name: "Annual return", FirstDueOn: "2026-03-20", Recurrence: "once", LeadDays: []int{30, 0}}))
	if n := m(e.s.ApplyDue(e.ctx)); n != 1 {
		t.Fatalf("ApplyDue = %d", n)
	}
	sql, err := fs.ReadFile(migrationFS, "migrations/005_reminders.sql")
	e.must(err)
	_, err = e.s.db.Exec(string(sql))
	e.must(err)
	if n := e.count(`SELECT COUNT(*) FROM outbox WHERE delivered_at IS NULL`); n != 0 {
		t.Fatalf("%d events still queued", n)
	}
	if n := m(e.s.ApplyDue(e.ctx)); n != 1 {
		t.Fatalf("a filing nobody heard about is announced afresh: %d", n)
	}
	// Once delivered it is not raised again, whatever runs the statements a second time (a restore).
	_, err = e.s.db.Exec(`UPDATE outbox SET delivered_at = 1, attempts = 1 WHERE delivered_at IS NULL`)
	e.must(err)
	_, err = e.s.db.Exec(string(sql))
	e.must(err)
	if n := m(e.s.ApplyDue(e.ctx)); n != 0 {
		t.Fatalf("a delivered reminder was raised again: %d", n)
	}
}

func TestEventsGoThroughCanCoreAndKeepTryingWhileRefused(t *testing.T) {
	var status atomic.Int32
	var calls atomic.Int32
	status.Store(http.StatusForbidden) // the manifest that declares the event is not recorded yet
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			w.WriteHeader(404)
			return
		}
		calls.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer core.Close()
	tok := filepath.Join(t.TempDir(), "token")
	e := newEnv(t).base()
	e.must(os.WriteFile(tok, []byte("projected-token"), 0o600))
	le := e.entity()
	m(e.s.CreateObligation(e.ctx, e.sec, le.ID, ObligationInput{Name: "Annual return", FirstDueOn: "2026-03-20", Recurrence: "once", LeadDays: []int{30, 0}}))
	m(e.s.ApplyDue(e.ctx))
	d := NewDispatcher("", nil, nil)
	d.Can = &modclient.Client{BaseURL: core.URL, TokenFile: tok, ModuleID: "entity"}
	for range maxDeliveryAttempts + 4 { // more refusals than a webhook is given
		e.clock = e.clock.Add(24 * 3600 * 1e9)
		m(e.s.DeliverPending(e.ctx, d))
	}
	status.Store(http.StatusAccepted)
	e.clock = e.clock.Add(24 * 3600 * 1e9)
	if n := m(e.s.DeliverPending(e.ctx, d)); n < 1 {
		t.Fatalf("delivered %d after %d calls", n, calls.Load())
	}
	if n := e.count(`SELECT COUNT(*) FROM outbox WHERE delivered_at IS NULL AND type = ?`, EventFilingDue); n != 0 {
		t.Fatalf("%d filing events still queued", n)
	}
}

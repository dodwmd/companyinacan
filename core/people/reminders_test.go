package people

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dodwmd/companyinacan/core/modclient"
)

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

// submitted_by is folded for separation of duties; the account that is notified is not, because
// can-core's accounts are case-sensitive.
func TestTimeOffRequesterAccountKeepsItsCase(t *testing.T) {
	x := newToEnv(t)
	who := x.annA
	who.Subject = "user:Ann.B"
	lr := x.submit(who, x.vac, "2026-03-23", "2026-03-24")
	_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	got := x.eventData(EventTimeOffDecided)
	if len(got) != 1 || got[0]["requester"] != "user:Ann.B" {
		t.Fatalf("%v", got)
	}
}

func TestEventsKeepTryingWhileCanCoreRefuses(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusForbidden)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) }))
	defer core.Close()
	tok := filepath.Join(t.TempDir(), "token")
	x := newToEnv(t)
	x.must(os.WriteFile(tok, []byte("projected-token"), 0o600))
	x.submit(x.annA, x.vac, "2026-03-23", "2026-03-24")
	d := NewDispatcher("", nil, slog.New(slog.DiscardHandler))
	d.Can = &modclient.Client{BaseURL: core.URL, TokenFile: tok, ModuleID: "people"}
	deliver := func() {
		_, err := x.s.db.ExecContext(x.ctx, `UPDATE outbox SET next_attempt = 0`)
		x.must(err)
		_, err = x.s.DeliverPending(x.ctx, d)
		x.must(err)
	}
	for range maxDeliveryAttempts + 4 {
		deliver()
	}
	status.Store(http.StatusAccepted)
	for range 20 { // the outbox goes out a batch at a time
		deliver()
	}
	left, err := x.s.PendingEvents(x.ctx)
	x.must(err)
	if len(left) != 0 {
		t.Fatalf("%d events given up on", len(left))
	}
}

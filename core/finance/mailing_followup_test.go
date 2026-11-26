package finance

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// refusingCore is a real client of a can-core that is not listening.
func refusingCore(t *testing.T) *modclient.Client {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &modclient.Client{BaseURL: "http://" + addr, TokenFile: token, ModuleID: "finance"}
}

// A refused connection sent nothing: a manual send is "failed", not "unknown".
func TestManualSendWithCoreRefusingIsFailed(t *testing.T) {
	c := refusingCore(t)
	e := newMailEnv(t, func(_ *Config, o *Options) { o.Mailer = c })
	n := e.sentInvoice(0)
	var m DocumentMail
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, &m)
	if m.Status != DMFailed {
		t.Fatalf("status %q", m.Status)
	}
	// A reminder waits for the service instead.
	e.days(5)
	e.schedule(1)
	if e.run() != 0 {
		t.Fatal("nothing can have been sent")
	}
	var handed int64
	var status string
	if err := e.app.store.db.QueryRow(`SELECT status, handed_at FROM document_mails WHERE kind = 'reminder'`).Scan(&status, &handed); err != nil || status != DMRetry || handed != 0 {
		t.Fatalf("%s %d %v", status, handed, err)
	}
}

// A reminder whose replies stay lost for 72 hours may have left at any of the attempts: the next
// step is 3 days after the last one, not in the sweep that gives up.
func TestDunningGapAfterRepliesStayLost(t *testing.T) {
	e := newMailEnv(t)
	n := e.sentInvoice(0)
	e.days(2)
	e.schedule(1, 4)
	e.mail.fail(fmt.Errorf("%w: connection reset", modclient.ErrUnavailable)) // no reply, every time
	for range 3 {                                                             // now, +36 h, +72 h: the last attempt the limit allows
		e.run()
		e.advance(36 * time.Hour)
	}
	e.mail.fail(nil)
	// 108 hours after the claim: given up, and step 2 is due but too soon.
	if e.run() != 0 {
		t.Fatalf("sent right after giving up: %+v", e.mails(n).Items)
	}
	if got := e.mails(n).Items; len(got) != 1 || got[0].Status != DMFailed {
		t.Fatalf("%+v", got)
	}
	e.days(1)
	if e.run() != 0 {
		t.Fatal("less than 3 days after the last lost attempt")
	}
	e.advance(13 * time.Hour)
	if e.run() != 1 {
		t.Fatalf("step 2 after the gap: %+v", e.mails(n).Items)
	}
}

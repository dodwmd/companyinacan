package finance

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/modclient"
)

// A restored database does not know who has paid or disputed since the dump: nothing may leave
// until a person says so, including what was waiting when the dump was taken.
func TestRestoreSwitchesRemindersOff(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.db"), filepath.Join(dir, "dst.db")
	e := newMailEnv(t, func(c *Config, _ *Options) { c.DBPath = src })
	e.schedule(1)
	a, b, c := e.sentInvoice(0), e.sentInvoice(0), e.sentInvoice(0)
	e.days(2)
	e.mail.fail(&modclient.StatusError{Status: http.StatusTooManyRequests})
	e.run() // a is waiting to be retried
	e.mail.fail(nil)
	if err := e.app.store.inTx(context.Background(), func(tx *sql.Tx) error { // b was claimed and not handed over
		_, err := e.app.store.claimMail(context.Background(), tx, "can-finance", DocReminder, "x", &b, e.cust, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	restore := func(from, to string) string {
		t.Helper()
		var out bytes.Buffer
		set := t.TempDir()
		for _, run := range [][]string{{"backup", from, "-out"}, {"restore", to, "-from"}} {
			cfg := filepath.Join(t.TempDir(), "finance.yaml")
			if err := os.WriteFile(cfg, []byte("db_path: "+run[1]+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if code := modbackup.Main("can-finance", Migrations(), []string{run[0], "-config", cfg, run[2], set}, func(string) string { return "" }, &out, &out); code != 0 {
				t.Fatalf("%s: %d %s", run[0], code, out.String())
			}
		}
		return out.String()
	}
	if out := restore(src, dst); !strings.Contains(out, "payment reminders to customers are now OFF") || !strings.Contains(out, "2 waiting customer message(s) cancelled") {
		t.Fatalf("the restore does not say what it did: %s", out)
	}

	// The restored server, with the same relay.
	mail, now := &queueFake{}, e.now
	r := newMailEnvOn(t, dst, mail, now)
	if n := r.run(); n != 0 || len(mail.all()) != 0 {
		t.Fatalf("a restored server mailed customers: %d", n)
	}
	var p DunningPolicy
	r.must(200, adminTok, "GET", "/v1/dunning", nil, &p)
	if p.Enabled || p.RestoredAt == nil || !slices.Equal(p.DaysAfterDue, []int{1}) {
		t.Fatalf("%+v", p)
	}
	for _, n := range []int64{a, b} {
		got := r.mails(n)
		if len(got.Items) != 1 || got.Items[0].Status != DMCancelled || !got.Reminders.OffSinceRestore {
			t.Fatalf("%+v", got)
		}
	}
	if !strings.Contains(r.audit(), `"dunning.restore"`) {
		t.Fatal("not audited")
	}
	// An administrator turns them on again, and the notice goes.
	r.schedule(1)
	var again DunningPolicy
	r.must(200, adminTok, "GET", "/v1/dunning", nil, &again)
	if again.RestoredAt != nil || r.run() != 1 || r.mails(c).Items[0].Status != DMQueued {
		t.Fatalf("%+v %+v", again, mail.all())
	}

	// With reminders off there is nothing to announce.
	off := filepath.Join(dir, "off.db")
	r.must(200, adminTok, "PUT", "/v1/dunning", obj{"enabled": false, "days_after_due": []int{1}}, nil)
	if out := restore(dst, off); strings.Contains(out, "NOTE") {
		t.Fatalf("%s", out)
	}
}

// newMailEnvOn opens an existing database.
func newMailEnvOn(t *testing.T, path string, mail *queueFake, now time.Time) *mailEnv {
	t.Helper()
	e := newEnv(t, func(c *Config, o *Options) { c.DBPath, o.Mailer = path, mail })
	e.now = now
	return &mailEnv{env: e, mail: mail}
}

// flaky loses the reply of the first n calls, or never answers.
type flaky struct {
	*queueFake
	lose  int
	calls []string
}

func (f *flaky) QueueEmail(ctx context.Context, e modclient.Email) ([]string, error) {
	f.calls = append(f.calls, e.SourceID)
	ids, err := f.queueFake.QueueEmail(ctx, e)
	if f.lose > 0 {
		f.lose--
		return nil, fmt.Errorf("%w: connection reset", modclient.ErrUnavailable)
	}
	return ids, err
}

func TestManualSendWithALostReply(t *testing.T) {
	f := &flaky{queueFake: &queueFake{}}
	e := newMailEnv(t, func(_ *Config, o *Options) { o.Mailer = f })
	n := e.issued(14)
	f.lose = 1
	var m DocumentMail
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, &m)
	var inv Invoice
	e.must(200, adminTok, "GET", "/v1/invoices/"+id(n), nil, &inv)
	if m.Status != DMQueued || len(f.calls) != 2 || f.calls[0] != f.calls[1] || len(f.all()) != 1 || inv.Status != StSent {
		t.Fatalf("%s %v %s", m.Status, f.calls, inv.Status)
	}
	// No answer twice: not "failed", and not to be sent again straight away.
	other := e.issued(14)
	f.lose = 2
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(other)+"/email", nil, &m)
	e.must(200, adminTok, "GET", "/v1/invoices/"+id(other), nil, &inv)
	if m.Status != DMUnknown || inv.Status != StApproved {
		t.Fatalf("%s %s", m.Status, inv.Status)
	}
	e.must(409, e.maker, "POST", "/v1/invoices/"+id(other)+"/email", nil, nil)
}

func TestReminderGapRunsFromTheHandover(t *testing.T) {
	e := newMailEnv(t)
	n := e.sentInvoice(0)
	e.schedule(1, 2)
	e.days(1)
	e.mail.fail(&modclient.StatusError{Status: http.StatusServiceUnavailable})
	e.run()
	e.days(2) // step 2 is reached while step 1 still waits
	e.run()
	if got := e.mails(n).Items; len(got) != 1 || got[0].Status != DMRetry {
		t.Fatalf("a later step overtook a waiting one: %+v", got)
	}
	e.mail.fail(nil)
	if e.run() != 1 { // step 1 leaves now, two days after it was claimed
		t.Fatal("retry")
	}
	e.days(2)
	if e.run() != 0 {
		t.Fatal("the gap must count from the handover, not from the claim")
	}
	e.days(1)
	if e.run() != 1 {
		t.Fatal("step 2 three days after the handover")
	}
}

func TestPartPaidButNeverSentIsNotChased(t *testing.T) {
	e := newMailEnv(t)
	e.schedule(1)
	byHand, byMail := e.issued(0), e.issued(0)
	for _, n := range []int64{byHand, byMail} {
		e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/payments", obj{"amount_minor": 1000}, nil)
	}
	e.days(3)
	if e.run() != 0 || len(e.mail.all()) != 0 {
		t.Fatalf("an invoice nobody sent was chased: %+v", e.mail.all())
	}
	var a, b Invoice
	e.must(200, e.maker, "POST", "/v1/invoices/"+id(byHand)+"/send", nil, &a)
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(byMail)+"/email", nil, nil)
	e.must(200, adminTok, "GET", "/v1/invoices/"+id(byMail), nil, &b)
	for _, v := range []Invoice{a, b} {
		if v.Status != StPartiallyPaid || v.SentAt == nil {
			t.Fatalf("%s %v", v.Status, v.SentAt)
		}
	}
	if e.countEvents(EventInvoiceSent) != 2 || e.run() != 2 {
		t.Fatalf("sent invoices are chased: %+v", e.mail.all())
	}
}

func TestSenderIsWarnedAboutAnIncompletePDF(t *testing.T) {
	e := newMailEnv(t)
	plain := e.issued(14)
	var p Party
	e.must(201, adminTok, "POST", "/v1/parties", obj{"name": "Zoë Ελληνικά", "kind": "customer", "email": "x@customer.example"}, &p)
	var inv Invoice
	e.must(201, e.maker, "POST", "/v1/invoices", obj{"party_id": p.ID, "lines": []obj{line("work", 1000, 100)}}, &inv)
	e.must(200, adminTok, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	for n, want := range map[int64]bool{plain: false, inv.ID: true} {
		var out struct {
			Incomplete bool `json:"pdf_incomplete"`
		}
		e.must(200, adminTok, "GET", "/v1/invoices/"+id(n)+"/emails", nil, &out)
		if out.Incomplete != want {
			t.Errorf("invoice %d: pdf_incomplete %v", n, out.Incomplete)
		}
		var v Invoice
		e.must(200, adminTok, "GET", "/v1/invoices/"+id(n), nil, &v)
		// The warning agrees with what the renderer does.
		if got := bytes.Contains(RenderInvoicePDF(&v, e.seller()), []byte("Some characters cannot be shown")); got != want {
			t.Errorf("invoice %d: the PDF says %v", n, got)
		}
	}
	var m DocumentMail
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(inv.ID)+"/email", nil, &m)
	if !m.PDFIncomplete {
		t.Errorf("%+v", m)
	}
}

func (e *mailEnv) seller() Seller {
	return Seller{Name: "Example Seller Pty", Address: "1 Fake Street"}
}

func TestPDFRendersAreBounded(t *testing.T) {
	e := newMailEnv(t)
	n := e.issued(14)
	for range cap(pdfSlots) {
		pdfSlots <- struct{}{}
	}
	defer func() {
		for len(pdfSlots) > 0 {
			<-pdfSlots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, _, _ := e.req(adminTok, "GET", "/v1/invoices/"+id(n)+"/pdf", nil)
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("answered %d while every slot was taken", code)
	case <-ctx.Done():
	}
	<-pdfSlots
	if code := <-done; code != 200 {
		t.Fatalf("after a slot freed: %d", code)
	}
}

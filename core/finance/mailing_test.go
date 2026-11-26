package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

const billBase = "https://billing.example.test"

// queueFake is a mail service that also answers delivery status and de-duplicates on source_id,
// as can-core does.
type queueFake struct {
	mu     sync.Mutex
	sent   []modclient.Email
	err    error // returned for every send
	lost   bool  // accept the message, then answer as if unavailable (a lost reply)
	status map[string]string
	seen   map[string]string // source id -> message text
}

func (m *queueFake) SendEmail(ctx context.Context, e modclient.Email) error {
	_, err := m.QueueEmail(ctx, e)
	return err
}

func (m *queueFake) QueueEmail(_ context.Context, e modclient.Email) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if m.seen == nil {
		m.seen = map[string]string{}
	}
	id := "eml_" + e.SourceID
	if text, ok := m.seen[e.SourceID]; ok {
		if text != e.Text {
			return nil, fmt.Errorf("refused: %w", &modclient.StatusError{Status: http.StatusConflict})
		}
		return []string{id}, nil
	}
	m.seen[e.SourceID] = e.Text
	m.sent = append(m.sent, e)
	if m.lost {
		return nil, fmt.Errorf("%w: connection reset", modclient.ErrUnavailable)
	}
	return []string{id}, nil
}

func (m *queueFake) EmailStatus(_ context.Context, id string) (modclient.EmailStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[id]
	if st == "gone" {
		return modclient.EmailStatus{}, &modclient.StatusError{Status: http.StatusNotFound}
	}
	if st == "" {
		st = "pending"
	}
	return modclient.EmailStatus{ID: id, Status: st}, nil
}

func (m *queueFake) all() []modclient.Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sent)
}

func (m *queueFake) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

type mailEnv struct {
	*env
	mail  *queueFake
	logs  *lockedBuf
	maker string
	cust  int64
}

const custEmail = "billing@customer.example"

func newMailEnv(t *testing.T, opts ...envOpt) *mailEnv {
	t.Helper()
	m, logs := &queueFake{}, &lockedBuf{}
	e := newEnv(t, append([]envOpt{func(_ *Config, o *Options) {
		o.Mailer = m
		o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}}, opts...)...)
	out := &mailEnv{env: e, mail: m, logs: logs}
	out.maker = e.token("maker", []string{"finance.write"}, nil, nil, false)
	var p Party
	e.must(201, adminTok, "POST", "/v1/parties", obj{"name": "Zebra Widgets", "kind": "customer", "email": custEmail, "address": "1 Fake Road"}, &p)
	out.cust = p.ID
	return out
}

// issued creates and approves an invoice of 50.00 for the customer, due in dueDays.
func (e *mailEnv) issued(dueDays int) int64 {
	e.t.Helper()
	var inv Invoice
	e.must(201, e.maker, "POST", "/v1/invoices", obj{"party_id": e.cust, "lines": []obj{line("Anvil", 1000, 5000)}, "due_days": dueDays}, &inv)
	e.must(200, adminTok, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	return inv.ID
}

// sentInvoice is an invoice recorded as sent without a mail.
func (e *mailEnv) sentInvoice(dueDays int) int64 {
	e.t.Helper()
	n := e.issued(dueDays)
	e.must(200, e.maker, "POST", "/v1/invoices/"+id(n)+"/send", nil, nil)
	return n
}

func (e *mailEnv) schedule(days ...int) {
	e.t.Helper()
	e.must(200, adminTok, "PUT", "/v1/dunning", obj{"enabled": true, "days_after_due": days, "max_overdue_days": 90}, nil)
}

func (e *mailEnv) run() int {
	e.t.Helper()
	var out struct {
		Reminders int `json:"reminders"`
	}
	e.must(200, adminTok, "POST", "/v1/dunning/run", nil, &out)
	return out.Reminders
}

func (e *mailEnv) days(n int) { e.advance(time.Duration(n) * 24 * time.Hour) }

type mailList struct {
	Items     []DocumentMail `json:"items"`
	Reminders ReminderState  `json:"reminders"`
}

func (e *mailEnv) mails(inv int64) mailList {
	e.t.Helper()
	var out mailList
	e.must(200, adminTok, "GET", "/v1/invoices/"+id(inv)+"/emails", nil, &out)
	return out
}

func (e *mailEnv) audit() string {
	e.t.Helper()
	items, err := e.app.store.ListAudit(context.Background(), 0, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(items)
	return string(b)
}

var billLinkRE = regexp.MustCompile(regexp.QuoteMeta(billBase) + `(/public/invoices/[A-Za-z0-9._-]+)`)

func TestEmailInvoice(t *testing.T) {
	e := newMailEnv(t)
	n := e.issued(14)
	var m DocumentMail
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, &m)
	if m.Status != DMQueued || m.Kind != DocDocument || m.InvoiceID == nil || *m.InvoiceID != n {
		t.Fatalf("%+v", m)
	}
	sent := e.mail.all()
	if len(sent) != 1 || !slices.Equal(sent[0].To, []string{custEmail}) || sent[0].Subject != "Invoice INV-00001 from Example Seller Pty" {
		t.Fatalf("%+v", sent)
	}
	for _, want := range []string{"Total: 50.00 USD", "Balance due: 50.00 USD", "nothing is attached"} {
		if !strings.Contains(sent[0].Text, want) {
			t.Errorf("mail lacks %q:\n%s", want, sent[0].Text)
		}
	}
	link := billLinkRE.FindStringSubmatch(sent[0].Text)
	if link == nil {
		t.Fatalf("no link in %s", sent[0].Text)
	}
	code, _, page := e.req("", "GET", link[1], nil)
	token := link[1][strings.LastIndex(link[1], "/")+1:]
	if code != 200 || !strings.Contains(string(page), "Anvil") || !strings.Contains(string(page), `href="`+token+`/pdf"`) {
		t.Fatalf("public page %d %s", code, page)
	}
	code, hdr, pdf := e.req("", "GET", link[1]+"/pdf", nil)
	if code != 200 || hdr.Get("Content-Type") != "application/pdf" || hdr.Get("Content-Disposition") != `attachment; filename="INV-00001.pdf"` || !strings.HasPrefix(string(pdf), "%PDF-1.4") {
		t.Fatalf("public pdf %d %v", code, hdr)
	}

	var inv Invoice
	e.must(200, adminTok, "GET", "/v1/invoices/"+id(n), nil, &inv)
	if inv.Status != StSent || e.countEvents(EventInvoiceSent) != 1 {
		t.Fatalf("an emailed invoice is sent: %s", inv.Status)
	}
	// Neither the address, the name nor the link reaches the audit log, the logs or an event.
	var outbox string
	if err := e.app.store.db.QueryRow(`SELECT group_concat(payload) FROM outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{"audit": e.audit(), "logs": e.logs.String(), "events": outbox} {
		for _, bad := range []string{custEmail, "Zebra", token} {
			if strings.Contains(text, bad) {
				t.Errorf("%s holds %q", where, bad)
			}
		}
	}
	if a := e.audit(); !strings.Contains(a, `"document.email"`) || !strings.Contains(a, `"document.email.outcome"`) {
		t.Fatalf("audit: %s", a)
	}

	// A double click is refused; revoking kills the mailed links.
	e.must(409, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, nil)
	e.advance(2 * time.Minute)
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, nil)
	e.must(200, e.maker, "POST", "/v1/invoices/"+id(n)+"/link/revoke", nil, nil)
	if code, _, _ := e.req("", "GET", link[1]+"/pdf", nil); code != 404 {
		t.Fatalf("revoked pdf link: %d", code)
	}

	// Delivery status follows the mail service.
	got := e.mails(n)
	if len(got.Items) != 2 || got.Items[1].Status != DMQueued {
		t.Fatalf("%+v", got)
	}
	e.mail.mu.Lock()
	e.mail.status = map[string]string{"eml_" + e.mail.sent[0].SourceID: "sent", "eml_" + e.mail.sent[1].SourceID: "dead"}
	e.mail.mu.Unlock()
	got = e.mails(n)
	if got.Items[1].Status != DMSent || got.Items[0].Status != DMFailed {
		t.Fatalf("%+v", got.Items)
	}
}

func TestEmailInvoiceRefusals(t *testing.T) {
	e := newMailEnv(t)
	var draft Invoice
	e.must(201, e.maker, "POST", "/v1/invoices", obj{"party_id": e.cust, "lines": []obj{line("work", 1000, 100)}}, &draft)
	e.must(409, e.maker, "POST", "/v1/invoices/"+id(draft.ID)+"/email", nil, nil)

	n := e.issued(14)
	e.mail.fail(fmt.Errorf("refused: %w", &modclient.StatusError{Status: http.StatusForbidden, Detail: custEmail}))
	var m DocumentMail
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, &m)
	var inv Invoice
	e.must(200, adminTok, "GET", "/v1/invoices/"+id(n), nil, &inv)
	if m.Status != DMNotAllowed || inv.Status != StApproved || strings.Contains(e.logs.String(), custEmail) {
		t.Fatalf("%s %s", m.Status, inv.Status)
	}
	e.mail.fail(&modclient.StatusError{Status: http.StatusTooManyRequests})
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, &m)
	if m.Status != DMFailed {
		t.Fatalf("a manual send is not retried behind the sender's back: %s", m.Status)
	}
	e.mail.fail(nil)

	// A reader may not send, another party's token does not see it.
	reader := e.token("reader", []string{"finance.read"}, nil, nil, false)
	e.must(403, reader, "POST", "/v1/invoices/"+id(n)+"/email", nil, nil)
	other := e.token("other", []string{"finance.write"}, []int64{e.party(adminTok, "Else", "customer")}, nil, false)
	e.must(404, other, "POST", "/v1/invoices/"+id(n)+"/email", nil, nil)
	e.must(404, other, "GET", "/v1/invoices/"+id(n)+"/emails", nil, nil)
	e.must(404, other, "POST", "/v1/parties/"+id(e.cust)+"/statement/email", nil, nil)
	scoped := e.token("proj", []string{"finance.write"}, nil, []string{"ACME"}, false)
	e.must(403, scoped, "POST", "/v1/parties/"+id(e.cust)+"/statement/email", nil, nil)
	e.must(403, scoped, "PUT", "/v1/parties/"+id(e.cust)+"/reminders", obj{"state": "stopped"}, nil)

	e.must(200, adminTok, "POST", "/v1/invoices/"+id(n)+"/void", obj{"reason": "oops"}, nil)
	e.must(409, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, nil)

	// No address, then an erased party.
	var bare Party
	e.must(201, adminTok, "POST", "/v1/parties", obj{"name": "No Mail", "kind": "customer"}, &bare)
	var inv2 Invoice
	e.must(201, e.maker, "POST", "/v1/invoices", obj{"party_id": bare.ID, "lines": []obj{line("work", 1000, 100)}}, &inv2)
	e.must(200, adminTok, "POST", "/v1/invoices/"+id(inv2.ID)+"/approve", nil, nil)
	e.must(422, e.maker, "POST", "/v1/invoices/"+id(inv2.ID)+"/email", nil, nil)
	last := e.issued(14)
	e.must(200, adminTok, "POST", "/v1/parties/"+id(e.cust)+"/erase", nil, nil)
	e.must(409, e.maker, "POST", "/v1/invoices/"+id(last)+"/email", nil, nil)
	e.must(409, e.maker, "POST", "/v1/parties/"+id(e.cust)+"/statement/email", nil, nil)
}

func TestEmailNeedsALinkAndAMailService(t *testing.T) {
	for name, opt := range map[string]envOpt{
		"no public link": func(c *Config, o *Options) { c.PublicBaseURL, o.Mailer = "", &queueFake{} },
		"no mailer":      func(*Config, *Options) {},
	} {
		e := newEnv(t, opt)
		maker := e.token("maker", []string{"finance.write"}, nil, nil, false)
		var inv Invoice
		e.must(201, maker, "POST", "/v1/invoices", obj{"party_id": e.party(adminTok, "Customer", "customer"), "lines": []obj{line("work", 1000, 100)}}, &inv)
		e.must(200, adminTok, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
		if code := e.do(maker, "POST", "/v1/invoices/"+id(inv.ID)+"/email", nil, nil); code != 409 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

func TestEmailCreditNoteAndStatement(t *testing.T) {
	e := newMailEnv(t)
	e.must(409, e.maker, "POST", "/v1/parties/"+id(e.cust)+"/statement/email", nil, nil) // nothing open
	a, b := e.sentInvoice(14), e.sentInvoice(30)
	var cn Invoice
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(a)+"/credit-notes", obj{"reason": "Internal reason", "lines": []obj{line("Goodwill", 1000, 500)}}, &cn)
	e.must(200, adminTok, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, nil)
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(cn.ID)+"/email", nil, nil)
	note := e.mail.all()[0]
	if note.Subject != "Credit note CN-00001 from Example Seller Pty" || strings.Contains(note.Text, "Internal reason") || strings.Contains(note.Text, "Balance due") {
		t.Fatalf("%q\n%s", note.Subject, note.Text)
	}

	e.must(201, e.maker, "POST", "/v1/invoices/"+id(b)+"/payments", obj{"amount_minor": 1000}, nil)
	var m DocumentMail
	e.must(201, e.maker, "POST", "/v1/parties/"+id(e.cust)+"/statement/email", nil, &m)
	st := e.mail.all()[1]
	if m.Kind != DocStatement || m.InvoiceID != nil || len(billLinkRE.FindAllString(st.Text, -1)) != 2 {
		t.Fatalf("%+v\n%s", m, st.Text)
	}
	// 45.00 left of the credited one and 40.00 of the part-paid one.
	for _, want := range []string{"INV-00001", "INV-00002", "45.00 USD", "40.00 USD", "Total outstanding: 85.00 USD"} {
		if !strings.Contains(st.Text, want) {
			t.Errorf("statement lacks %q:\n%s", want, st.Text)
		}
	}
	var list mailList
	e.must(200, adminTok, "GET", "/v1/parties/"+id(e.cust)+"/emails", nil, &list)
	if len(list.Items) != 2 {
		t.Fatalf("%+v", list)
	}
}

func TestDunningScheduleAndOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.db")
	same := func(c *Config, _ *Options) { c.DBPath = path }
	e := newMailEnv(t, same)
	n := e.sentInvoice(0) // due 2026-11-25
	e.days(40)
	if e.run() != 0 {
		t.Fatal("reminders are off until someone turns them on")
	}
	e.now = e.now.Add(-40 * 24 * time.Hour)
	e.schedule(3, 10, 20)
	e.days(2)
	if e.run() != 0 {
		t.Fatal("not due yet")
	}
	e.days(1)
	if e.run() != 1 || e.run() != 0 {
		t.Fatal("step 1 once")
	}
	// A restart does not repeat it.
	if err := e.app.Close(); err != nil {
		t.Fatal(err)
	}
	mail, now, maker := e.mail, e.now, e.maker
	e.env = newEnv(t, same, func(_ *Config, o *Options) { o.Mailer = mail })
	e.now, e.maker = now, maker
	if e.run() != 0 {
		t.Fatal("repeated after a restart")
	}
	e.days(7)
	if e.run() != 1 || e.run() != 0 {
		t.Fatal("step 2 once")
	}
	e.days(10)
	if e.run() != 1 {
		t.Fatal("step 3")
	}
	e.days(30)
	if e.run() != 0 {
		t.Fatal("capped at the number of steps")
	}
	sent := e.mail.all()
	if len(sent) != 3 || sent[0].Subject != "Payment reminder: invoice INV-00001 from Example Seller Pty" || !strings.Contains(sent[0].Text, "Balance due: 50.00 USD") {
		t.Fatalf("%d %+v", len(sent), sent)
	}
	got := e.mails(n)
	var steps []string
	for _, m := range got.Items {
		steps = append(steps, strconv.Itoa(m.Step))
	}
	if strings.Join(steps, ",") != "3,2,1" || !got.Reminders.Enabled {
		t.Fatalf("%v %+v", steps, got.Reminders)
	}
}

func TestDunningSendsOnlyTheLatestStepAndKeepsAGap(t *testing.T) {
	e := newMailEnv(t)
	n := e.sentInvoice(0)
	e.days(15)
	e.schedule(3, 10, 16)
	if e.run() != 1 {
		t.Fatal("one message, not the backlog")
	}
	if got := e.mails(n).Items; len(got) != 1 || got[0].Step != 2 {
		t.Fatalf("%+v", got)
	}
	e.days(1) // step 3 is reached, but a message left yesterday
	if e.run() != 0 {
		t.Fatal("too soon")
	}
	e.days(2)
	if e.run() != 1 {
		t.Fatal("step 3 after the gap")
	}
	// An old debt is left to a person.
	old := e.sentInvoice(0)
	e.days(91)
	if e.run() != 0 || len(e.mails(old).Items) != 0 {
		t.Fatal("older than max_overdue_days")
	}
}

func TestDunningNeverAfterPaymentVoidDisputeStopOrErasure(t *testing.T) {
	e := newMailEnv(t)
	e.schedule(1)
	paid, void, disputed, stopped, credited := e.sentInvoice(0), e.sentInvoice(0), e.sentInvoice(0), e.sentInvoice(0), e.sentInvoice(0)
	unsent := e.issued(0)
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(paid)+"/payments", obj{"amount_minor": 5000}, nil)
	e.must(200, adminTok, "POST", "/v1/invoices/"+id(void)+"/void", obj{"reason": "x"}, nil)
	e.must(200, e.maker, "PUT", "/v1/invoices/"+id(disputed)+"/reminders", obj{"state": "disputed"}, nil)
	e.must(200, e.maker, "PUT", "/v1/invoices/"+id(stopped)+"/reminders", obj{"state": "stopped"}, nil)
	var cn Invoice
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(credited)+"/credit-notes", obj{"reason": "all", "lines": []obj{line("all", 1000, 5000)}}, &cn)
	e.must(200, adminTok, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, nil)
	e.days(5)
	if n := e.run(); n != 0 || len(e.mail.all()) != 0 {
		t.Fatalf("%d reminders: %+v", n, e.mail.all())
	}
	if len(e.mails(unsent).Items) != 0 {
		t.Fatal("an invoice that never reached the customer is not chased")
	}
	if st := e.mails(disputed).Reminders; st.Hold != RemindersDisputed {
		t.Fatalf("%+v", st)
	}
	// A party-wide stop.
	e.must(200, e.maker, "PUT", "/v1/invoices/"+id(disputed)+"/reminders", obj{"state": "on"}, nil)
	e.must(200, e.maker, "PUT", "/v1/parties/"+id(e.cust)+"/reminders", obj{"state": "stopped"}, nil)
	if e.run() != 0 {
		t.Fatal("party stopped")
	}
	e.must(200, e.maker, "PUT", "/v1/parties/"+id(e.cust)+"/reminders", obj{"state": "on"}, nil)
	if e.run() != 1 {
		t.Fatal("the resolved dispute is chased again")
	}
	e.must(200, e.maker, "PUT", "/v1/invoices/"+id(stopped)+"/reminders", obj{"state": "on"}, nil)
	e.must(200, adminTok, "POST", "/v1/parties/"+id(e.cust)+"/erase", nil, nil)
	if e.run() != 0 {
		t.Fatal("erased")
	}
	e.must(422, e.maker, "PUT", "/v1/invoices/"+id(stopped)+"/reminders", obj{"state": "later"}, nil)
}

func TestDunningRetriesWithOneKey(t *testing.T) {
	e := newMailEnv(t)
	e.schedule(1)
	a, b := e.sentInvoice(0), e.sentInvoice(0)
	e.days(2)
	e.mail.fail(fmt.Errorf("%w: quota", modclient.ErrUnavailable))
	if e.run() != 0 {
		t.Fatal("service down")
	}
	// The sweep stops at the first refusal instead of spending a claim on every invoice.
	if x, y := e.mails(a).Items, e.mails(b).Items; len(x) != 1 || x[0].Status != DMRetry || len(y) != 0 {
		t.Fatalf("%+v %+v", x, y)
	}
	e.mail.fail(nil)
	if e.run() != 2 || len(e.mail.all()) != 2 {
		t.Fatalf("%+v", e.mail.all())
	}

	// A reply that is lost: the same key goes again and nothing is sent twice.
	c := e.sentInvoice(0)
	e.days(2)
	e.mail.lost = true
	e.run()
	e.mail.lost = false
	if e.run() != 1 || len(e.mail.all()) != 3 || e.mails(c).Items[0].Status != DMQueued {
		t.Fatalf("%d %+v", len(e.mail.all()), e.mails(c).Items)
	}
}

func TestDunningResumeAfterACrashAndPaymentInBetween(t *testing.T) {
	e := newMailEnv(t)
	e.schedule(1)
	a, b := e.sentInvoice(0), e.sentInvoice(0)
	e.days(2)
	// Claimed, then the process died before the call.
	claim := func(n int64) {
		if err := e.app.store.inTx(context.Background(), func(tx *sql.Tx) error {
			_, err := e.app.store.claimMail(context.Background(), tx, "can-finance", DocReminder, "x", &n, e.cust, 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	claim(a)
	claim(b)
	claim(b) // the step is claimed once
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(b)+"/payments", obj{"amount_minor": 5000}, nil)
	if e.run() != 1 || len(e.mail.all()) != 1 {
		t.Fatalf("%+v", e.mail.all())
	}
	if x := e.mails(b).Items; len(x) != 1 || x[0].Status != DMCancelled {
		t.Fatalf("paid before it left: %+v", x)
	}
	// A message nobody would take for three days is given up.
	c := e.sentInvoice(0)
	e.days(2)
	e.mail.fail(fmt.Errorf("%w: down", modclient.ErrUnavailable))
	e.run()
	e.days(4)
	e.mail.fail(nil)
	e.run()
	if x := e.mails(c).Items; len(x) != 1 || x[0].Status != DMFailed {
		t.Fatalf("%+v", x)
	}
}

func TestDunningPolicyValidationAndScope(t *testing.T) {
	e := newMailEnv(t)
	var p DunningPolicy
	e.must(200, e.maker, "GET", "/v1/dunning", nil, nil)
	for _, bad := range []obj{
		{"enabled": true, "days_after_due": []int{}}, {"enabled": true, "days_after_due": []int{5, 5}},
		{"enabled": true, "days_after_due": []int{1, 2, 3, 4, 5, 6}}, {"enabled": true, "days_after_due": []int{0}},
		{"enabled": true, "days_after_due": []int{30}, "max_overdue_days": 10},
		{"enabled": true, "days_after_due": []int{30}, "max_overdue_days": 4000},
	} {
		if code := e.do(adminTok, "PUT", "/v1/dunning", bad, nil); code != 422 {
			t.Errorf("%v: %d", bad, code)
		}
	}
	e.must(403, e.maker, "PUT", "/v1/dunning", obj{"enabled": true, "days_after_due": []int{1}}, nil)
	scoped := e.token("scoped-admin", []string{"finance.admin"}, []int64{e.cust}, nil, false)
	e.must(403, scoped, "PUT", "/v1/dunning", obj{"enabled": true, "days_after_due": []int{1}}, nil)
	e.must(200, adminTok, "GET", "/v1/dunning", nil, &p)
	if p.Enabled {
		t.Fatal("off by default")
	}
}

func TestMailRecordsAndErasureAndPurge(t *testing.T) {
	e := newMailEnv(t)
	n := e.sentInvoice(0)
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/email", nil, nil)
	e.must(201, e.maker, "POST", "/v1/parties/"+id(e.cust)+"/statement/email", nil, nil)
	// The table holds no personal data.
	rows, err := e.app.store.db.Query(`SELECT kind || status || nonce || core_id || created_by FROM document_mails`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s, "@") || strings.Contains(s, "Zebra") {
			t.Errorf("row holds %q", s)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	e.must(201, e.maker, "POST", "/v1/invoices/"+id(n)+"/payments", obj{"amount_minor": 5000}, nil)
	e.must(200, adminTok, "POST", "/v1/parties/"+id(e.cust)+"/erase", nil, nil)
	e.days(366 * 9)
	if _, err := e.app.Purge(context.Background()); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := e.app.store.db.QueryRow(`SELECT COUNT(*) FROM document_mails`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d rows left: %v", left, err)
	}
}

func TestUIMailAndReminders(t *testing.T) {
	m := &queueFake{}
	u := newUIEnv(t, func(_ *Config, o *Options) { o.Mailer = m })
	maker := u.login(t, u.token("maker", []string{"finance.write"}, nil, nil, false))
	admin := u.login(t, u.token("boss", []string{"finance.admin"}, nil, nil, false))
	party := u.party(adminTok, "Customer One", "customer")
	var inv Invoice
	u.must(201, adminTok, "POST", "/v1/invoices", obj{"party_id": party, "lines": []obj{line("work", 1000, 100)}}, &inv)
	boss2 := u.token("approver", []string{"finance.approve"}, nil, nil, false)
	u.must(200, boss2, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	page := "/invoices/" + id(inv.ID)

	mustHave(t, maker.get(page), "Email to customer", "Reminders for this invoice")
	w := maker.post(page+"/email", nil)
	status(t, w, 303)
	mustHave(t, maker.get(w.Header().Get("Location")), "Handed to the mail service", "Queued by the mail service")
	status(t, maker.post(page+"/reminders", url.Values{"state": {"disputed"}}), 303)
	mustHave(t, maker.get(page), "Disputed: no reminders")
	if got := maker.get(page + "/pdf"); got.Code != 200 || got.Header().Get("Content-Type") != "application/pdf" {
		t.Fatalf("%d", got.Code)
	}

	pp := "/parties/" + id(party)
	status(t, maker.post(pp+"/statement", nil), 303)
	status(t, maker.post(pp+"/reminders", url.Values{"state": {"stopped"}}), 303)
	mustHave(t, maker.get(pp), "Allow reminders for this customer", "Statement")

	status(t, maker.post("/settings/reminders", url.Values{"enabled": {"1"}, "days": {"7"}}), 403)
	status(t, admin.post("/settings/reminders", url.Values{"enabled": {"1"}, "days": {"seven"}}), 422)
	status(t, admin.post("/settings/reminders", url.Values{"enabled": {"1"}, "days": {"7, 21"}, "max_overdue_days": {"60"}}), 303)
	mustHave(t, admin.get("/settings/reminders"), `value="7, 21"`, `value="60"`)
	if len(m.all()) != 2 {
		t.Fatalf("%d", len(m.all()))
	}
}

func (e *mailEnv) countEvents(typ string) (n int) {
	e.t.Helper()
	if err := e.app.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE type = ?`, typ).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestInterruptedManualSendIsNotRetried(t *testing.T) {
	e := newMailEnv(t)
	n := e.sentInvoice(14)
	if err := e.app.store.inTx(context.Background(), func(tx *sql.Tx) error {
		_, err := e.app.store.claimMail(context.Background(), tx, "maker", DocDocument, "x", &n, e.cust, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.days(1)
	if got := e.mails(n).Items; len(got) != 1 || got[0].Status != DMUnknown || len(e.mail.all()) != 0 {
		t.Fatalf("%+v", got)
	}
}

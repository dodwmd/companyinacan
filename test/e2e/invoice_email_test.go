//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

type delivered struct{ subject, body string }

// mailFor returns what the SMTP sink holds for one recipient.
func mailFor(t *testing.T, address string) []delivered {
	t.Helper()
	r := web.Do(t, harness.Req{Host: "mail.localhost", Path: "/messages"}).Expect(t, "SMTP sink", 200)
	var all []sunk
	if err := json.Unmarshal([]byte(r.String()), &all); err != nil {
		t.Fatalf("sink messages: %v", err)
	}
	var out []delivered
	for _, m := range all {
		if len(m.To) != 1 || m.To[0] != address {
			continue
		}
		msg, err := mail.ReadMessage(strings.NewReader(m.Data))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, delivered{subject, strings.ReplaceAll(string(body), "\r\n", "\n")})
	}
	return out
}

// An invoice reaches a customer outside the company: finance -> can-core email.send (finance has
// external_recipients in the dev company.yaml) -> SMTP -> the sink, with a public link that shows
// the invoice and its PDF without a login. Then a statement, and a reminder for the overdue
// invoice, once.
func TestInvoiceEmailAndReminder(t *testing.T) {
	admin := financeAPI(t)
	u := harness.Unique()
	address := "billing-" + u + "@customer.example"
	party := admin.Post(t, "/v1/parties", map[string]any{"kind": "customer", "name": "E2E Mail " + u, "email": address}).Expect(t, "create party", 201).Str("id")

	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format(time.DateOnly) }
	inv := admin.Post(t, "/v1/invoices", map[string]any{
		"party_id": mustInt(t, party), "currency": "USD", "issue_date": day(-20), "due_date": day(-10),
		"lines": []map[string]any{{"description": "Rocket skates " + u, "quantity_milli": 1000, "unit_price_minor": 12300}},
	}).Expect(t, "create an overdue invoice", 201)
	id := inv.Str("id")
	approver := admin.WithToken(admin.Post(t, "/v1/tokens", map[string]any{"kind": "service", "subject": "e2e-mail-approver-" + u, "name": "e2e " + u, "scopes": []string{"finance.approve", "finance.read"}}).
		Expect(t, "mint approver token", 201, 200).Str("token"))
	approveInCanCore(t, admin, id, u)
	harness.Wait(t, "the approval decision to reach finance", time.Minute, func() bool {
		return approver.Post(t, "/v1/invoices/"+id+"/approve", nil).Status == 200
	})

	sent := admin.Post(t, "/v1/invoices/"+id+"/email", nil).Expect(t, "email the invoice", 201)
	if sent.Str("status") != "queued" {
		t.Fatalf("not queued: %s", sent.String())
	}
	var got []delivered
	harness.Wait(t, "the invoice at the SMTP sink", time.Minute, func() bool { got = mailFor(t, address); return len(got) == 1 })
	if !strings.HasPrefix(got[0].subject, "Invoice ") || !strings.Contains(got[0].body, "Balance due: 123.00 USD") {
		t.Fatalf("%q\n%s", got[0].subject, got[0].body)
	}
	var link string
	for _, line := range strings.Split(got[0].body, "\n") {
		if p, err := url.Parse(line); err == nil && strings.HasPrefix(p.Path, "/public/invoices/") && !strings.HasSuffix(p.Path, "/pdf") {
			link = p.Path
		}
	}
	if link == "" {
		t.Fatalf("no public link in %s", got[0].body)
	}
	page := web.Do(t, harness.Req{Host: "finance.localhost", Path: link}).Expect(t, "the mailed link", 200).String()
	if !strings.Contains(page, "Rocket skates "+u) || !strings.Contains(page, "Download PDF") {
		t.Fatalf("the public page: %s", page)
	}
	pdf := web.Do(t, harness.Req{Host: "finance.localhost", Path: link + "/pdf"}).Expect(t, "the PDF of the mailed link", 200)
	if !strings.HasPrefix(pdf.String(), "%PDF-1.4") || !strings.Contains(pdf.String(), "Rocket skates "+u) {
		t.Fatalf("not the invoice as PDF")
	}
	if after := admin.Get(t, "/v1/invoices/"+id).Expect(t, "read invoice", 200); after.Str("status") != "sent" {
		t.Fatalf("an emailed invoice is sent: %s", after.String())
	}
	harness.Wait(t, "the delivery status", time.Minute, func() bool {
		return strings.Contains(admin.Get(t, "/v1/invoices/"+id+"/emails").Expect(t, "delivery status", 200).String(), `"status":"sent"`)
	})

	admin.Post(t, "/v1/parties/"+party+"/statement/email", nil).Expect(t, "email a statement", 201)
	harness.Wait(t, "the statement at the SMTP sink", time.Minute, func() bool { got = mailFor(t, address); return len(got) == 2 })

	// Reminders: off until turned on, and one per step however often the sweep runs. The schedule is
	// company-wide, so it is switched off again for the rest of the suite.
	if p := admin.Get(t, "/v1/dunning").Expect(t, "schedule", 200); strings.Contains(p.String(), `"enabled":true`) {
		t.Logf("reminders were left on by an earlier run")
	}
	t.Cleanup(func() {
		admin.Put(t, "/v1/dunning", map[string]any{"enabled": false, "days_after_due": []int{7, 14, 30}}).Expect(t, "reminders off again", 200)
	})
	admin.Put(t, "/v1/dunning", map[string]any{"enabled": true, "days_after_due": []int{5, 25}, "max_overdue_days": 40}).Expect(t, "turn reminders on", 200)
	admin.Post(t, "/v1/dunning/run", nil).Expect(t, "send due reminders", 200)
	admin.Post(t, "/v1/dunning/run", nil).Expect(t, "run again", 200)
	// The sweep takes at most 20 invoices at a time: on a stack with many older overdue ones, go on.
	harness.Wait(t, "the reminder at the SMTP sink", time.Minute, func() bool {
		admin.Post(t, "/v1/dunning/run", nil).Expect(t, "run", 200)
		got = mailFor(t, address)
		return len(got) >= 3
	})
	time.Sleep(3 * time.Second)
	got = mailFor(t, address)
	reminders := 0
	for _, m := range got {
		if strings.HasPrefix(m.subject, "Payment reminder: ") {
			reminders++
		}
	}
	if reminders != 1 || len(got) != 3 {
		t.Fatalf("want exactly one reminder, got %d of %d messages", reminders, len(got))
	}
}

//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

type sunk struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Data string   `json:"data"`
}

// A contact form post through the edge produces an email: forms -> can-core email.send -> outbox ->
// SMTP -> the in-repo sink. The recipient is the form's notify_email; the visitor is only Reply-To.
// The admin UI then shows the delivery with the address masked.
func TestFormSubmissionSendsEmail(t *testing.T) {
	tok := web.Do(t, harness.Req{Host: "localhost", Path: "/_forms/contact/token"}).Expect(t, "forms timing token", 200).Str("token")
	time.Sleep(3 * time.Second) // min_fill_seconds
	visitor := "visitor-" + harness.Unique() + "@visitor.example"
	web.Do(t, harness.Req{
		Method: "POST", Host: "localhost", Path: "/_forms/contact",
		Headers: map[string]string{"Origin": "http://localhost:" + env.Port},
		Body:    url.Values{"name": {"Mail Visitor"}, "email": {visitor}, "message": {"please email us back " + visitor}, "website": {""}, "_t": {tok}},
	}).Expect(t, "contact form POST", 200, 201, 303)

	var got *sunk
	for deadline := time.Now().Add(45 * time.Second); got == nil && time.Now().Before(deadline); time.Sleep(time.Second) {
		r := web.Do(t, harness.Req{Host: "mail.localhost", Path: "/messages"}).Expect(t, "SMTP sink", 200)
		var all []sunk
		if err := json.Unmarshal([]byte(r.String()), &all); err != nil {
			t.Fatalf("sink messages: %v", err)
		}
		for _, m := range all {
			if strings.Contains(m.Data, "Reply-To: <"+visitor+">") {
				got = &m
			}
		}
	}
	if got == nil {
		t.Fatalf("no email for the submission of %s arrived at the SMTP sink (docker compose logs core forms smtpsink)", visitor)
	}
	if !slices.Equal(got.To, []string{"sales@acme.example"}) || got.From != "no-reply@acme.example" {
		t.Fatalf("envelope from %q to %v, want the configured sender and notify_email only", got.From, got.To)
	}
	msg, err := mail.ReadMessage(strings.NewReader(got.Data))
	if err != nil {
		t.Fatalf("%v", err)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if s := msg.Header.Get("Subject"); s != "New Contact submission" || !strings.Contains(string(body), "please email us back "+visitor) {
		t.Fatalf("subject %q body %q", s, body)
	}

	s := harness.NewSession(env)
	adminLogin(t, s)
	page := s.Do(t, harness.Req{Host: "admin.localhost", Path: "/email"}).Expect(t, "email delivery page", 200).String()
	for _, want := range []string{"s***@acme.example", "Sent", "smtpsink"} {
		if !strings.Contains(page, want) {
			t.Errorf("the delivery page lacks %q", want)
		}
	}
	if strings.Contains(page, visitor) || strings.Contains(page, "sales@acme.example") {
		t.Errorf("the delivery page shows an address in full")
	}
}

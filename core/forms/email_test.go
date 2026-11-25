package forms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

type flakyMailer struct {
	fails int
	err   error
	got   []Email
}

func (m *flakyMailer) Send(_ context.Context, e Email) error {
	if m.fails > 0 {
		m.fails--
		return m.err
	}
	m.got = append(m.got, e)
	return nil
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestSendEmailRetriesWithOneSourceIDAndSplitsRecipients(t *testing.T) {
	m := &flakyMailer{fails: 2, err: fmt.Errorf("%w: HTTP 503", modclient.ErrUnavailable)}
	d := &Dispatcher{mailer: m, sleep: noSleep}
	var to []string
	for i := range maxEmailRecipients + 2 {
		to = append(to, fmt.Sprintf("a%d@acme.example", i))
	}
	n, err := d.sendEmail(context.Background(), Email{To: to, SourceID: "lead:1"})
	if err != nil || n != 4 || len(m.got) != 2 || len(m.got[0].To) != maxEmailRecipients || m.got[0].SourceID != "lead:1" || m.got[1].SourceID != "lead:1.1" || len(m.got[1].To) != 2 {
		t.Fatal(n, err, m.got)
	}

	refused := &flakyMailer{fails: 5, err: errors.New("email.send: status 403")}
	d.mailer = refused
	if n, err := d.sendEmail(context.Background(), Email{To: to[:1]}); err == nil || n != 1 {
		t.Fatal("a refusal must not be retried:", n, err)
	}
	down := &flakyMailer{fails: 5, err: modclient.ErrUnavailable}
	d.mailer = down
	if n, err := d.sendEmail(context.Background(), Email{To: to[:1]}); err == nil || n != 3 {
		t.Fatal(n, err)
	}
}

func TestNotificationOnlyGoesToConfiguredRecipients(t *testing.T) {
	f := &Form{ID: "c", Name: strings.Repeat("é", 300), NotifyEmail: []string{"sales@acme.example"}, Fields: []Field{{Name: "message", Type: "text"}}}
	l := &Lead{ID: "ld_1", Email: "victim@evil.example\r\nBcc: other@evil.example", Fields: map[string]string{"message": "x\x00y\x1b" + strings.Repeat("z", 2*maxEmailText)}}
	e := BuildNotification(f, l)
	if len(e.To) != 1 || e.To[0] != "sales@acme.example" || e.ReplyTo != "" {
		t.Fatalf("%v %q", e.To, e.ReplyTo)
	}
	if len(e.Subject) > maxEmailSubject || len(e.Text) > maxEmailText+200 || strings.ContainsAny(e.Text, "\x00\x1b") || !strings.Contains(e.Text, "Lead id: ld_1") || e.SourceID != "lead:ld_1" {
		t.Fatalf("subject %d, text %d, source %q", len(e.Subject), len(e.Text), e.SourceID)
	}
}

package forms

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

type conflictFirstPart struct{ got []Email }

func (m *conflictFirstPart) Send(_ context.Context, e Email) error {
	if e.To[0] == "p0@acme.example" {
		return fmt.Errorf("email.send: %w", modclient.NewStatusError(http.StatusConflict, http.Header{}, ""))
	}
	m.got = append(m.got, e)
	return nil
}

// QA of #52, 8: "already queued" for the first ten recipients says nothing about the others.
func TestAConflictOnOnePartStillSendsTheOthers(t *testing.T) {
	f := newQueueFixture(t)
	f.form.NotifyEmail = nil
	for i := range 25 {
		f.form.NotifyEmail = append(f.form.NotifyEmail, fmt.Sprintf("p%d@acme.example", i))
	}
	m := &conflictFirstPart{}
	f.d.mailer = m
	l := f.lead()
	f.d.deliverEmail(context.Background(), l, f.form)
	if len(m.got) != 2 || len(m.got[0].To) != 10 || len(m.got[1].To) != 5 {
		t.Fatalf("parts sent after the conflict: %+v", m.got)
	}
	if f.state(l.ID) != "" {
		t.Fatal(f.state(l.ID))
	}
}

// QA of #52, 9: one lead that cannot be read must not stop every other notification.
func TestAnUnreadableLeadDoesNotStallRetries(t *testing.T) {
	f := newQueueFixture(t)
	bad := f.lead()
	f.now = f.now.Add(time.Second)
	good := f.lead()
	if _, err := f.store.db.Exec(`UPDATE leads SET fields = 'not json' WHERE id = ?`, bad.ID); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(firstAttemptGrace + time.Second)
	f.retry()
	if len(f.m.got) != 1 || f.state(good.ID) != "" {
		t.Fatalf("%d sent, good lead %q", len(f.m.got), f.state(good.ID))
	}
	p, status, err := f.store.pendingEmail(context.Background(), bad.ID)
	if err != nil || status != "pending" || p.attempts != 1 {
		t.Fatalf("the unreadable lead waits like any failure: %q %+v %v", status, p, err)
	}
	f.now = f.now.Add(giveUpAfter)
	f.retry()
	if _, status, _ := f.store.pendingEmail(context.Background(), bad.ID); status != "failed" {
		t.Fatalf("not given up: %q", status)
	}
}

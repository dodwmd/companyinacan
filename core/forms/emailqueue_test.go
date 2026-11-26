package forms

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

type switchMailer struct {
	err  error
	got  []Email
	hits int
}

func (m *switchMailer) Send(_ context.Context, e Email) error {
	m.hits++
	if m.err != nil {
		return m.err
	}
	m.got = append(m.got, e)
	return nil
}

type queueFixture struct {
	t     *testing.T
	store *Store
	d     *Dispatcher
	m     *switchMailer
	form  *Form
	now   time.Time
}

func newQueueFixture(t *testing.T) *queueFixture {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &queueFixture{t: t, store: s, m: &switchMailer{}, now: time.Unix(1_800_000_000, 0),
		form: &Form{ID: "contact", Name: "Contact", NotifyEmail: []string{"sales@acme.example"}}}
	s.now = func() time.Time { return f.now }
	f.d = NewDispatcher(s, f.m, nil, nil)
	f.d.now, f.d.sleep = s.now, noSleep
	f.d.forms = func(id string) *Form {
		if id == f.form.ID {
			return f.form
		}
		return nil
	}
	return f
}

func (f *queueFixture) lead() *Lead {
	f.t.Helper()
	l := &Lead{FormID: "contact", Fields: map[string]string{"message": "hi"}, CreatedAt: f.now, RetentionUntil: f.now.Add(time.Hour), emailDue: f.now.Add(firstAttemptGrace)}
	if err := f.store.Insert(context.Background(), l); err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *queueFixture) state(id string) string {
	f.t.Helper()
	l, err := f.store.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return l.Notification
}

func (f *queueFixture) retry() {
	f.t.Helper()
	if err := f.d.RetryEmails(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func TestLeadNotificationSurvivesAnOutageAndHonoursRetryAfter(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	quota := fmt.Errorf("email.send: %w", modclient.NewStatusError(http.StatusTooManyRequests, http.Header{"Retry-After": {"7200"}}, ""))
	f.m.err = quota
	first, second := f.lead(), f.lead()
	f.d.deliverEmail(ctx, first, f.form)
	if f.m.hits != 1 || f.state(first.ID) != NotificationRetrying {
		t.Fatal("a Retry-After must not be retried at once:", f.m.hits, f.state(first.ID))
	}
	// The second lead's first attempt never happened (crash, full queue): the loop finds it.
	f.now = f.now.Add(firstAttemptGrace + time.Second)
	f.retry()
	if f.m.hits != 2 {
		t.Fatal(f.m.hits)
	}
	f.now = f.now.Add(time.Hour)
	f.retry()
	if f.m.hits != 2 {
		t.Fatal("asked again before Retry-After had passed:", f.m.hits)
	}
	// Days later, still down: still queued.
	f.m.err = modclient.ErrUnavailable
	f.now = f.now.Add(3 * 24 * time.Hour)
	f.retry()
	if f.state(first.ID) != NotificationRetrying || f.state(second.ID) != NotificationRetrying {
		t.Fatal("given up too early")
	}
	f.m.err = nil
	f.now = f.now.Add(2 * time.Hour)
	f.retry()
	if len(f.m.got) != 2 || f.state(first.ID) != "" || f.state(second.ID) != "" {
		t.Fatal(len(f.m.got), f.state(first.ID), f.state(second.ID))
	}
	f.retry()
	if len(f.m.got) != 2 {
		t.Fatal("sent twice")
	}
	ds, err := f.store.Deliveries(ctx, first.ID)
	if err != nil || len(ds) != 2 || ds[0].OK || !ds[1].OK {
		t.Fatalf("history: first failure and final outcome: %+v %v", ds, err)
	}
}

func TestLeadNotificationFailuresAreShown(t *testing.T) {
	f := newQueueFixture(t)
	ctx := context.Background()
	refusal := func(status int) error { return modclient.NewStatusError(status, http.Header{}, "") }

	bad := f.lead()
	f.m.err = refusal(http.StatusBadRequest)
	f.d.deliverEmail(ctx, bad, f.form)
	if f.state(bad.ID) != NotificationFailed {
		t.Fatal("a malformed message cannot be retried:", f.state(bad.ID))
	}
	// Not allowed (yet): the operator can fix the allow-list, so it waits, for a week.
	denied := f.lead()
	f.m.err = refusal(http.StatusForbidden)
	f.d.deliverEmail(ctx, denied, f.form)
	if f.state(denied.ID) != NotificationRetrying {
		t.Fatal(f.state(denied.ID))
	}
	f.now = f.now.Add(giveUpAfter + time.Hour)
	f.retry()
	if f.state(denied.ID) != NotificationFailed {
		t.Fatal(f.state(denied.ID))
	}
	// can-core already has it (the recipients changed between attempts): done.
	dup := f.lead()
	f.m.err = refusal(http.StatusConflict)
	f.d.deliverEmail(ctx, dup, f.form)
	if f.state(dup.ID) != "" {
		t.Fatal(f.state(dup.ID))
	}
}

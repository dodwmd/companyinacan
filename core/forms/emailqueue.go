package forms

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The lead notification is kept until can-core has taken it (E03d): the row is written together with
// the lead, the first attempt follows at once, and anything that did not get through is retried from
// the stored lead with growing pauses. The source id makes every repeat safe.
const (
	// firstAttemptGrace keeps the retry loop off a lead whose first attempt is still under way.
	firstAttemptGrace = 2 * time.Minute
	// giveUpAfter is how long an undeliverable notification is retried before it is marked failed.
	giveUpAfter    = 7 * 24 * time.Hour
	retryInterval  = 30 * time.Second
	retryBatch     = 20
	maxRetryPause  = time.Hour
	outcomeTimeout = 5 * time.Second
)

// Notification states shown with a lead (empty: nothing to report).
const (
	NotificationRetrying = "retrying"
	NotificationFailed   = "failed"
)

// retryPause is the wait after the given failed round: one minute, doubling, at most an hour.
func retryPause(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		return maxRetryPause
	}
	return time.Minute << (attempt - 1)
}

type pendingEmail struct {
	leadID   string
	attempts int
	created  time.Time
}

func (s *Store) pendingEmail(ctx context.Context, leadID string) (pendingEmail, string, error) {
	p := pendingEmail{leadID: leadID}
	var status string
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT status, attempts, created_at FROM lead_email WHERE lead_id = ?`, leadID).Scan(&status, &p.attempts, &created)
	p.created = time.Unix(created, 0)
	return p, status, err
}

func (s *Store) dueEmails(ctx context.Context, now time.Time, limit int) ([]pendingEmail, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT lead_id, attempts, created_at FROM lead_email WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY next_attempt_at, created_at LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []pendingEmail
	for rows.Next() {
		var p pendingEmail
		var created int64
		if err := rows.Scan(&p.leadID, &p.attempts, &created); err != nil {
			return nil, err
		}
		p.created = time.Unix(created, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) setEmail(ctx context.Context, leadID, status string, attempts int, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE lead_email SET status = ?, attempts = ?, next_attempt_at = ? WHERE lead_id = ?`, status, attempts, next.Unix(), leadID)
	return err
}

// permanentEmailError reports a refusal that repeating cannot change: the message itself is wrong.
// Everything else (can-core down, quota, no relay yet, an address or module not yet allowed) can be
// put right, so it is retried.
func permanentEmailError(err error) bool {
	var se *modclient.StatusError
	return errors.As(err, &se) && (se.Status == http.StatusBadRequest || se.Status == http.StatusRequestEntityTooLarge)
}

// alreadyQueued reports that can-core has this lead's notification already (the form's recipients or
// name changed between two attempts).
func alreadyQueued(err error) bool {
	var se *modclient.StatusError
	return errors.As(err, &se) && se.Status == http.StatusConflict
}

// deliverEmail sends one lead's notification and records the outcome. It reports whether email.send
// was unavailable, and for how long it asked to be left alone.
func (d *Dispatcher) deliverEmail(ctx context.Context, l *Lead, f *Form) (unavailable bool, wait time.Duration) {
	p, status, err := d.store.pendingEmail(ctx, l.ID)
	queued := err == nil
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		d.log.Error("reading the notification state", "lead", l.ID, "error", err)
		return false, 0
	case queued && status != "pending":
		return false, 0
	}
	n, sendErr := d.sendEmail(ctx, BuildNotification(f, l))
	if sendErr != nil && ctx.Err() != nil {
		return true, 0 // shutting down: the row stays as it is and is retried
	}
	// The outcome is written even if ctx ends now.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outcomeTimeout)
	defer cancel()
	if alreadyQueued(sendErr) {
		sendErr = nil
	}
	if sendErr != nil {
		unavailable, wait = errors.Is(sendErr, modclient.ErrUnavailable), modclient.RetryAfter(sendErr)
	}
	if !queued {
		d.record(rctx, l.ID, "email", n, sendErr)
		return unavailable, wait
	}
	now := d.now()
	state, rounds := "sent", p.attempts+1
	next := now
	if sendErr != nil {
		state, next = "pending", now.Add(max(retryPause(rounds), wait))
		if permanentEmailError(sendErr) || now.Sub(p.created) >= giveUpAfter {
			state = "failed"
		}
	}
	if err := d.store.setEmail(rctx, l.ID, state, rounds, next); err != nil {
		d.log.Error("recording the notification state", "lead", l.ID, "error", err)
	}
	// The delivery history gets the first failure and the final outcome, not every retry.
	if rounds == 1 || state != "pending" {
		d.record(rctx, l.ID, "email", n, sendErr)
	}
	if state == "failed" {
		d.log.Error("lead notification given up; see the lead in the admin UI", "lead", l.ID)
	}
	return unavailable, wait
}

// RetryEmails sends the notifications that are due. It stops at the first sign that email.send is
// unavailable and honours its Retry-After.
func (d *Dispatcher) RetryEmails(ctx context.Context) error {
	if d.mailer == nil || d.now().Before(d.emailPausedUntil) {
		return nil
	}
	due, err := d.store.dueEmails(ctx, d.now(), retryBatch)
	if err != nil {
		return err
	}
	for _, p := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lead, err := d.store.Get(ctx, p.leadID)
		if err != nil {
			return err
		}
		form := d.forms(lead.FormID)
		if form == nil || len(form.NotifyEmail) == 0 {
			if err := d.store.setEmail(ctx, p.leadID, "failed", p.attempts, d.now()); err != nil {
				return err
			}
			d.record(ctx, p.leadID, "email", 0, errors.New("the form no longer has a notify_email"))
			continue
		}
		if unavailable, wait := d.deliverEmail(ctx, lead, form); unavailable {
			d.emailPausedUntil = d.now().Add(wait)
			return nil
		}
	}
	return nil
}

func (d *Dispatcher) retryLoop(ctx context.Context) {
	t := time.NewTicker(retryInterval)
	defer t.Stop()
	for {
		if err := d.RetryEmails(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("notification retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

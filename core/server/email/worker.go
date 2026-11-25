package email

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
)

// Delivery is at least once: a crash between the relay accepting a message and the row being updated
// sends it again, with the same Message-ID. There is exactly one worker (can-core is one replica).

// Backoff is the wait after the given failed attempt: one minute, doubling, at most one hour.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		return time.Hour
	}
	return time.Minute << (attempt - 1)
}

type due struct {
	id, source, recipient, replyTo, subject, body string
	attempts                                      int
}

// RunOnce expires mail that has waited too long and attempts every due message once, a batch at
// most. Without a usable relay nothing is attempted and no attempt is counted.
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	if err := s.expire(ctx); err != nil {
		return 0, err
	}
	p, err := s.source.Load()
	if err != nil {
		return 0, err
	}
	if p.RelayErr != nil {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, source, recipient, reply_to, subject, body, attempts FROM email_outbox
		WHERE status='pending' AND next_attempt_at<=? ORDER BY next_attempt_at, id LIMIT ?`, s.Now().Unix(), batchSize)
	if err != nil {
		return 0, err
	}
	var batch []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.source, &d.recipient, &d.replyTo, &d.subject, &d.body, &d.attempts); err != nil {
			_ = rows.Close()
			return 0, err
		}
		batch = append(batch, d)
	}
	// The database has one connection: close the rows before sending or writing.
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for i, d := range batch {
		if ctx.Err() != nil {
			return i, ctx.Err()
		}
		// Rules may have been tightened since the message was queued (a module turned off, an address
		// removed): the current ones decide what leaves.
		rules := p.Rules(d.source)
		if !rules.Enabled || (d.source != SystemSource && !config.RecipientAllowed(d.recipient, rules.Recipients)) {
			if err := s.record(ctx, d, &SendError{Stage: "policy check (the recipient is no longer allowed)", Permanent: true}); err != nil {
				return i, err
			}
			continue
		}
		msg := message{ID: d.id, From: p.Relay.From, To: d.recipient, ReplyTo: d.replyTo, Subject: d.subject, Text: d.body, Date: s.Now()}
		sendErr := s.transport.Send(ctx, p.Relay, d.recipient, msg.build())
		if sendErr != nil && ctx.Err() != nil {
			return i, ctx.Err() // shutting down: not an attempt
		}
		if err := s.record(ctx, d, sendErr); err != nil {
			return i, err
		}
	}
	return len(batch), nil
}

// record stores the outcome of one attempt together with its audit entry.
func (s *Service) record(ctx context.Context, d due, sendErr error) error {
	now := s.Now()
	attempts := d.attempts + 1
	status, action, result, reason := StatusSent, "email.sent", "sent", ""
	next := now
	if sendErr != nil {
		// Only the fixed text of a SendError is stored; anything else could carry data.
		reason = "delivery failed"
		var se *SendError
		permanent := false
		if errors.As(sendErr, &se) {
			reason, permanent = se.Error(), se.Permanent
		}
		status, action, result, next = StatusPending, "email.retry", "retry", now.Add(Backoff(attempts))
		if permanent || attempts >= MaxAttempts {
			status, action, result = StatusDead, "email.dead", "dead"
		}
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		finished := int64(0)
		if status != StatusPending {
			finished = now.Unix()
		}
		q := `UPDATE email_outbox SET status=?, attempts=?, last_error=?, next_attempt_at=?, finished_at=? WHERE id=? AND status='pending'`
		if status == StatusSent { // the content has done its job
			q = `UPDATE email_outbox SET status=?, attempts=?, last_error=?, next_attempt_at=?, finished_at=?, recipient='', reply_to='', subject='', body='' WHERE id=? AND status='pending'`
		}
		if _, err := tx.ExecContext(ctx, q, status, attempts, reason, next.Unix(), finished, d.id); err != nil {
			return err
		}
		detail := map[string]any{"source": d.source, "attempt": attempts}
		if reason != "" {
			detail["reason"] = reason
		}
		_, err := s.audit.AppendTx(ctx, tx, audit.Entry{Actor: "can-core", Action: action, Target: d.id, Detail: detail})
		return err
	})
	if err != nil {
		return err
	}
	messages.With(result).Inc()
	if status == StatusDead {
		s.log.Warn("email given up", "message", d.id, "source", d.source, "attempts", attempts, "reason", reason)
	}
	return nil
}

// expire gives up on mail that has been pending too long (for example while no relay was usable).
func (s *Service) expire(ctx context.Context) error {
	now := s.Now()
	cutoff := now.Add(-maxPendingAge).Unix()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_outbox WHERE status='pending' AND queued_at<?`, cutoff).Scan(&n); err != nil || n == 0 {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE email_outbox SET status='dead', last_error='expired before it could be sent', finished_at=? WHERE status='pending' AND queued_at<?`, now.Unix(), cutoff)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		messages.With("dead").Add(float64(c))
		_, err = s.audit.AppendTx(ctx, tx, audit.Entry{Actor: "can-core", Action: "email.expire", Target: "outbox", Detail: map[string]any{"count": c}})
		return err
	})
}

// Prune blanks the content of old dead letters and deletes old finished rows.
func (s *Service) Prune(ctx context.Context) error {
	now := s.Now()
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE email_outbox SET recipient='', reply_to='', subject='', body='' WHERE status='dead' AND finished_at<? AND recipient<>''`, now.Add(-deadContentTTL).Unix())
		if err != nil {
			return err
		}
		blanked, _ := res.RowsAffected()
		res, err = tx.ExecContext(ctx, `DELETE FROM email_outbox WHERE status<>'pending' AND finished_at<?`, now.Add(-rowTTL).Unix())
		if err != nil {
			return err
		}
		deleted, _ := res.RowsAffected()
		if blanked+deleted == 0 {
			return nil
		}
		_, err = s.audit.AppendTx(ctx, tx, audit.Entry{Actor: "can-core", Action: "email.prune", Target: "outbox", Detail: map[string]any{"blanked": blanked, "deleted": deleted}})
		return err
	})
}

// Run sends until ctx ends: on every new message, at least every 15 seconds for due retries, and
// prunes hourly.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	var pruned time.Time
	for {
		if time.Since(pruned) >= time.Hour {
			pruned = time.Now()
			if err := s.Prune(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("email outbox pruning failed", "error", err)
			}
		}
		for {
			n, err := s.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				s.log.Error("email delivery pass failed", "error", err)
			}
			if err != nil || n < batchSize {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-t.C:
		}
	}
}

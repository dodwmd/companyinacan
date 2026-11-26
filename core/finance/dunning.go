package finance

// Payment reminders to customers (specs/062-invoice-email). Off until someone turns them on. A
// reminder goes only for an invoice that was sent, is past its due date and still has a balance,
// and never once it is paid, credited in full, void, disputed or stopped, or for an erased party.
// Each step of the schedule is claimed once per invoice in document_mails before anything leaves.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Hold states of an invoice's reminders (and "on" or "stopped" for a party).
const (
	RemindersOn       = "on"
	RemindersStopped  = "stopped"
	RemindersDisputed = "disputed"
)

var holdStates = []string{RemindersOn, RemindersStopped, RemindersDisputed}

const (
	maxReminderSteps   = 5
	maxStepDays        = 365
	maxOverdueLimit    = 730
	defaultMaxOverdue  = 90
	reminderGap        = 3 * 24 * time.Hour // between two reminders for one invoice, whatever the schedule says
	reminderRetryLimit = 72 * time.Hour     // a reminder the mail service never took is given up after this
	reminderBatch      = 20                 // reminders per sweep: under can-core's default of 60 an hour
)

// DunningPolicy is the reminder schedule. Reminder n goes when the invoice is DaysAfterDue[n-1]
// days overdue; there are never more reminders than entries. Invoices overdue for longer than
// MaxOverdueDays are left to a person, so that turning reminders on does not chase old debts.
type DunningPolicy struct {
	Enabled        bool  `json:"enabled"`
	DaysAfterDue   []int `json:"days_after_due"`
	MaxOverdueDays int   `json:"max_overdue_days"`
}

func defaultDunning() DunningPolicy {
	return DunningPolicy{DaysAfterDue: []int{7, 14, 30}, MaxOverdueDays: defaultMaxOverdue}
}

func (p *DunningPolicy) validate() error {
	var pr problems
	if len(p.DaysAfterDue) < 1 || len(p.DaysAfterDue) > maxReminderSteps {
		pr.add("days_after_due", "1 to %d entries", maxReminderSteps)
	}
	last := 0
	for _, d := range p.DaysAfterDue {
		if d <= last || d > maxStepDays {
			pr.add("days_after_due", "increasing whole days between 1 and %d", maxStepDays)
			break
		}
		last = d
	}
	if p.MaxOverdueDays < last || p.MaxOverdueDays > maxOverdueLimit {
		pr.add("max_overdue_days", "at least the last reminder day and at most %d", maxOverdueLimit)
	}
	return pr.err()
}

func dunningPolicy(ctx context.Context, q dbq) (DunningPolicy, error) {
	p := defaultDunning()
	var days string
	err := q.QueryRowContext(ctx, `SELECT enabled, days_after_due, max_overdue_days FROM dunning_policy WHERE id = 1`).Scan(&p.Enabled, &days, &p.MaxOverdueDays)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.DaysAfterDue = nil
	return p, json.Unmarshal([]byte(days), &p.DaysAfterDue)
}

// Dunning returns the reminder schedule.
func (s *Store) Dunning(ctx context.Context) (DunningPolicy, error) { return dunningPolicy(ctx, s.db) }

// SetDunning replaces the schedule. It is company-wide, so a scoped token cannot change it.
func (s *Store) SetDunning(ctx context.Context, a *Principal, in DunningPolicy) (DunningPolicy, error) {
	if !a.Unscoped() {
		return in, ErrForbidden
	}
	if in.MaxOverdueDays == 0 {
		in.MaxOverdueDays = defaultMaxOverdue
	}
	if err := in.validate(); err != nil {
		return in, err
	}
	days, err := json.Marshal(in.DaysAfterDue)
	if err != nil {
		return in, err
	}
	return in, s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dunning_policy (id, enabled, days_after_due, max_overdue_days, updated_by, updated_at) VALUES (1, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET enabled = excluded.enabled, days_after_due = excluded.days_after_due, max_overdue_days = excluded.max_overdue_days, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
			in.Enabled, string(days), in.MaxOverdueDays, a.Subject, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "dunning.policy", "", map[string]any{"enabled": in.Enabled, "days_after_due": in.DaysAfterDue, "max_overdue_days": in.MaxOverdueDays})
	})
}

// ReminderState says whether reminders may go for an invoice (Hold) and its party.
type ReminderState struct {
	Hold         string `json:"hold"` // on, stopped or disputed; "on" for a party list
	PartyStopped bool   `json:"party_stopped"`
	Enabled      bool   `json:"enabled"` // the company's schedule
}

func (s *Store) reminderState(ctx context.Context, invoiceID, partyID int64) (*ReminderState, error) {
	st := &ReminderState{Hold: RemindersOn}
	if err := s.db.QueryRowContext(ctx, `SELECT reason FROM invoice_reminder_holds WHERE invoice_id = ?`, invoiceID).Scan(&st.Hold); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM party_reminder_stops WHERE party_id = ?)`, partyID).Scan(&st.PartyStopped); err != nil {
		return nil, err
	}
	p, err := s.Dunning(ctx)
	st.Enabled = p.Enabled
	return st, err
}

// SetInvoiceReminders stops reminders for an invoice, marks it disputed, or switches them on again.
func (s *Store) SetInvoiceReminders(ctx context.Context, a *Principal, id int64, state string) error {
	if state != RemindersOn && state != RemindersStopped && state != RemindersDisputed {
		return invalid("state", "on, stopped or disputed")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		v, err := s.loadInvoice(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if v.Kind != KindInvoice || v.Status == StDraft {
			return invalid("state", "reminders apply to issued invoices")
		}
		if state == RemindersOn {
			_, err = tx.ExecContext(ctx, `DELETE FROM invoice_reminder_holds WHERE invoice_id = ?`, id)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO invoice_reminder_holds (invoice_id, reason, set_by, set_at) VALUES (?, ?, ?, ?)
				ON CONFLICT (invoice_id) DO UPDATE SET reason = excluded.reason, set_by = excluded.set_by, set_at = excluded.set_at`, id, state, a.Subject, ts(s.now()))
		}
		if err != nil {
			return err
		}
		if state != RemindersOn {
			if _, err := tx.ExecContext(ctx, `UPDATE document_mails SET status = 'cancelled', updated_at = ? WHERE invoice_id = ? AND kind = 'reminder' AND status IN ('pending', 'retry')`, ts(s.now()), id); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, a.Subject, "invoice.reminders", deref(v.Number), map[string]any{"id": id, "state": state})
	})
}

// SetPartyReminders stops (or allows again) reminders for everything a party owes.
func (s *Store) SetPartyReminders(ctx context.Context, a *Principal, partyID int64, state string) error {
	if state != RemindersOn && state != RemindersStopped {
		return invalid("state", "on or stopped")
	}
	if err := s.wholeParty(a, partyID); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM parties WHERE id = ?`, partyID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var err error
		if state == RemindersOn {
			_, err = tx.ExecContext(ctx, `DELETE FROM party_reminder_stops WHERE party_id = ?`, partyID)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO party_reminder_stops (party_id, set_by, set_at) VALUES (?, ?, ?)`, partyID, a.Subject, ts(s.now()))
		}
		if err != nil {
			return err
		}
		if state != RemindersOn {
			if _, err := tx.ExecContext(ctx, `UPDATE document_mails SET status = 'cancelled', updated_at = ? WHERE party_id = ? AND kind = 'reminder' AND status IN ('pending', 'retry')`, ts(s.now()), partyID); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, a.Subject, "party.reminders", strconv.FormatInt(partyID, 10), map[string]any{"state": state})
	})
}

// remindable selects the invoices a reminder may be about. Arguments: today, and the oldest due
// date still chased.
const remindable = `FROM invoices i JOIN parties p ON p.id = i.party_id
	WHERE i.kind = 'invoice' AND i.status IN ('sent', 'partially_paid') AND i.total_minor - i.paid_minor - i.credited_minor > 0
	AND i.due_date < ? AND i.due_date >= ? AND p.erased_at IS NULL AND p.email <> ''
	AND NOT EXISTS (SELECT 1 FROM invoice_reminder_holds h WHERE h.invoice_id = i.id)
	AND NOT EXISTS (SELECT 1 FROM party_reminder_stops x WHERE x.party_id = i.party_id)`

func (s *Store) remindableArgs(p DunningPolicy) (today, oldest string) {
	now := s.now().UTC()
	return dateOf(now), dateOf(now.AddDate(0, 0, -p.MaxOverdueDays))
}

// reminderDue reports whether a reminder for the invoice may still leave.
func (s *Store) reminderDue(ctx context.Context, q dbq, invoiceID int64) (bool, error) {
	p, err := dunningPolicy(ctx, q)
	if err != nil || !p.Enabled {
		return false, err
	}
	today, oldest := s.remindableArgs(p)
	var n int
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) `+remindable+` AND i.id = ?`, today, oldest, invoiceID).Scan(&n)
	return n > 0, err
}

type dueReminder struct {
	invoiceID, partyID int64
	number             string
	step               int
}

// dueReminders picks the reminders to send now: for each invoice only the latest step its age has
// reached, so that a sweep after a long stop sends one message, not the whole schedule.
func (s *Store) dueReminders(ctx context.Context, p DunningPolicy) ([]dueReminder, error) {
	today, oldest := s.remindableArgs(p)
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.party_id, i.number_text, i.due_date,
		COALESCE((SELECT MAX(m.step) FROM document_mails m WHERE m.invoice_id = i.id AND m.kind = 'reminder'), 0),
		COALESCE((SELECT MAX(m.created_at) FROM document_mails m WHERE m.invoice_id = i.id AND m.kind = 'reminder'), 0) `+
		remindable+` ORDER BY i.due_date, i.id`, today, oldest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []dueReminder
	for len(out) < reminderBatch && rows.Next() {
		var d dueReminder
		var due string
		var done int
		var last int64
		if err := rows.Scan(&d.invoiceID, &d.partyID, &d.number, &due, &done, &last); err != nil {
			return nil, err
		}
		overdue := int(parseDate(today).Sub(parseDate(due)).Hours() / 24)
		for _, days := range p.DaysAfterDue {
			if overdue >= days {
				d.step++
			}
		}
		if d.step > done && (last == 0 || s.now().Sub(fromUnix(last)) >= reminderGap) {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// RunDunning sends the reminders that are due and returns how many the mail service accepted. It is
// safe to repeat and to interrupt. While the mail service cannot take messages it stops; they are
// tried again by the next sweep.
func (s *Store) RunDunning(ctx context.Context) (int, error) {
	if !s.MailReady() {
		return 0, nil
	}
	s.docs.dun.Lock()
	defer s.docs.dun.Unlock()
	p, err := s.Dunning(ctx)
	if err != nil || !p.Enabled {
		return 0, err
	}
	sent := 0
	deliver := func(id int64) (more bool, err error) {
		m, err := s.deliverMail(ctx, id)
		if err != nil {
			return false, err
		}
		if m.Status == DMQueued {
			sent++
		}
		return m.Status != DMRetry, nil
	}
	// First what an earlier sweep claimed and could not finish (a restart, or the service down).
	waiting, err := queryMails(ctx, s.db, `kind = 'reminder' AND status IN ('pending', 'retry') ORDER BY id LIMIT ?`, reminderBatch)
	if err != nil {
		return 0, err
	}
	for _, m := range waiting {
		if s.now().Sub(m.CreatedAt) > reminderRetryLimit {
			if err := s.inTx(ctx, func(tx *sql.Tx) error { return s.setMailStatus(ctx, tx, "can-finance", &m, DMFailed, "") }); err != nil {
				return sent, err
			}
			continue
		}
		if more, err := deliver(m.ID); err != nil || !more {
			return sent, err
		}
	}
	due, err := s.dueReminders(ctx, p)
	if err != nil {
		return sent, err
	}
	for _, d := range due {
		var m *DocumentMail
		if err := s.inTx(ctx, func(tx *sql.Tx) error {
			m = nil
			ok, err := s.reminderDue(ctx, tx, d.invoiceID)
			if err != nil || !ok {
				return err
			}
			m, err = s.claimMail(ctx, tx, "can-finance", DocReminder, d.number, &d.invoiceID, d.partyID, d.step)
			return err
		}); err != nil {
			return sent, err
		}
		if m == nil {
			continue
		}
		if more, err := deliver(m.ID); err != nil || !more {
			return sent, err
		}
	}
	return sent, nil
}

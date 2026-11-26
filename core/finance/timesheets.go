package finance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Timesheet statuses.
const (
	TsDraft     = "draft"
	TsSubmitted = "submitted"
	TsApproved  = "approved"
	TsRejected  = "rejected"
)

// Entry is time spent on a project, optionally on a task. Project and task are opaque keys
// owned by the `work` tracker; this service never calls it, so a key that no longer exists
// there is the caller's problem to notice.
type Entry struct {
	ID          int64  `json:"id"`
	Day         string `json:"day"`
	Project     string `json:"project"`
	Task        string `json:"task"`
	Minutes     int    `json:"minutes"`
	Billable    bool   `json:"billable"`
	Description string `json:"description"`
	InvoiceID   *int64 `json:"invoice_id"`
}

// EntryInput is one entry a client sends.
type EntryInput struct {
	Day         string `json:"day"`
	Project     string `json:"project"`
	Task        string `json:"task"`
	Minutes     int    `json:"minutes"`
	Billable    bool   `json:"billable"`
	Description string `json:"description"`
}

// Timesheet is a person's week.
type Timesheet struct {
	ID              int64      `json:"id"`
	Owner           string     `json:"owner"`
	WeekStart       string     `json:"week_start"`
	Status          string     `json:"status"`
	Entries         []Entry    `json:"entries"`
	TotalMinutes    int        `json:"total_minutes"`
	BillableMinutes int        `json:"billable_minutes"`
	SubmittedAt     *time.Time `json:"submitted_at"`
	DecidedBy       *string    `json:"decided_by"`
	DecidedAt       *time.Time `json:"decided_at"`
	RejectReason    string     `json:"reject_reason"`
	Version         int64      `json:"version"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	updatedBy, submittedBy string
}

const tsCols = `id, owner, week_start, status, submitted_at, decided_by, decided_at, reject_reason, version, created_by, created_at, updated_at, updated_by, submitted_by`

func scanTimesheet(sc scanner) (*Timesheet, error) {
	var t Timesheet
	var sub, dat sql.NullInt64
	var dby sql.NullString
	var c, u int64
	if err := sc.Scan(&t.ID, &t.Owner, &t.WeekStart, &t.Status, &sub, &dby, &dat, &t.RejectReason, &t.Version, &t.CreatedBy, &c, &u, &t.updatedBy, &t.submittedBy); err != nil {
		return nil, err
	}
	t.SubmittedAt, t.DecidedBy, t.DecidedAt = nullTime(sub), nullStr(dby), nullTime(dat)
	t.CreatedAt, t.UpdatedAt = fromUnix(c), fromUnix(u)
	t.Entries = []Entry{}
	return &t, nil
}

// isMonday reports whether a date string is a valid Monday.
func isMonday(s string) bool { return validDate(s) && parseDate(s).Weekday() == time.Monday }

// lockedThrough returns the latest locked date ("" when no period is locked).
func (s *Store) lockedThrough(ctx context.Context, q dbq) (string, error) {
	var d sql.NullString
	err := q.QueryRowContext(ctx, `SELECT through_date FROM period_locks ORDER BY id DESC LIMIT 1`).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return d.String, err
}

func (s *Store) checkUnlocked(ctx context.Context, q dbq, date string) error {
	through, err := s.lockedThrough(ctx, q)
	if err != nil {
		return err
	}
	if through != "" && date <= through {
		return fmt.Errorf("%w: records dated on or before %s are locked", ErrLocked, through)
	}
	return nil
}

// LockPeriod locks all timesheet weeks that start, and expenses incurred, on or before a
// date. The most recent lock is the one in force: setting an earlier date reopens the
// periods after it. Only finance.admin may, and every change is audited.
func (s *Store) LockPeriod(ctx context.Context, a *Principal, through, note string) error {
	if !validDate(through) || !oneLineText(note, 200) {
		return invalid("through_date", "use YYYY-MM-DD; note is one line")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO period_locks (through_date, locked_by, note, at) VALUES (?, ?, ?, ?)`, through, a.Subject, note, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "period.lock", through, map[string]any{"note": note})
	})
}

// PeriodLock reports the current lock.
func (s *Store) PeriodLock(ctx context.Context) (string, error) {
	return s.lockedThrough(ctx, s.rdb)
}

func (s *Store) tsScope(a *Principal, w *where) {
	c, args := ownerClause(a, "owner")
	w.scope(c, args)
	if len(a.Parties) > 0 {
		w.and("(1 = 0)") // a customer-scoped token has no business with people's time
	}
	if len(a.Projects) > 0 {
		c, args := projectClause(a, "project")
		w.and("(EXISTS (SELECT 1 FROM time_entries e WHERE e.timesheet_id = timesheets.id AND "+c+"))", args...)
	}
}

func (s *Store) loadEntries(ctx context.Context, q dbq, a *Principal, t *Timesheet) error {
	var w where
	w.and("(timesheet_id = ?)", t.ID)
	c, args := projectClause(a, "project")
	w.scope(c, args)
	rows, err := q.QueryContext(ctx, `SELECT id, day, project, task, minutes, billable, description, invoice_id FROM time_entries WHERE `+w.sql()+` ORDER BY day, id`, w.args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e Entry
		var inv sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Day, &e.Project, &e.Task, &e.Minutes, &e.Billable, &e.Description, &inv); err != nil {
			return err
		}
		if inv.Valid {
			e.InvoiceID = &inv.Int64
		}
		t.TotalMinutes += e.Minutes
		if e.Billable {
			t.BillableMinutes += e.Minutes
		}
		t.Entries = append(t.Entries, e)
	}
	return rows.Err()
}

func (s *Store) loadTimesheet(ctx context.Context, q dbq, a *Principal, id int64) (*Timesheet, error) {
	var w where
	w.and("(id = ?)", id)
	s.tsScope(a, &w)
	t, err := scanTimesheet(q.QueryRowContext(ctx, `SELECT `+tsCols+` FROM timesheets WHERE `+w.sql(), w.args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, s.loadEntries(ctx, q, a, t)
}

// GetTimesheet returns a timesheet in scope (entries filtered to the caller's projects).
func (s *Store) GetTimesheet(ctx context.Context, a *Principal, id int64) (*Timesheet, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	return s.loadTimesheet(rctx, s.rdb, a, id)
}

// TimesheetFilter selects timesheets.
type TimesheetFilter struct {
	Owner, Status, From, To string
}

// ListTimesheets lists timesheets in scope, newest week first.
func (s *Store) ListTimesheets(ctx context.Context, a *Principal, f TimesheetFilter, before int64, limit int) ([]Timesheet, int64, error) {
	limit = clampLimit(limit)
	var w where
	s.tsScope(a, &w)
	if before > 0 {
		w.and("(id < ?)", before)
	}
	if f.Owner != "" {
		w.and("(owner = ?)", f.Owner)
	}
	if f.Status != "" {
		w.and("(status = ?)", f.Status)
	}
	if f.From != "" {
		w.and("(week_start >= ?)", f.From)
	}
	if f.To != "" {
		w.and("(week_start <= ?)", f.To)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+tsCols+` FROM timesheets WHERE `+w.sql()+` ORDER BY id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	var out []Timesheet
	for rows.Next() {
		t, err := scanTimesheet(rows)
		if err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		out = append(out, *t)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	for i := range out {
		if err := s.loadEntries(rctx, s.rdb, a, &out[i]); err != nil {
			return nil, 0, err
		}
	}
	if out == nil {
		out = []Timesheet{}
	}
	return out, next, nil
}

// canActOn reports whether a may edit and submit a person's sheet: the owner, or an admin.
func canActOn(a *Principal, owner string) bool {
	return owner == a.Subject || (a.Can(PermAdmin) && !a.OwnOnly)
}

// CreateTimesheet opens the week (a Monday) for a person, or returns the one that exists.
// created reports whether it is new. Only the person or an admin opens a week.
func (s *Store) CreateTimesheet(ctx context.Context, a *Principal, owner, weekStart string) (*Timesheet, bool, error) {
	if owner == "" {
		owner = a.Subject
	}
	var p problems
	if !validSubject(owner) {
		p.add("owner", "not a valid person id")
	}
	if !isMonday(weekStart) {
		p.add("week_start", "must be a Monday, YYYY-MM-DD")
	}
	if err := p.err(); err != nil {
		return nil, false, err
	}
	if !canActOn(a, owner) {
		return nil, false, ErrNotFound
	}
	if len(a.Parties) > 0 {
		return nil, false, ErrForbidden
	}
	var out *Timesheet
	created := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.checkUnlocked(ctx, tx, weekStart); err != nil {
			return err
		}
		now := ts(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO timesheets (owner, week_start, created_by, updated_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (owner, week_start) DO NOTHING`, owner, weekStart, a.Subject, a.Subject, now, now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		created = n > 0
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM timesheets WHERE owner = ? AND week_start = ?`, owner, weekStart).Scan(&id); err != nil {
			return err
		}
		out, err = s.loadTimesheet(ctx, tx, &Principal{}, id)
		return err
	})
	return out, created, err
}

func (in *EntryInput) check(week string, p *problems, f string) {
	d, err := time.Parse(time.DateOnly, in.Day)
	ws := parseDate(week)
	if err != nil || d.Before(ws) || d.After(ws.AddDate(0, 0, 6)) {
		p.add(f+".day", "must be a date inside the week starting %s", week)
	}
	if !projectRE.MatchString(in.Project) {
		p.add(f+".project", "not a project key")
	}
	if in.Task != "" && !taskRE.MatchString(in.Task) {
		p.add(f+".task", "not an issue key like ACME-12")
	}
	if in.Minutes < 1 || in.Minutes > 1440 {
		p.add(f+".minutes", "between 1 and 1440")
	}
	if !oneLineText(in.Description, 500) {
		p.add(f+".description", "one line, at most 500 characters")
	}
}

// SetEntries replaces a draft or rejected sheet's entries.
func (s *Store) SetEntries(ctx context.Context, a *Principal, id int64, ifMatch *int64, in []EntryInput) (*Timesheet, error) {
	if len(in) > maxEntries {
		return nil, invalid("entries", "at most %d entries", maxEntries)
	}
	var out *Timesheet
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := s.loadTimesheet(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !canActOn(a, t.Owner) {
			return ErrNotFound
		}
		if ifMatch != nil && *ifMatch != t.Version {
			return ErrVersionMismatch
		}
		if t.Status != TsDraft && t.Status != TsRejected {
			return fmt.Errorf("%w: a %s timesheet cannot be edited", ErrConflict, t.Status)
		}
		if err := s.checkUnlocked(ctx, tx, t.WeekStart); err != nil {
			return err
		}
		var p problems
		perDay := map[string]int{}
		for i := range in {
			f := fmt.Sprintf("entries[%d]", i)
			in[i].check(t.WeekStart, &p, f)
			if len(a.Projects) > 0 && !a.CanProject(in[i].Project) {
				p.add(f+".project", "outside your project scope")
			}
			perDay[in[i].Day] += in[i].Minutes
		}
		for d, m := range perDay {
			if m > 1440 {
				p.add("entries", "%s has more than 24 hours", d)
			}
		}
		if err := p.err(); err != nil {
			return err
		}
		// A project-scoped token only replaces the entries it can see.
		if len(a.Projects) > 0 {
			c, args := projectClause(a, "project")
			_, err = tx.ExecContext(ctx, `DELETE FROM time_entries WHERE timesheet_id = ? AND `+c, append([]any{id}, args...)...)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM time_entries WHERE timesheet_id = ?`, id)
		}
		if err != nil {
			return err
		}
		for _, e := range in {
			if _, err := tx.ExecContext(ctx, `INSERT INTO time_entries (timesheet_id, day, project, task, minutes, billable, description) VALUES (?, ?, ?, ?, ?, ?, ?)`, id, e.Day, e.Project, e.Task, e.Minutes, e.Billable, e.Description); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE timesheets SET status = 'draft', updated_by = ?, version = version + 1, updated_at = ? WHERE id = ?`, a.Subject, ts(s.now()), id); err != nil {
			return err
		}
		out, err = s.loadTimesheet(ctx, tx, a, id)
		return err
	})
	return out, err
}

// SubmitTimesheet sends a sheet for approval.
func (s *Store) SubmitTimesheet(ctx context.Context, a *Principal, id int64, ifMatch *int64) (*Timesheet, error) {
	defer s.pump.Kick() // send the approval request now rather than at the next pass
	return s.withTimesheet(ctx, a, id, ifMatch, func(tx *sql.Tx, t *Timesheet) error {
		if !canActOn(a, t.Owner) {
			return ErrNotFound
		}
		if t.Status != TsDraft && t.Status != TsRejected {
			return fmt.Errorf("%w: the timesheet is %s", ErrConflict, t.Status)
		}
		if len(t.Entries) == 0 {
			return invalid("entries", "add at least one entry before submitting")
		}
		if err := s.checkUnlocked(ctx, tx, t.WeekStart); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE timesheets SET status = 'submitted', submitted_at = ?, submitted_by = ?, reject_reason = '', version = version + 1, updated_at = ? WHERE id = ?`, ts(s.now()), a.Subject, ts(s.now()), t.ID); err != nil {
			return err
		}
		if err := s.requestDecision(ctx, tx, apTimesheet, t.ID, 0, a.Subject); err != nil {
			return err
		}
		return s.emit(ctx, tx, EventTimesheetSubmitted, timesheetStepData{TimesheetID: t.ID, Owner: t.Owner})
	})
}

func (s *Store) withTimesheet(ctx context.Context, a *Principal, id int64, ifMatch *int64, fn func(tx *sql.Tx, t *Timesheet) error) (*Timesheet, error) {
	var out *Timesheet
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := s.loadTimesheet(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != t.Version {
			return ErrVersionMismatch
		}
		if err := fn(tx, t); err != nil {
			return err
		}
		out, err = s.loadTimesheet(ctx, tx, a, id)
		return err
	})
	return out, err
}

type timesheetApprovedData struct {
	TimesheetID     int64     `json:"timesheet_id"`
	Person          string    `json:"person"`
	WeekStart       string    `json:"week_start"`
	TotalMinutes    int       `json:"total_minutes"`
	BillableMinutes int       `json:"billable_minutes"`
	ApprovedAt      time.Time `json:"approved_at"`
}

// decideTimesheet is approve and reject: the same rules, so neither can skip a check.
func (s *Store) decideTimesheet(ctx context.Context, a *Principal, id int64, ifMatch *int64, approve bool, reason string) (*Timesheet, error) {
	if !approve && (strings.TrimSpace(reason) == "" || !oneLineText(reason, 500)) {
		return nil, invalid("reason", "required, one line, at most 500 characters")
	}
	return s.withTimesheet(ctx, a, id, ifMatch, func(tx *sql.Tx, t *Timesheet) error {
		// A project-scoped approver must be able to see every project the sheet touches.
		if len(a.Projects) > 0 {
			var total int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM time_entries WHERE timesheet_id = ?`, id).Scan(&total); err != nil {
				return err
			}
			if total != len(t.Entries) {
				return ErrNotFound
			}
		}
		if err := s.authorizeApproval(ctx, a, ApprovalRequest{Kind: "timesheet", Owner: t.Owner}, t.CreatedBy, t.updatedBy, t.submittedBy); err != nil {
			return err
		}
		if t.Status != TsSubmitted {
			return fmt.Errorf("%w: the timesheet is %s, not submitted", ErrConflict, t.Status)
		}
		if err := s.engineAllows(ctx, tx, apTimesheet, id, 0, a, approve); err != nil {
			return err
		}
		if err := s.applyTimesheet(ctx, tx, t, approve, a.Subject, reason); err != nil {
			return err
		}
		return s.withdrawDecision(ctx, tx, apTimesheet, id)
	})
}

// applyTimesheet records the decision on a submitted sheet: the one place for both the module's own
// routes and the engine's decisions. The caller has authorised it.
func (s *Store) applyTimesheet(ctx context.Context, tx *sql.Tx, t *Timesheet, approve bool, by, reason string) error {
	if err := s.checkUnlocked(ctx, tx, t.WeekStart); err != nil {
		return err
	}
	now := s.now()
	status := TsRejected
	if approve {
		status = TsApproved
	}
	if _, err := tx.ExecContext(ctx, `UPDATE timesheets SET status = ?, decided_by = ?, decided_at = ?, reject_reason = ?, version = version + 1, updated_at = ? WHERE id = ?`, status, by, ts(now), reason, ts(now), t.ID); err != nil {
		return err
	}
	if err := s.audit(ctx, tx, by, "timesheet."+status, fmt.Sprint(t.ID), map[string]any{"owner": t.Owner}); err != nil {
		return err
	}
	if err := s.emit(ctx, tx, EventTimesheetDecided, timesheetStepData{TimesheetID: t.ID, Owner: t.Owner}); err != nil {
		return err
	}
	if approve {
		return s.emit(ctx, tx, EventTimesheetApproved, timesheetApprovedData{TimesheetID: t.ID, Person: t.Owner, WeekStart: t.WeekStart, TotalMinutes: t.TotalMinutes, BillableMinutes: t.BillableMinutes, ApprovedAt: now.UTC()})
	}
	return nil
}

// ApproveTimesheet approves a submitted sheet. Nobody approves their own time, and neither
// does whoever created the sheet on someone's behalf.
func (s *Store) ApproveTimesheet(ctx context.Context, a *Principal, id int64, ifMatch *int64) (*Timesheet, error) {
	return s.decideTimesheet(ctx, a, id, ifMatch, true, "")
}

// RejectTimesheet sends a submitted sheet back to its owner with a reason.
func (s *Store) RejectTimesheet(ctx context.Context, a *Principal, id int64, ifMatch *int64, reason string) (*Timesheet, error) {
	return s.decideTimesheet(ctx, a, id, ifMatch, false, reason)
}

// ---- billing: approved billable time to a draft invoice -----------------------------------

// BillingRequest asks for a draft invoice built from approved, billable, not yet billed time.
type BillingRequest struct {
	PartyID     int64  `json:"party_id"`
	Project     string `json:"project"`
	From        string `json:"from"`
	To          string `json:"to"`
	RateMinor   int64  `json:"rate_minor"` // per hour
	Currency    string `json:"currency"`
	FXRate      string `json:"fx_rate"`
	TaxCode     string `json:"tax_code"`
	Series      string `json:"series"`
	DueDays     *int   `json:"due_days"`
	Description string `json:"description"`
}

// BillTime drafts an invoice from time. One line per task (or one general line), quantity
// in hours to the milli-hour: minutes are summed per task first and converted once, so
// rounding happens once per line. The entries are marked billed and cannot be billed
// twice; deleting or voiding the invoice releases them.
func (s *Store) BillTime(ctx context.Context, a *Principal, in BillingRequest, idemKey string) (*Invoice, bool, error) {
	var p problems
	if !projectRE.MatchString(in.Project) {
		p.add("project", "not a project key")
	}
	if !validDate(in.From) || !validDate(in.To) || in.To < in.From {
		p.add("from", "use YYYY-MM-DD dates with from <= to")
	}
	if in.RateMinor < 0 || in.RateMinor > maxMinor {
		p.add("rate_minor", "hourly rate in minor units")
	}
	if !oneLineText(in.Description, 200) {
		p.add("description", "one line, at most 200 characters")
	}
	if err := p.err(); err != nil {
		return nil, false, err
	}
	if a.OwnOnly || !a.CanProject(in.Project) {
		return nil, false, ErrNotFound
	}
	var out *Invoice
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "timesheet.bill", in, func() (*Invoice, error) {
			rows, err := tx.QueryContext(ctx, `SELECT e.id, e.task, e.minutes FROM time_entries e JOIN timesheets t ON t.id = e.timesheet_id
				WHERE t.status = 'approved' AND e.billable = 1 AND e.invoice_id IS NULL AND e.project = ? AND e.day >= ? AND e.day <= ? ORDER BY e.task, e.day, e.id LIMIT 5000`, in.Project, in.From, in.To)
			if err != nil {
				return nil, err
			}
			byTask := map[string]int64{}
			var ids []int64
			for rows.Next() {
				var id, mins int64
				var task string
				if err := rows.Scan(&id, &task, &mins); err != nil {
					_ = rows.Close()
					return nil, err
				}
				byTask[task] += mins
				ids = append(ids, id)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				return nil, invalid("project", "no approved, billable, unbilled time for %s between %s and %s", in.Project, in.From, in.To)
			}
			tasks := make([]string, 0, len(byTask))
			for t := range byTask {
				tasks = append(tasks, t)
			}
			sort.Strings(tasks)
			var lines []LineInput
			for _, task := range tasks {
				qty, _ := mulDivRound(byTask[task], 1000, 60) // minutes to milli-hours
				if qty < 1 {
					qty = 1
				}
				label := in.Description
				if label == "" {
					label = "Time on " + in.Project
				}
				if task != "" {
					label += " " + task
				}
				unit, tc := in.RateMinor, in.TaxCode
				lines = append(lines, LineInput{Description: label, QuantityMilli: qty, UnitPriceMinor: &unit, TaxCode: &tc})
			}
			ni := NewInvoice{PartyID: in.PartyID, Series: in.Series, Project: in.Project, Currency: in.Currency, FXRate: in.FXRate, DueDays: in.DueDays, Lines: lines,
				Notes: fmt.Sprintf("Time %s to %s", in.From, in.To)}
			invID, err := s.insertDraft(ctx, tx, a, a.Subject, KindInvoice, nil, ni)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx, `UPDATE time_entries SET invoice_id = ? WHERE id = ? AND invoice_id IS NULL`, invID, id); err != nil {
					return nil, err
				}
			}
			return s.loadInvoice(ctx, tx, a, invID)
		})
		return err
	})
	return out, replayed, err
}

package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// systemActor creates the drafts a schedule generates. Because it is not the person who
// set the schedule up, that person (or anyone) can approve the drafts without breaking
// separation of duties; the schedule's own creator is in the audit log.
const systemActor = "system:recurring"

// Recurring is a schedule that drafts an invoice every period. Drafts are never approved or
// sent automatically: a person reviews each one.
type Recurring struct {
	ID        int64       `json:"id"`
	PartyID   int64       `json:"party_id"`
	Name      string      `json:"name"`
	Currency  string      `json:"currency"`
	FXRate    string      `json:"fx_rate"`
	Project   string      `json:"project"`
	Series    string      `json:"series"`
	Lines     []LineInput `json:"lines"`
	Interval  string      `json:"interval"`
	NextDate  string      `json:"next_date"`
	EndDate   *string     `json:"end_date"`
	DueDays   int         `json:"due_days"`
	Notes     string      `json:"notes"`
	Status    string      `json:"status"`
	CreatedBy string      `json:"created_by"`
	Version   int64       `json:"version"`
}

// NewRecurring is the body of a schedule create.
type NewRecurring struct {
	PartyID  int64       `json:"party_id"`
	Name     string      `json:"name"`
	Currency string      `json:"currency"`
	FXRate   string      `json:"fx_rate"`
	Project  string      `json:"project"`
	Series   string      `json:"series"`
	Lines    []LineInput `json:"lines"`
	Interval string      `json:"interval"`
	Start    string      `json:"start_date"`
	End      *string     `json:"end_date"`
	DueDays  *int        `json:"due_days"`
	Notes    string      `json:"notes"`
}

const recCols = `id, party_id, name, currency, fx_rate_e8, project, series, lines, interval, next_date, end_date, due_days, notes, status, created_by, version`

func scanRecurring(sc scanner) (*Recurring, error) {
	var r Recurring
	var fx int64
	var lines string
	var end sql.NullString
	if err := sc.Scan(&r.ID, &r.PartyID, &r.Name, &r.Currency, &fx, &r.Project, &r.Series, &lines, &r.Interval, &r.NextDate, &end, &r.DueDays, &r.Notes, &r.Status, &r.CreatedBy, &r.Version); err != nil {
		return nil, err
	}
	r.FXRate, r.EndDate = FormatFX(fx), nullStr(end)
	return &r, json.Unmarshal([]byte(lines), &r.Lines)
}

// AddInterval advances a date by one period. Month and year steps clamp to the end of a
// shorter month (31 January + 1 month = 28 or 29 February) and do not remember the original
// day, so a schedule that starts on the 31st drifts to the 28th; start on days 1 to 28 for
// stable dates.
func AddInterval(d time.Time, interval string) time.Time {
	switch interval {
	case "weekly":
		return d.AddDate(0, 0, 7)
	case "quarterly":
		return addMonths(d, 3)
	case "yearly":
		return addMonths(d, 12)
	}
	return addMonths(d, 1)
}

func addMonths(d time.Time, n int) time.Time {
	y, m, day := d.Date()
	first := time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}

// CreateRecurring adds a schedule. Lines are checked now (prices, tax codes) and priced again
// each time a draft is generated, with the tax rates of that day.
func (s *Store) CreateRecurring(ctx context.Context, a *Principal, in NewRecurring) (*Recurring, error) {
	var p problems
	if !oneLineText(in.Name, maxName) || in.Name == "" {
		p.add("name", "required, one line")
	}
	switch in.Interval {
	case "weekly", "monthly", "quarterly", "yearly":
	default:
		p.add("interval", "weekly, monthly, quarterly or yearly")
	}
	if !validDate(in.Start) {
		p.add("start_date", "use YYYY-MM-DD")
	}
	if in.End != nil && (!validDate(*in.End) || *in.End < in.Start) {
		p.add("end_date", "use YYYY-MM-DD, not before start_date")
	}
	if in.Series != "" && !seriesRE.MatchString(in.Series) {
		p.add("series", "1 to 6 capital letters")
	}
	if in.Project != "" && !projectRE.MatchString(in.Project) {
		p.add("project", "not a project key")
	}
	days := 30
	if in.DueDays != nil {
		days = *in.DueDays
	}
	if days < 0 || days > 365 {
		p.add("due_days", "between 0 and 365")
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	if len(a.Projects) > 0 && !a.CanProject(in.Project) {
		return nil, invalid("project", "a project-scoped token must use one of its projects")
	}
	if in.Series == "" {
		in.Series = "INV"
	}
	var out *Recurring
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		party, err := s.loadPartyRow(ctx, tx, a, in.PartyID)
		if err != nil {
			return err
		}
		cur := in.Currency
		if cur == "" {
			cur = party.Currency
		}
		if cur == "" {
			cur = s.base
		}
		fx, err := s.resolveFX(cur, in.FXRate)
		if err != nil {
			return err
		}
		if _, _, err := s.priceLines(ctx, tx, cur, "lines", in.Lines); err != nil {
			return err
		}
		lj, _ := json.Marshal(in.Lines)
		now := ts(s.now())
		var end any
		if in.End != nil {
			end = *in.End
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO recurring (party_id, name, currency, fx_rate_e8, project, series, lines, interval, next_date, end_date, due_days, notes, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.PartyID, in.Name, cur, fx, in.Project, in.Series, string(lj), in.Interval, in.Start, end, days, in.Notes, a.Subject, now, now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if err := s.audit(ctx, tx, a.Subject, "recurring.create", fmt.Sprint(id), map[string]any{"party_id": in.PartyID, "interval": in.Interval}); err != nil {
			return err
		}
		out, err = scanRecurring(tx.QueryRowContext(ctx, `SELECT `+recCols+` FROM recurring WHERE id = ?`, id))
		return err
	})
	return out, err
}

// ListRecurring lists schedules in the caller's party scope.
func (s *Store) ListRecurring(ctx context.Context, a *Principal) ([]Recurring, error) {
	var w where
	c, args := partyClause(a, "party_id")
	w.scope(c, args)
	c, args = projectClause(a, "project")
	w.scope(c, args)
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+recCols+` FROM recurring WHERE `+w.sql()+` ORDER BY id LIMIT 500`, w.args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Recurring{}
	for rows.Next() {
		r, err := scanRecurring(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// SetRecurringStatus pauses, resumes or ends a schedule.
func (s *Store) SetRecurringStatus(ctx context.Context, a *Principal, id int64, status string) (*Recurring, error) {
	if status != "active" && status != "paused" && status != "ended" {
		return nil, invalid("status", "active, paused or ended")
	}
	var out *Recurring
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var w where
		w.and("(id = ?)", id)
		c, args := partyClause(a, "party_id")
		w.scope(c, args)
		c, args = projectClause(a, "project")
		w.scope(c, args)
		cur, err := scanRecurring(tx.QueryRowContext(ctx, `SELECT `+recCols+` FROM recurring WHERE `+w.sql(), w.args...))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status == "ended" {
			return fmt.Errorf("%w: the schedule has ended", ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recurring SET status = ?, version = version + 1, updated_at = ? WHERE id = ?`, status, ts(s.now()), id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "recurring."+status, fmt.Sprint(id), map[string]any{}); err != nil {
			return err
		}
		out, err = scanRecurring(tx.QueryRowContext(ctx, `SELECT `+recCols+` FROM recurring WHERE id = ?`, id))
		return err
	})
	return out, err
}

const maxCatchUp = 12 // periods drafted per schedule per run, so a long outage cannot flood

// RunRecurring drafts every invoice that is due on or before asOf and returns the new
// draft ids. Running it twice for the same date creates nothing the second time: each
// (schedule, period) is unique in the database, so overlapping runs cannot duplicate either.
func (s *Store) RunRecurring(ctx context.Context, asOf string) ([]int64, error) {
	if !validDate(asOf) {
		return nil, invalid("as_of", "use YYYY-MM-DD")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recurring WHERE status = 'active' AND next_date <= ? ORDER BY id LIMIT 500`, asOf)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var created []int64
	for _, id := range ids {
		var made []int64
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			r, err := scanRecurring(tx.QueryRowContext(ctx, `SELECT `+recCols+` FROM recurring WHERE id = ? AND status = 'active'`, id))
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			for i := 0; i < maxCatchUp && r.NextDate <= asOf && r.Status == "active"; i++ {
				due := r.DueDays
				in := NewInvoice{PartyID: r.PartyID, Series: r.Series, Project: r.Project, Currency: r.Currency, FXRate: r.FXRate,
					IssueDate: r.NextDate, DueDays: &due, Notes: r.Notes, Lines: r.Lines}
				invID, err := s.insertDraft(ctx, tx, nil, systemActor, KindInvoice, nil, in)
				if err != nil {
					// A schedule that cannot draft (erased party, retired tax code) is paused, not retried forever.
					var ie *InvalidError
					if errors.As(err, &ie) || errors.Is(err, ErrNotFound) {
						if _, uerr := tx.ExecContext(ctx, `UPDATE recurring SET status = 'paused', version = version + 1 WHERE id = ?`, id); uerr != nil {
							return uerr
						}
						return s.audit(ctx, tx, systemActor, "recurring.paused_on_error", fmt.Sprint(id), map[string]any{"error": err.Error()})
					}
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE invoices SET recurring_id = ?, recurring_period = ? WHERE id = ?`, id, r.NextDate, invID); err != nil {
					return err
				}
				made = append(made, invID)
				next := AddInterval(parseDate(r.NextDate), r.Interval).Format(time.DateOnly)
				status := "active"
				if r.EndDate != nil && next > *r.EndDate {
					status = "ended"
				}
				if _, err := tx.ExecContext(ctx, `UPDATE recurring SET next_date = ?, status = ?, version = version + 1, updated_at = ? WHERE id = ?`, next, status, ts(s.now()), id); err != nil {
					return err
				}
				r.NextDate, r.Status = next, status
			}
			return nil
		})
		if err != nil {
			return created, err
		}
		created = append(created, made...)
	}
	return created, nil
}

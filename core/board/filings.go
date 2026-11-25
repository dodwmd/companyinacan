package board

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ObligationInput adds a recurring or one-off filing obligation. Obligations are data the operator
// enters (what must be filed, with which authority, when first due, how often): no jurisdiction's
// filing rules are implemented. LeadDays are the reminder points before the due date (0 is the day).
type ObligationInput struct {
	Name       string `json:"name"`
	Authority  string `json:"authority"`
	FirstDueOn string `json:"first_due_on"`
	Recurrence string `json:"recurrence"` // once | monthly | quarterly | annual
	LeadDays   []int  `json:"lead_days"`
}

// Obligation is a filing obligation.
type Obligation struct {
	ID         string `json:"id"`
	EntityID   string `json:"entity_id"`
	Name       string `json:"name"`
	Authority  string `json:"authority,omitempty"`
	FirstDueOn string `json:"first_due_on"`
	Recurrence string `json:"recurrence"`
	LeadDays   []int  `json:"lead_days"`
	Active     bool   `json:"active"`
}

// Filing is one due date of an obligation.
type Filing struct {
	ID           string `json:"id"`
	ObligationID string `json:"obligation_id"`
	EntityID     string `json:"entity_id"`
	Name         string `json:"name"`
	DueOn        string `json:"due_on"`
	Status       string `json:"status"` // pending | filed | waived
	Overdue      bool   `json:"overdue"`
	DaysUntil    int    `json:"days_until"`
	FiledOn      string `json:"filed_on,omitempty"`
	DocRef       string `json:"doc_ref,omitempty"`
}

func parseLeadDays(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func joinInts(l []int) string {
	p := make([]string, len(l))
	for i, n := range l {
		p[i] = strconv.Itoa(n)
	}
	return strings.Join(p, ",")
}

func recurrenceMonths(r string) int {
	switch r {
	case "monthly":
		return 1
	case "quarterly":
		return 3
	case "annual":
		return 12
	}
	return 0
}

// addMonthsClamped adds months to an anchor date keeping the anchor's day of month, clamped to the
// last day of a shorter month (31 January + 1 month is 28 or 29 February, and 31 March again).
func addMonthsClamped(anchor time.Time, months int) time.Time {
	y, m, d := anchor.Date()
	total := int(m) - 1 + months
	y += total / 12
	m = time.Month(total%12 + 1)
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if d > last {
		d = last
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// CreateObligation adds an obligation and its first due date. secretary.
func (s *Service) CreateObligation(ctx context.Context, a Actor, entityID string, in ObligationInput) (*Obligation, error) {
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActFilingManage, f, "legal-entity:"+entityID); err != nil {
		return nil, err
	}
	if err := validLine("name", in.Name, 200, true); err != nil {
		return nil, err
	}
	if err := validLine("authority", in.Authority, 200, false); err != nil {
		return nil, err
	}
	if _, err := parseDate(in.FirstDueOn); err != nil {
		return nil, err
	}
	switch in.Recurrence {
	case "once", "monthly", "quarterly", "annual":
	default:
		return nil, invalid("recurrence must be once, monthly, quarterly or annual")
	}
	if in.LeadDays == nil {
		in.LeadDays = []int{30, 7, 0}
	}
	if len(in.LeadDays) > 8 {
		return nil, invalid("at most 8 reminder points")
	}
	for _, d := range in.LeadDays {
		if d < 0 || d > 366 {
			return nil, invalid("lead_days are 0 to 366 days before the due date")
		}
	}
	id := newID("obl")
	err = s.txAudit(ctx, a, "write", "legal-entity:"+entityID+"/obligations", nil, func(ctx context.Context, tx *sqlTx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO filing_obligations (id, entity_id, name, authority, first_due_on, recurrence, lead_days, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			id, entityID, in.Name, in.Authority, in.FirstDueOn, in.Recurrence, joinInts(in.LeadDays), canonSubject(a.Subject), s.now().Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO filings (id, obligation_id, entity_id, due_on, status, created_at) VALUES (?,?,?,?,'pending',?)`, newID("fil"), id, entityID, in.FirstDueOn, s.now().Unix())
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Obligation{ID: id, EntityID: entityID, Name: in.Name, Authority: in.Authority, FirstDueOn: in.FirstDueOn, Recurrence: in.Recurrence, LeadDays: in.LeadDays, Active: true}, nil
}

// ListFilings lists the due dates of an entity (status "" for all, or pending, filed, waived), soonest first.
func (s *Service) ListFilings(ctx context.Context, a Actor, entityID, status string) ([]Filing, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActRegisterRead, f) {
		s.auditDeny(ctx, a, "read", "legal-entity:"+entityID+"/filings", nil)
		return nil, ErrNotFound
	}
	if status != "" && status != "pending" && status != "filed" && status != "waived" {
		return nil, invalid("status must be pending, filed or waived")
	}
	q := `SELECT f.id, f.obligation_id, f.entity_id, o.name, f.due_on, f.status, IFNULL(f.filed_on,''), f.doc_ref FROM filings f JOIN filing_obligations o ON o.id = f.obligation_id WHERE f.entity_id = ?`
	args := []any{entityID}
	if status != "" {
		q += ` AND f.status = ?`
		args = append(args, status)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY f.due_on, f.id LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Filing{}
	today := s.today()
	for rows.Next() {
		var x Filing
		if err := rows.Scan(&x.ID, &x.ObligationID, &x.EntityID, &x.Name, &x.DueOn, &x.Status, &x.FiledOn, &x.DocRef); err != nil {
			return nil, err
		}
		x.DaysUntil, _ = daysBetween(today, x.DueOn)
		x.Overdue = x.Status == "pending" && x.DueOn < today
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "legal-entity:"+entityID+"/filings", nil); err != nil {
		return nil, err
	}
	return out, nil
}

// FilingResult records the outcome of a due date.
type FilingResult struct {
	Status  string `json:"status"` // filed | waived
	FiledOn string `json:"filed_on"`
	DocRef  string `json:"doc_ref"`
}

// MarkFiling records a due date as filed (with the date and the reference of the receipt) or waived,
// and creates the next due date of a recurring obligation. secretary or officer.
func (s *Service) MarkFiling(ctx context.Context, a Actor, filingID string, in FilingResult) (*Filing, error) {
	if err := validID("fil", filingID); err != nil {
		return nil, err
	}
	var entityID, obID, due, status, recurrence, first, name string
	err := s.db.QueryRowContext(ctx, `SELECT f.entity_id, f.obligation_id, f.due_on, f.status, o.recurrence, o.first_due_on, o.name FROM filings f JOIN filing_obligations o ON o.id = f.obligation_id WHERE f.id = ?`, filingID).
		Scan(&entityID, &obID, &due, &status, &recurrence, &first, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_, f, err := s.seeEntity(ctx, a, entityID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActFilingMark, f, "filing:"+filingID); err != nil {
		return nil, err
	}
	if in.Status != "filed" && in.Status != "waived" {
		return nil, invalid("status must be filed or waived")
	}
	if in.FiledOn == "" {
		in.FiledOn = s.today()
	}
	if _, err := parseDate(in.FiledOn); err != nil {
		return nil, err
	}
	if in.FiledOn > s.today() {
		return nil, invalid("filed_on cannot be in the future")
	}
	if err := validRef("doc_ref", in.DocRef, false); err != nil {
		return nil, err
	}
	if status != "pending" {
		return nil, conflict("the filing is already %s", status)
	}
	err = s.txAudit(ctx, a, "write", "filing:"+filingID, nil, func(ctx context.Context, tx *sqlTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE filings SET status = ?, filed_on = ?, filed_by = ?, doc_ref = ? WHERE id = ? AND status = 'pending'`, in.Status, in.FiledOn, canonSubject(a.Subject), in.DocRef, filingID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return conflict("the filing changed: reload it")
		}
		return s.ensureNextFiling(ctx, tx, obID, entityID, recurrence, first)
	})
	if err != nil {
		return nil, err
	}
	return &Filing{ID: filingID, ObligationID: obID, EntityID: entityID, Name: name, DueOn: due, Status: in.Status, FiledOn: in.FiledOn, DocRef: in.DocRef}, nil
}

// ensureNextFiling creates the next due date of a recurring obligation when none is pending. The dates
// come from the anchor (first due date) so a 31st never drifts to the 28th for good.
func (s *Service) ensureNextFiling(ctx context.Context, tx *sqlTx, obID, entityID, recurrence, first string) error {
	step := recurrenceMonths(recurrence)
	if step == 0 {
		return nil
	}
	var pending, total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), SUM(status = 'pending') FROM filings WHERE obligation_id = ?`, obID).Scan(&total, &pending); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	anchor, err := time.Parse(dateLayout, first)
	if err != nil {
		return err
	}
	next := addMonthsClamped(anchor, step*total)
	if next.Year() > maxYear {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO filings (id, obligation_id, entity_id, due_on, status, created_at) VALUES (?,?,?,?,'pending',?)`, newID("fil"), obID, entityID, next.Format(dateLayout), s.now().Unix())
	return err
}

// applyFilingsDue announces filings that are coming due: for each pending filing of an active obligation,
// the reminder point that has been reached and not yet announced is sent once as entity.filing.due.v1
// (ids and dates only); earlier points are marked sent with it so a sweep that was down does not repeat itself.
func (s *Service) applyFilingsDue(ctx context.Context) (int, error) {
	today := s.today()
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.obligation_id, f.entity_id, f.due_on, f.reminders_sent, o.lead_days FROM filings f JOIN filing_obligations o ON o.id = f.obligation_id
		WHERE f.status = 'pending' AND o.active = 1 ORDER BY f.due_on LIMIT 5000`)
	if err != nil {
		return 0, err
	}
	type item struct{ id, ob, ent, due, sent, leads string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.ob, &it.ent, &it.due, &it.sent, &it.leads); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	_ = rows.Close()
	n := 0
	for _, it := range items {
		days, err := daysBetween(today, it.due)
		if err != nil {
			continue
		}
		sent := map[int]bool{}
		for _, d := range parseLeadDays(it.sent) {
			sent[d] = true
		}
		best, reached := -1, []int{}
		for _, d := range parseLeadDays(it.leads) {
			if days <= d {
				reached = append(reached, d)
				if !sent[d] && (best == -1 || d < best) {
					best = d
				}
			}
		}
		if best == -1 {
			continue
		}
		for _, d := range reached {
			sent[d] = true
		}
		var all []int
		for d := range sent {
			all = append(all, d)
		}
		newSent := joinInts(sortedInts(all))
		err = s.tx(ctx, func(ctx context.Context, tx *sqlTx) error {
			res, err := tx.ExecContext(ctx, `UPDATE filings SET reminders_sent = ? WHERE id = ? AND reminders_sent = ? AND status = 'pending'`, newSent, it.id, it.sent)
			if err != nil {
				return err
			}
			if k, _ := res.RowsAffected(); k == 0 {
				return nil // another sweep announced it
			}
			n++
			return s.enqueue(ctx, tx, EventFilingDue, map[string]any{"filing_id": it.id, "obligation_id": it.ob, "entity_id": it.ent, "due_on": it.due, "days_before": best})
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func sortedInts(l []int) []int {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j] < l[j-1]; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
	return l
}

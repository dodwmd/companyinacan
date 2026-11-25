package people

// Time off (Epic E14b): leave types with accrual policy as data, public holiday calendars per
// location, blackout periods, requests (draft -> pending -> approved | rejected | cancelled) and
// an append-only balance ledger. Units are half days ("halves"). Entitlements and statutory rules
// differ by jurisdiction: this models what the operator configures and runs no rule engine. It is
// a tool, not legal, HR or payroll advice.
//
// Concurrency: the service holds one database connection, so every transaction is serialised;
// balance checks and ledger writes happen in the same transaction.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	maxSpanDays   = 366
	maxHalves     = 2000
	pastDaysLimit = 90
)

var (
	leaveTypeIDRe = regexp.MustCompile(`^lvt_[0-9a-f]{24}$`)
	leaveReqIDRe  = regexp.MustCompile(`^lvr_[0-9a-f]{24}$`)
	blackoutIDRe  = regexp.MustCompile(`^blk_[0-9a-f]{24}$`)
	leaveKeyRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	weekdayNames  = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
)

// ---- leave types ------------------------------------------------------------------------------

// LeaveTypeIn creates a leave type. CarryoverCapHalves nil means unlimited carry-over.
type LeaveTypeIn struct {
	Key                  string `json:"key"`
	Name                 string `json:"name"`
	Paid                 bool   `json:"paid"`
	Sensitive            bool   `json:"sensitive"`
	AccrualMode          string `json:"accrual_mode"` // none (untracked) | annual | monthly
	GrantHalves          int    `json:"grant_halves"`
	CarryoverCapHalves   *int   `json:"carryover_cap_halves"`
	AllowNegative        bool   `json:"allow_negative"`
	NegativeFloorHalves  int    `json:"negative_floor_halves"`
	IgnoresBlackouts     bool   `json:"ignores_blackouts"`
	SetsLeaveStatus      bool   `json:"sets_leave_status"`
	LeaveStatusMinHalves int    `json:"leave_status_min_halves"`
}

// LeaveType is a stored leave type.
type LeaveType struct {
	ID                   string `json:"id"`
	Key                  string `json:"key"`
	Name                 string `json:"name"`
	Paid                 bool   `json:"paid"`
	Sensitive            bool   `json:"sensitive"`
	Tracked              bool   `json:"tracked"`
	AccrualMode          string `json:"accrual_mode"`
	GrantHalves          int    `json:"grant_halves"`
	CarryoverCapHalves   int    `json:"carryover_cap_halves"` // -1 unlimited
	AllowNegative        bool   `json:"allow_negative"`
	NegativeFloorHalves  int    `json:"negative_floor_halves"`
	IgnoresBlackouts     bool   `json:"ignores_blackouts"`
	SetsLeaveStatus      bool   `json:"sets_leave_status"`
	LeaveStatusMinHalves int    `json:"leave_status_min_halves"`
	Active               bool   `json:"active"`
}

// LeaveTypePatch changes a leave type; nil means unchanged. The key never changes.
type LeaveTypePatch struct {
	Name                 *string `json:"name"`
	GrantHalves          *int    `json:"grant_halves"`
	CarryoverCapHalves   *int    `json:"carryover_cap_halves"`
	AllowNegative        *bool   `json:"allow_negative"`
	NegativeFloorHalves  *int    `json:"negative_floor_halves"`
	IgnoresBlackouts     *bool   `json:"ignores_blackouts"`
	SetsLeaveStatus      *bool   `json:"sets_leave_status"`
	LeaveStatusMinHalves *int    `json:"leave_status_min_halves"`
	Active               *bool   `json:"active"`
}

const leaveTypeCols = `id, key, name, paid, sensitive, accrual_mode, grant_halves, carryover_cap_halves, allow_negative, negative_floor_halves, ignores_blackouts, sets_leave_status, leave_status_min_halves, active`

func scanLeaveType(sc interface{ Scan(...any) error }) (*LeaveType, error) {
	var t LeaveType
	var paid, sens, neg, ign, setl, act int
	if err := sc.Scan(&t.ID, &t.Key, &t.Name, &paid, &sens, &t.AccrualMode, &t.GrantHalves, &t.CarryoverCapHalves, &neg, &t.NegativeFloorHalves, &ign, &setl, &t.LeaveStatusMinHalves, &act); err != nil {
		return nil, err
	}
	t.Paid, t.Sensitive, t.AllowNegative, t.IgnoresBlackouts, t.SetsLeaveStatus, t.Active = paid == 1, sens == 1, neg == 1, ign == 1, setl == 1, act == 1
	t.Tracked = t.AccrualMode != "none"
	return &t, nil
}

func (s *Service) loadLeaveType(ctx context.Context, q querier, id string) (*LeaveType, error) {
	if !leaveTypeIDRe.MatchString(id) {
		return nil, ErrNotFound
	}
	t, err := scanLeaveType(q.QueryRowContext(ctx, `SELECT `+leaveTypeCols+` FROM leave_types WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func (t *LeaveType) validate() error {
	if !leaveKeyRe.MatchString(t.Key) {
		return invalid("key must match %s", leaveKeyRe)
	}
	if err := validLine("name", t.Name, 80, true); err != nil {
		return err
	}
	if !slices.Contains([]string{"none", "annual", "monthly"}, t.AccrualMode) {
		return invalid("accrual_mode must be none, annual or monthly")
	}
	for name, v := range map[string]int{"grant_halves": t.GrantHalves, "negative_floor_halves": t.NegativeFloorHalves, "leave_status_min_halves": t.LeaveStatusMinHalves} {
		if v < 0 || v > maxHalves {
			return invalid("%s must be between 0 and %d (half days)", name, maxHalves)
		}
	}
	if t.CarryoverCapHalves < -1 || t.CarryoverCapHalves > maxHalves {
		return invalid("carryover_cap_halves must be -1 (unlimited) or between 0 and %d", maxHalves)
	}
	if t.AccrualMode == "none" && (t.GrantHalves != 0 || t.AllowNegative || t.NegativeFloorHalves != 0) {
		return invalid("an untracked type (accrual_mode none) has no grant or negative balance; use annual with a grant of 0 for manually granted leave")
	}
	if !t.AllowNegative && t.NegativeFloorHalves != 0 {
		return invalid("negative_floor_halves needs allow_negative")
	}
	if !t.SetsLeaveStatus && t.LeaveStatusMinHalves != 0 {
		return invalid("leave_status_min_halves needs sets_leave_status")
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CreateLeaveType defines a leave type and its accrual policy. hr or admin.
func (s *Service) CreateLeaveType(ctx context.Context, a Actor, in LeaveTypeIn) (*LeaveType, error) {
	if !Decide(a, ActLeaveConfig, Fact{}) {
		s.auditDeny(ctx, a, "write", "leave-type", nil)
		return nil, ErrForbidden
	}
	if in.AccrualMode == "" {
		in.AccrualMode = "none"
	}
	t := LeaveType{ID: newID("lvt"), Key: in.Key, Name: in.Name, Paid: in.Paid, Sensitive: in.Sensitive, AccrualMode: in.AccrualMode, GrantHalves: in.GrantHalves,
		CarryoverCapHalves: -1, AllowNegative: in.AllowNegative, NegativeFloorHalves: in.NegativeFloorHalves, IgnoresBlackouts: in.IgnoresBlackouts,
		SetsLeaveStatus: in.SetsLeaveStatus, LeaveStatusMinHalves: in.LeaveStatusMinHalves, Active: true}
	if in.CarryoverCapHalves != nil {
		t.CarryoverCapHalves = *in.CarryoverCapHalves
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	err := s.txAudit(ctx, a, "write", "leave-type:"+t.ID, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO leave_types (id, key, name, paid, sensitive, accrual_mode, grant_halves, carryover_cap_halves, allow_negative, negative_floor_halves, ignores_blackouts, sets_leave_status, leave_status_min_halves, active, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)`, t.ID, t.Key, t.Name, b2i(t.Paid), b2i(t.Sensitive), t.AccrualMode, t.GrantHalves, t.CarryoverCapHalves, b2i(t.AllowNegative),
			t.NegativeFloorHalves, b2i(t.IgnoresBlackouts), b2i(t.SetsLeaveStatus), t.LeaveStatusMinHalves, s.now().Unix())
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("a leave type with this key exists")
		}
		return nil, err
	}
	t.Tracked = t.AccrualMode != "none"
	return &t, nil
}

// UpdateLeaveType changes the policy of a leave type. Existing ledger entries are not rewritten;
// a changed grant applies to periods accrued from now on. hr or admin.
func (s *Service) UpdateLeaveType(ctx context.Context, a Actor, id string, p LeaveTypePatch) (*LeaveType, error) {
	if !Decide(a, ActLeaveConfig, Fact{}) {
		s.auditDeny(ctx, a, "write", "leave-type:"+id, nil)
		return nil, ErrForbidden
	}
	var out *LeaveType
	err := s.txAudit(ctx, a, "write", "leave-type:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		t, err := s.loadLeaveType(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Name != nil {
			t.Name = *p.Name
		}
		if p.GrantHalves != nil {
			t.GrantHalves = *p.GrantHalves
		}
		if p.CarryoverCapHalves != nil {
			t.CarryoverCapHalves = *p.CarryoverCapHalves
		}
		if p.AllowNegative != nil {
			t.AllowNegative = *p.AllowNegative
		}
		if p.NegativeFloorHalves != nil {
			t.NegativeFloorHalves = *p.NegativeFloorHalves
		}
		if p.IgnoresBlackouts != nil {
			t.IgnoresBlackouts = *p.IgnoresBlackouts
		}
		if p.SetsLeaveStatus != nil {
			t.SetsLeaveStatus = *p.SetsLeaveStatus
		}
		if p.LeaveStatusMinHalves != nil {
			t.LeaveStatusMinHalves = *p.LeaveStatusMinHalves
		}
		if p.Active != nil {
			t.Active = *p.Active
		}
		if err := t.validate(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE leave_types SET name=?, grant_halves=?, carryover_cap_halves=?, allow_negative=?, negative_floor_halves=?, ignores_blackouts=?, sets_leave_status=?, leave_status_min_halves=?, active=? WHERE id=?`,
			t.Name, t.GrantHalves, t.CarryoverCapHalves, b2i(t.AllowNegative), t.NegativeFloorHalves, b2i(t.IgnoresBlackouts), b2i(t.SetsLeaveStatus), t.LeaveStatusMinHalves, b2i(t.Active), id)
		out = t
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListLeaveTypes lists the leave types. Any staff member: the existence of a type is not private.
func (s *Service) ListLeaveTypes(ctx context.Context, a Actor) ([]LeaveType, error) {
	if !Decide(a, ActLeaveCalendar, Fact{}) {
		return nil, ErrForbidden
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaveTypeCols+` FROM leave_types ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LeaveType{}
	for rows.Next() {
		t, err := scanLeaveType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// ---- holiday calendars and blackouts -------------------------------------------------------------

// Holiday is a public holiday.
type Holiday struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

// CalendarIn defines (or replaces) the calendar of a location. WorkDays names the working days
// (mon..sun); empty means Monday to Friday.
type CalendarIn struct {
	Location string    `json:"location"`
	Name     string    `json:"name"`
	WorkDays []string  `json:"work_days"`
	Holidays []Holiday `json:"holidays"`
}

// Calendar is a stored holiday calendar.
type Calendar struct {
	ID       string    `json:"id"`
	Location string    `json:"location"`
	Name     string    `json:"name"`
	WorkDays []string  `json:"work_days"`
	Holidays []Holiday `json:"holidays"`
}

func locationKey(l string) string { return canonSubject(l) }

func daysMask(names []string) (int, error) {
	if len(names) == 0 {
		return 31, nil
	}
	m := 0
	for _, n := range names {
		i := slices.Index(weekdayNames, strings.ToLower(n))
		if i < 0 {
			return 0, invalid("work_days entries must be among %s", strings.Join(weekdayNames, ", "))
		}
		m |= 1 << i
	}
	return m, nil
}

func maskDays(m int) []string {
	var out []string
	for i, n := range weekdayNames {
		if m&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// SaveCalendar creates or replaces the holiday calendar of a location. hr or admin. Holidays are
// data the operator maintains: nothing is derived from a jurisdiction.
func (s *Service) SaveCalendar(ctx context.Context, a Actor, in CalendarIn) (*Calendar, error) {
	if !Decide(a, ActLeaveConfig, Fact{}) {
		s.auditDeny(ctx, a, "write", "holiday-calendar", nil)
		return nil, ErrForbidden
	}
	key := locationKey(in.Location)
	if err := validLine("location", key, 100, true); err != nil {
		return nil, err
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	mask, err := daysMask(in.WorkDays)
	if err != nil {
		return nil, err
	}
	if len(in.Holidays) > 400 {
		return nil, invalid("at most 400 holidays per calendar")
	}
	seen := map[string]bool{}
	for _, h := range in.Holidays {
		if err := validDate(h.Date); err != nil {
			return nil, err
		}
		if seen[h.Date] {
			return nil, invalid("holiday %s is listed twice", h.Date)
		}
		seen[h.Date] = true
		if err := validLine("holiday name", h.Name, 100, true); err != nil {
			return nil, err
		}
	}
	id := newID("cal")
	err = s.txAudit(ctx, a, "write", "holiday-calendar:"+key, nil, func(ctx context.Context, tx *sqlTx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM holiday_calendars WHERE location = ?`, key).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id = newID("cal")
			if _, err := tx.ExecContext(ctx, `INSERT INTO holiday_calendars (id, location, name, work_days, created_at) VALUES (?,?,?,?,?)`, id, key, in.Name, mask, s.now().Unix()); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE holiday_calendars SET name = ?, work_days = ? WHERE id = ?`, in.Name, mask, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM holidays WHERE calendar_id = ?`, id); err != nil {
				return err
			}
		}
		for _, h := range in.Holidays {
			if _, err := tx.ExecContext(ctx, `INSERT INTO holidays (calendar_id, date, name) VALUES (?,?,?)`, id, h.Date, h.Name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	hs := append([]Holiday{}, in.Holidays...)
	return &Calendar{ID: id, Location: key, Name: in.Name, WorkDays: maskDays(mask), Holidays: hs}, nil
}

// ListCalendars lists the holiday calendars. Any staff member.
func (s *Service) ListCalendars(ctx context.Context, a Actor) ([]Calendar, error) {
	if !Decide(a, ActLeaveCalendar, Fact{}) {
		return nil, ErrForbidden
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, location, name, work_days FROM holiday_calendars ORDER BY location`)
	if err != nil {
		return nil, err
	}
	var out []Calendar
	for rows.Next() {
		var c Calendar
		var mask int
		if err := rows.Scan(&c.ID, &c.Location, &c.Name, &mask); err != nil {
			_ = rows.Close()
			return nil, err
		}
		c.WorkDays = maskDays(mask)
		out = append(out, c)
	}
	_ = rows.Close()
	if out == nil {
		out = []Calendar{}
	}
	for i := range out {
		hr, err := s.db.QueryContext(ctx, `SELECT date, name FROM holidays WHERE calendar_id = ? ORDER BY date`, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Holidays = []Holiday{}
		for hr.Next() {
			var h Holiday
			if err := hr.Scan(&h.Date, &h.Name); err != nil {
				_ = hr.Close()
				return nil, err
			}
			out[i].Holidays = append(out[i].Holidays, h)
		}
		_ = hr.Close()
	}
	return out, nil
}

// workWeek is the working calendar that applies to one person.
type workWeek struct {
	mask     int
	holidays map[string]bool
}

func (s *Service) workWeekFor(ctx context.Context, q querier, personID string) (*workWeek, error) {
	w := &workWeek{mask: 31, holidays: map[string]bool{}}
	var loc string
	if err := q.QueryRowContext(ctx, `SELECT location FROM people WHERE id = ?`, personID).Scan(&loc); err != nil {
		return nil, err
	}
	var id string
	err := q.QueryRowContext(ctx, `SELECT id, work_days FROM holiday_calendars WHERE location = ?`, locationKey(loc)).Scan(&id, &w.mask)
	if errors.Is(err, sql.ErrNoRows) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT date FROM holidays WHERE calendar_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		w.holidays[d] = true
	}
	return w, rows.Err()
}

func dayNum(date string) (int, error) {
	t, err := time.Parse(dateLayout, date)
	if err != nil {
		return 0, invalid("date %q must be YYYY-MM-DD", date)
	}
	return int(t.Unix() / 86400), nil
}

func dayDate(n int) string { return time.Unix(int64(n)*86400, 0).UTC().Format(dateLayout) }

// halves counts the working half days in the slot range [from, to] (slot = day*2 + 0 am or 1 pm).
func (w *workWeek) halves(from, to int) int {
	n := 0
	for d := from / 2; d <= to/2; d++ {
		wd := int(time.Unix(int64(d)*86400, 0).UTC().Weekday()+6) % 7 // Monday = 0
		if w.mask&(1<<wd) == 0 || w.holidays[dayDate(d)] {
			continue
		}
		for _, sl := range []int{d * 2, d*2 + 1} {
			if sl >= from && sl <= to {
				n++
			}
		}
	}
	return n
}

// BlackoutIn defines a period in which requests (other than types that ignore blackouts) are refused.
type BlackoutIn struct {
	Name      string `json:"name"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	TeamID    string `json:"team_id"`
}

// Blackout is a stored blackout period.
type Blackout struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	TeamID    string `json:"team_id,omitempty"`
}

// CreateBlackout adds a blackout period for the company or one team. hr or admin.
func (s *Service) CreateBlackout(ctx context.Context, a Actor, in BlackoutIn) (*Blackout, error) {
	if !Decide(a, ActLeaveConfig, Fact{}) {
		s.auditDeny(ctx, a, "write", "blackout", nil)
		return nil, ErrForbidden
	}
	if err := validLine("name", in.Name, 100, true); err != nil {
		return nil, err
	}
	d1, err := dayNum(in.StartDate)
	if err != nil {
		return nil, err
	}
	d2, err := dayNum(in.EndDate)
	if err != nil {
		return nil, err
	}
	if d2 < d1 || d2-d1 > maxSpanDays {
		return nil, invalid("end_date must be on or after start_date, within %d days", maxSpanDays)
	}
	id := newID("blk")
	err = s.txAudit(ctx, a, "write", "blackout:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		if in.TeamID != "" {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM teams WHERE id = ?`, in.TeamID).Scan(&n); err != nil || n == 0 {
				return invalid("unknown team")
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO blackouts (id, name, start_date, end_date, team_id, created_by, created_at) VALUES (?,?,?,?,?,?,?)`,
			id, in.Name, in.StartDate, in.EndDate, nullStr(in.TeamID), a.auditID(), s.now().Unix())
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Blackout{ID: id, Name: in.Name, StartDate: in.StartDate, EndDate: in.EndDate, TeamID: in.TeamID}, nil
}

// ListBlackouts lists blackout periods. Any staff member (they are announced rules).
func (s *Service) ListBlackouts(ctx context.Context, a Actor) ([]Blackout, error) {
	if !Decide(a, ActLeaveCalendar, Fact{}) {
		return nil, ErrForbidden
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, start_date, end_date, COALESCE(team_id,'') FROM blackouts ORDER BY start_date, id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Blackout{}
	for rows.Next() {
		var b Blackout
		if err := rows.Scan(&b.ID, &b.Name, &b.StartDate, &b.EndDate, &b.TeamID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBlackout removes a blackout period. Requests already accepted stay.
func (s *Service) DeleteBlackout(ctx context.Context, a Actor, id string) error {
	if !Decide(a, ActLeaveConfig, Fact{}) {
		s.auditDeny(ctx, a, "write", "blackout:"+id, nil)
		return ErrForbidden
	}
	if !blackoutIDRe.MatchString(id) {
		return ErrNotFound
	}
	return s.txAudit(ctx, a, "write", "blackout:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM blackouts WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---- requests -------------------------------------------------------------------------------------

// LeaveRequestIn is the body of a request. StartPart is full or pm (the first day starts at
// noon); EndPart is full or am (the last day ends at noon).
type LeaveRequestIn struct {
	TypeID    string `json:"type_id"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	StartPart string `json:"start_part"`
	EndPart   string `json:"end_part"`
	Note      string `json:"note"`
}

// LeaveRequest is a leave request as the caller may see it. Note and Type are for the person,
// their manager and hr.
type LeaveRequest struct {
	ID        string  `json:"id"`
	PersonID  string  `json:"person_id"`
	TypeID    string  `json:"type_id"`
	Type      string  `json:"type"`
	Status    string  `json:"status"`
	StartDate string  `json:"start_date"`
	EndDate   string  `json:"end_date"`
	StartPart string  `json:"start_part"`
	EndPart   string  `json:"end_part"`
	Halves    int     `json:"halves"`
	Days      float64 `json:"days"`
	Note      string  `json:"note,omitempty"`
	DecidedBy string  `json:"decided_by,omitempty"`
	CreatedAt string  `json:"created_at"`
}

type leaveRow struct {
	LeaveRequest
	startSlot, endSlot int
	noteSealed         sql.NullString
	approvalRef        string
	submittedBy        string
	statusApplied      int
	typeKey            string
}

const leaveCols = `r.id, r.person_id, r.type_id, t.key, r.status, r.start_date, r.end_date, r.start_part, r.end_part, r.start_slot, r.end_slot, r.halves, r.note_sealed, r.approval_ref, r.submitted_by, r.decided_by, r.status_applied, r.created_at`

func scanLeave(sc interface{ Scan(...any) error }) (*leaveRow, error) {
	var r leaveRow
	var at int64
	if err := sc.Scan(&r.ID, &r.PersonID, &r.TypeID, &r.typeKey, &r.Status, &r.StartDate, &r.EndDate, &r.StartPart, &r.EndPart, &r.startSlot, &r.endSlot, &r.Halves,
		&r.noteSealed, &r.approvalRef, &r.submittedBy, &r.DecidedBy, &r.statusApplied, &at); err != nil {
		return nil, err
	}
	r.Type, r.Days, r.CreatedAt = r.typeKey, float64(r.Halves)/2, ts(at)
	return &r, nil
}

func (s *Service) loadLeave(ctx context.Context, q querier, id string) (*leaveRow, error) {
	if !leaveReqIDRe.MatchString(id) {
		return nil, ErrNotFound
	}
	r, err := scanLeave(q.QueryRowContext(ctx, `SELECT `+leaveCols+` FROM leave_requests r JOIN leave_types t ON t.id = r.type_id WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func leaveAAD(id string) string { return "leave:" + id + ":note" }

// leaveFact resolves the caller's relationship to the request's person. A draft is the
// requester's alone. ErrNotFound unless the caller may know the request exists.
func (s *Service) leaveFact(ctx context.Context, q querier, a Actor, id string) (*leaveRow, Fact, error) {
	r, err := s.loadLeave(ctx, q, id)
	if err != nil {
		return nil, Fact{}, err
	}
	var f Fact
	if a.HasPerson {
		if f, _, err = s.factFor(ctx, q, a, r.PersonID); err != nil {
			return nil, f, err
		}
	}
	if !Decide(a, ActLeaveRead, f) || (r.Status == "draft" && !f.Self) {
		return nil, f, ErrNotFound
	}
	return r, f, nil
}

// leaveView converts a row for a viewer allowed to read it, opening the sealed note.
func (s *Service) leaveView(r *leaveRow, withNote bool) (*LeaveRequest, error) {
	v := r.LeaveRequest
	if withNote && r.noteSealed.Valid {
		plain, err := s.keys.Open(r.noteSealed.String, leaveAAD(r.ID))
		if err != nil {
			return nil, errors.New("cannot decrypt leave request")
		}
		v.Note = plain
	}
	return &v, nil
}

func (s *Service) leaveMutate(ctx context.Context, a Actor, action, id string, fields []string, fn func(ctx context.Context, tx *sqlTx) error) error {
	err := s.txAudit(ctx, a, action, "leave-request:"+id, fields, fn)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
		s.auditDeny(ctx, a, action, "leave-request:"+id, fields)
	}
	return err
}

func (in *LeaveRequestIn) normalise() (start, end int, err error) {
	if in.StartPart == "" {
		in.StartPart = "full"
	}
	if in.EndPart == "" {
		in.EndPart = "full"
	}
	if in.StartPart != "full" && in.StartPart != "pm" {
		return 0, 0, invalid("start_part must be full or pm")
	}
	if in.EndPart != "full" && in.EndPart != "am" {
		return 0, 0, invalid("end_part must be full or am")
	}
	d1, err := dayNum(in.StartDate)
	if err != nil {
		return 0, 0, err
	}
	d2, err := dayNum(in.EndDate)
	if err != nil {
		return 0, 0, err
	}
	if d2 < d1 || d2-d1 >= maxSpanDays {
		return 0, 0, invalid("end_date must be on or after start_date, within %d days", maxSpanDays)
	}
	start, end = d1*2, d2*2+1
	if in.StartPart == "pm" {
		start++
	}
	if in.EndPart == "am" {
		end--
	}
	if end < start {
		return 0, 0, invalid("the request covers no time")
	}
	return start, end, validText("note", in.Note, 1000, false)
}

func (s *Service) checkDateWindow(startSlot, endSlot int) error {
	today, _ := dayNum(s.today())
	if startSlot/2 < today-pastDaysLimit || endSlot/2 > today+730 {
		return invalid("dates must be within the last %d days and the next two years", pastDaysLimit)
	}
	return nil
}

// CreateLeaveRequest saves a draft for the caller's own leave.
func (s *Service) CreateLeaveRequest(ctx context.Context, a Actor, in LeaveRequestIn) (*LeaveRequest, error) {
	f := Fact{Self: true}
	if !Decide(a, ActLeaveWrite, f) {
		s.auditDeny(ctx, a, "write", "leave-request", nil)
		return nil, ErrForbidden
	}
	start, end, err := in.normalise()
	if err != nil {
		return nil, err
	}
	if err := s.checkDateWindow(start, end); err != nil {
		return nil, err
	}
	id := newID("lvr")
	err = s.txAudit(ctx, a, "write", "leave-request:"+id, []string{"note"}, func(ctx context.Context, tx *sqlTx) error {
		t, err := s.loadLeaveType(ctx, tx, in.TypeID)
		if err != nil || !t.Active {
			return invalid("unknown or inactive leave type")
		}
		sealed, err := s.sealNote(id, in.Note)
		if err != nil {
			return err
		}
		now := s.now().Unix()
		_, err = tx.ExecContext(ctx, `INSERT INTO leave_requests (id, person_id, type_id, status, start_date, end_date, start_part, end_part, start_slot, end_slot, note_sealed, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, a.PersonID, in.TypeID, "draft", in.StartDate, in.EndDate, in.StartPart, in.EndPart, start, end, sealed, now, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetLeaveRequest(ctx, a, id)
}

func (s *Service) sealNote(id, note string) (any, error) {
	if note == "" {
		return nil, nil
	}
	return s.keys.Seal(note, leaveAAD(id))
}

// UpdateLeaveRequest edits a draft. Only the requester.
func (s *Service) UpdateLeaveRequest(ctx context.Context, a Actor, id string, in LeaveRequestIn) (*LeaveRequest, error) {
	start, end, err := in.normalise()
	if err != nil {
		return nil, err
	}
	if err := s.checkDateWindow(start, end); err != nil {
		return nil, err
	}
	err = s.leaveMutate(ctx, a, "write", id, []string{"note"}, func(ctx context.Context, tx *sqlTx) error {
		r, f, err := s.leaveFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActLeaveWrite, f) {
			return ErrForbidden
		}
		if r.Status != "draft" {
			return conflict("only a draft can be edited (status is %s)", r.Status)
		}
		t, err := s.loadLeaveType(ctx, tx, in.TypeID)
		if err != nil || !t.Active {
			return invalid("unknown or inactive leave type")
		}
		sealed, err := s.sealNote(id, in.Note)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE leave_requests SET type_id=?, start_date=?, end_date=?, start_part=?, end_part=?, start_slot=?, end_slot=?, note_sealed=?, updated_at=? WHERE id=?`,
			in.TypeID, in.StartDate, in.EndDate, in.StartPart, in.EndPart, start, end, sealed, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetLeaveRequest(ctx, a, id)
}

// GetLeaveRequest returns a request to the person, their manager or hr. The read is audited
// before the data is returned (it carries the leave type and a note).
func (s *Service) GetLeaveRequest(ctx context.Context, a Actor, id string) (*LeaveRequest, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	r, f, err := s.leaveFact(ctx, s.db, a, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.auditDeny(ctx, a, "read", "leave-request:"+id, nil)
		}
		return nil, err
	}
	v, err := s.leaveView(r, true)
	if err != nil {
		return nil, err
	}
	_ = f
	if err := s.auditRead(ctx, a, "leave-request:"+id, []string{"type", "note"}); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	return v, nil
}

// LeaveFilter narrows ListLeaveRequests.
type LeaveFilter struct {
	PersonID string
	Status   string
}

// ListLeaveRequests lists the caller's own requests (drafts included), their direct reports'
// (live reporting line, drafts excluded) and, for hr, everyone's. Reads are audited per record
// that is not the caller's own; notes are not part of a list.
func (s *Service) ListLeaveRequests(ctx context.Context, a Actor, flt LeaveFilter) ([]LeaveRequest, error) {
	if a.blocked() {
		return []LeaveRequest{}, nil
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if flt.Status != "" && !slices.Contains([]string{"draft", "pending", "approved", "rejected", "cancelled"}, flt.Status) {
		return nil, invalid("unknown status")
	}
	q := `SELECT ` + leaveCols + ` FROM leave_requests r JOIN leave_types t ON t.id = r.type_id JOIN people p ON p.id = r.person_id WHERE 1=1`
	var args []any
	if flt.PersonID != "" {
		q += ` AND r.person_id = ?`
		args = append(args, flt.PersonID)
	}
	if flt.Status != "" {
		q += ` AND r.status = ?`
		args = append(args, flt.Status)
	}
	if !a.hrAdmin() {
		q += ` AND (r.person_id = ? OR p.manager_id = ?)`
		args = append(args, a.PersonID, a.PersonID)
	}
	q += ` ORDER BY r.start_slot, r.id LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var cands []*leaveRow
	for rows.Next() {
		r, err := scanLeave(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		cands = append(cands, r)
	}
	_ = rows.Close()
	out := []LeaveRequest{}
	var res []string
	for _, r := range cands {
		var f Fact
		if a.HasPerson {
			if f, _, err = s.factFor(ctx, s.db, a, r.PersonID); err != nil {
				continue
			}
		}
		if !Decide(a, ActLeaveRead, f) || (r.Status == "draft" && !f.Self) {
			continue
		}
		if !f.Self {
			res = append(res, "leave-request:"+r.ID)
		}
		v := r.LeaveRequest
		out = append(out, v)
	}
	if err := s.auditReadMany(ctx, a, res, []string{"type"}); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	return out, nil
}

func (s *Service) ledgerSum(ctx context.Context, q querier, personID, typeID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(halves),0) FROM leave_ledger WHERE person_id = ? AND type_id = ?`, personID, typeID).Scan(&n)
	return n, err
}

func (s *Service) pendingHalves(ctx context.Context, q querier, personID, typeID, exclude string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(halves),0) FROM leave_requests WHERE person_id = ? AND type_id = ? AND status = 'pending' AND id != ?`, personID, typeID, exclude).Scan(&n)
	return n, err
}

// checkBalance fails with a conflict when the type is tracked, does not allow the shortfall, and
// the balance (less other pending requests, when reserve is set) cannot cover halves.
func (s *Service) checkBalance(ctx context.Context, q querier, t *LeaveType, personID, reqID string, halves int, reserve bool) error {
	if !t.Tracked {
		return nil
	}
	bal, err := s.ledgerSum(ctx, q, personID, t.ID)
	if err != nil {
		return err
	}
	if reserve {
		p, err := s.pendingHalves(ctx, q, personID, t.ID, reqID)
		if err != nil {
			return err
		}
		bal -= p
	}
	floor := 0
	if t.AllowNegative {
		floor = -t.NegativeFloorHalves
	}
	if bal-halves < floor {
		return conflict("not enough %s balance: %.1f days available, %.1f requested", t.Key, float64(max(bal, 0))/2, float64(halves)/2)
	}
	return nil
}

// SubmitLeaveRequest moves a draft to pending: it computes the working days, refuses overlaps,
// blackouts and an insufficient balance, and registers the request with the approval engine when
// one is configured. Everything happens in one transaction, so concurrent submissions cannot
// together exceed the balance.
func (s *Service) SubmitLeaveRequest(ctx context.Context, a Actor, id string) (*LeaveRequest, error) {
	err := s.leaveMutate(ctx, a, "leave.submit", id, nil, func(ctx context.Context, tx *sqlTx) error {
		r, f, err := s.leaveFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActLeaveWrite, f) {
			return ErrForbidden
		}
		if r.Status != "draft" {
			return conflict("only a draft can be submitted (status is %s)", r.Status)
		}
		t, err := s.loadLeaveType(ctx, tx, r.TypeID)
		if err != nil {
			return err
		}
		if !t.Active {
			return conflict("this leave type is no longer in use")
		}
		if err := s.checkDateWindow(r.startSlot, r.endSlot); err != nil {
			return err
		}
		ww, err := s.workWeekFor(ctx, tx, r.PersonID)
		if err != nil {
			return err
		}
		halves := ww.halves(r.startSlot, r.endSlot)
		if halves == 0 {
			return invalid("the request contains no working days")
		}
		var clash int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM leave_requests WHERE person_id = ? AND id != ? AND status IN ('pending','approved') AND start_slot <= ? AND end_slot >= ?`,
			r.PersonID, id, r.endSlot, r.startSlot).Scan(&clash); err != nil {
			return err
		}
		if clash > 0 {
			return conflict("this overlaps another pending or approved request")
		}
		if !t.IgnoresBlackouts {
			var name string
			err := tx.QueryRowContext(ctx, `SELECT b.name FROM blackouts b WHERE b.start_date <= ? AND b.end_date >= ?
				AND (b.team_id IS NULL OR b.team_id = (SELECT team_id FROM people WHERE id = ?)) LIMIT 1`, r.EndDate, r.StartDate, r.PersonID).Scan(&name)
			if err == nil {
				return conflict("requests are not accepted during the blackout period %q", name)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if err := s.checkBalance(ctx, tx, t, r.PersonID, id, halves, true); err != nil {
			return err
		}
		ref := ""
		if s.approvals != nil {
			if ref, err = s.approvals.Request(ctx, ApprovalRequest{Type: "timeoff", Ref: id, Requester: canonSubject(a.Subject)}); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE leave_requests SET status='pending', halves=?, approval_ref=?, submitted_by=?, updated_at=? WHERE id=?`, halves, ref, canonSubject(a.Subject), s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetLeaveRequest(ctx, a, id)
}

func timeoffEvent(r *leaveRow, halves int) map[string]any {
	return map[string]any{"request_id": r.ID, "person_id": r.PersonID, "start_date": r.StartDate, "end_date": r.EndDate, "start_part": r.StartPart, "end_part": r.EndPart, "halves": halves}
}

// DecideLeaveRequest approves or rejects a pending request. The live direct manager decides (hr
// when there is none), never the requester: separation of duties is one function (sodOK) applied
// to the actor and, with an approval engine, to the engine's decider as well. A rejected request
// is final; the person files a new one.
func (s *Service) DecideLeaveRequest(ctx context.Context, a Actor, id string, approve bool) (*LeaveRequest, error) {
	var started bool
	err := s.leaveMutate(ctx, a, "leave.decide", id, nil, func(ctx context.Context, tx *sqlTx) error {
		r, f, err := s.leaveFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActLeaveDecide, f) {
			return ErrForbidden
		}
		if r.Status != "pending" {
			return conflict("only a pending request can be decided (status is %s)", r.Status)
		}
		now := s.now().Unix()
		if !approve {
			if s.approvals != nil && r.approvalRef != "" {
				_ = s.approvals.Cancel(ctx, r.approvalRef)
			}
			_, err := tx.ExecContext(ctx, `UPDATE leave_requests SET status='rejected', decided_by=?, decided_at=?, updated_at=? WHERE id=?`, a.auditID(), now, now, id)
			return err
		}
		chain := append(s.idents(ctx, tx, r.PersonID), r.submittedBy)
		approver := a
		if s.approvals != nil {
			st, err := s.approvals.Status(ctx, r.approvalRef)
			if err != nil {
				return err
			}
			if st.Outcome != "approved" {
				return conflict("the approval engine has not approved this request (outcome %q)", st.Outcome)
			}
			approver = Actor{Subject: st.DecidedBy, PersonID: s.personIDForSubject(ctx, tx, st.DecidedBy)}
		}
		if !sodOK(a, chain...) || !sodOK(approver, chain...) {
			return ErrSeparation
		}
		t, err := s.loadLeaveType(ctx, tx, r.TypeID)
		if err != nil {
			return err
		}
		ww, err := s.workWeekFor(ctx, tx, r.PersonID)
		if err != nil {
			return err
		}
		halves := ww.halves(r.startSlot, r.endSlot)
		if halves == 0 {
			return conflict("the request contains no working days any more")
		}
		if err := s.checkBalance(ctx, tx, t, r.PersonID, id, halves, false); err != nil {
			return err
		}
		if t.Tracked {
			if _, err := tx.ExecContext(ctx, `INSERT INTO leave_ledger (person_id, type_id, entry_date, kind, halves, ref, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
				r.PersonID, r.TypeID, r.StartDate, "use", -halves, id, approver.auditIDOrSubject(), now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leave_requests SET status='approved', halves=?, decided_by=?, decided_at=?, updated_at=? WHERE id=?`, halves, approver.auditIDOrSubject(), now, now, id); err != nil {
			return err
		}
		started = r.StartDate <= s.today()
		return s.enqueue(ctx, tx, EventTimeOffApproved, timeoffEvent(r, halves))
	})
	if err != nil {
		return nil, err
	}
	if started {
		if _, err := s.ReconcileLeaveStatus(ctx); err != nil {
			return nil, err
		}
	}
	return s.GetLeaveRequest(ctx, a, id)
}

// CancelLeaveRequest cancels a draft, pending or approved request. The requester may cancel
// until the leave starts; hr may cancel approved leave that has started, and only the days not yet
// taken are credited back. An approved cancellation appends a reversal to the ledger.
func (s *Service) CancelLeaveRequest(ctx context.Context, a Actor, id string) (*LeaveRequest, error) {
	err := s.leaveMutate(ctx, a, "leave.cancel", id, nil, func(ctx context.Context, tx *sqlTx) error {
		r, f, err := s.leaveFact(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !Decide(a, ActLeaveCancel, f) {
			return ErrForbidden
		}
		now := s.now().Unix()
		today, _ := dayNum(s.today())
		switch r.Status {
		case "draft", "pending":
			if s.approvals != nil && r.approvalRef != "" && r.Status == "pending" {
				_ = s.approvals.Cancel(ctx, r.approvalRef)
			}
		case "approved":
			if f.Self && r.startSlot/2 <= today {
				return conflict("leave that has started can only be cancelled by hr")
			}
			t, err := s.loadLeaveType(ctx, tx, r.TypeID)
			if err != nil {
				return err
			}
			if t.Tracked {
				from := max(r.startSlot, today*2)
				ww, err := s.workWeekFor(ctx, tx, r.PersonID)
				if err != nil {
					return err
				}
				if back := ww.halves(from, r.endSlot); back > 0 {
					if _, err := tx.ExecContext(ctx, `INSERT INTO leave_ledger (person_id, type_id, entry_date, kind, halves, ref, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
						r.PersonID, r.TypeID, s.today(), "reversal", back, id, a.auditID(), now); err != nil {
						return err
					}
				}
			}
			if err := s.enqueue(ctx, tx, EventTimeOffCancelled, map[string]any{"request_id": r.ID, "person_id": r.PersonID, "start_date": r.StartDate, "end_date": r.EndDate}); err != nil {
				return err
			}
		default:
			return conflict("a %s request cannot be cancelled", r.Status)
		}
		_, err = tx.ExecContext(ctx, `UPDATE leave_requests SET status='cancelled', cancelled_by=?, cancelled_at=?, updated_at=? WHERE id=?`, a.auditID(), now, now, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.ReconcileLeaveStatus(ctx); err != nil {
		return nil, err
	}
	return s.GetLeaveRequest(ctx, a, id)
}

// ---- balances and the ledger ------------------------------------------------------------------

// Balance is one person's balance of a tracked leave type.
type Balance struct {
	TypeID   string  `json:"type_id"`
	Type     string  `json:"type"`
	Halves   int     `json:"halves"`
	Days     float64 `json:"days"`
	Reserved int     `json:"pending_halves"`
	AsOf     string  `json:"as_of"`
}

// LeaveBalances returns the balances of a person as of a date (default today): the sum of the
// ledger entries dated on or before it. The person, their manager and hr only; audited.
func (s *Service) LeaveBalances(ctx context.Context, a Actor, personID, asOf string) ([]Balance, error) {
	if !personIDRe.MatchString(personID) {
		personID = absentPersonID
	}
	if asOf == "" {
		asOf = s.today()
	}
	if err := validDate(asOf); err != nil {
		return nil, err
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	f, _, err := s.factFor(ctx, s.db, a, personID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActLeaveBalance, f) {
		s.auditDeny(ctx, a, "read", "leave-balance:"+personID, nil)
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.key, COALESCE((SELECT SUM(halves) FROM leave_ledger l WHERE l.person_id = ? AND l.type_id = t.id AND l.entry_date <= ?),0)
		FROM leave_types t WHERE t.accrual_mode != 'none' ORDER BY t.key`, personID, asOf)
	if err != nil {
		return nil, err
	}
	var out []Balance
	for rows.Next() {
		var b Balance
		if err := rows.Scan(&b.TypeID, &b.Type, &b.Halves); err != nil {
			_ = rows.Close()
			return nil, err
		}
		b.Days, b.AsOf = float64(b.Halves)/2, asOf
		out = append(out, b)
	}
	_ = rows.Close()
	if out == nil {
		out = []Balance{}
	}
	for i := range out {
		if out[i].Reserved, err = s.pendingHalves(ctx, s.db, personID, out[i].TypeID, ""); err != nil {
			return nil, err
		}
	}
	if err := s.auditRead(ctx, a, "leave-balance:"+personID, []string{"balance"}); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	return out, nil
}

// LedgerEntry is one balance ledger entry.
type LedgerEntry struct {
	Seq       int64  `json:"seq"`
	TypeID    string `json:"type_id"`
	EntryDate string `json:"entry_date"`
	Kind      string `json:"kind"`
	Halves    int    `json:"halves"`
	Ref       string `json:"ref"`
	Note      string `json:"note,omitempty"`
}

// LeaveLedger returns the ledger of a person for one type (all types when typeID is empty).
func (s *Service) LeaveLedger(ctx context.Context, a Actor, personID, typeID string) ([]LedgerEntry, error) {
	if !personIDRe.MatchString(personID) {
		personID = absentPersonID
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	f, _, err := s.factFor(ctx, s.db, a, personID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActLeaveBalance, f) {
		s.auditDeny(ctx, a, "read", "leave-ledger:"+personID, nil)
		return nil, ErrNotFound
	}
	q := `SELECT seq, type_id, entry_date, kind, halves, ref, note_sealed FROM leave_ledger WHERE person_id = ?`
	args := []any{personID}
	if typeID != "" {
		q += ` AND type_id = ?`
		args = append(args, typeID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY seq LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LedgerEntry{}
	for rows.Next() {
		var e LedgerEntry
		var note sql.NullString
		if err := rows.Scan(&e.Seq, &e.TypeID, &e.EntryDate, &e.Kind, &e.Halves, &e.Ref, &note); err != nil {
			return nil, err
		}
		if note.Valid {
			if e.Note, err = s.keys.Open(note.String, "leave_ledger:"+e.Ref+":note"); err != nil {
				return nil, errors.New("cannot decrypt ledger note")
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "leave-ledger:"+personID, []string{"ledger"}); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	return out, nil
}

// AdjustLeave appends a manual adjustment (a correction, a grant, a payout) to a person's ledger.
// hr or admin, never for their own record. Entries are never edited: correct with another entry.
func (s *Service) AdjustLeave(ctx context.Context, a Actor, personID, typeID string, halves int, reason string) (*LedgerEntry, error) {
	f, si, err := s.factFor(ctx, s.db, a, personIDOrAbsent(personID))
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActLeaveAdjust, f) {
		s.auditDeny(ctx, a, "write", "leave-ledger:"+personID, nil)
		if Decide(a, ActDirRead, f) {
			return nil, ErrForbidden
		}
		return nil, ErrNotFound
	}
	if halves == 0 || halves > maxHalves || halves < -maxHalves {
		return nil, invalid("halves must be a non-zero number of half days, at most %d either way", maxHalves)
	}
	if err := validText("reason", reason, 500, true); err != nil {
		return nil, err
	}
	if si.erased || si.status == StFormer || si.status == StCandidate {
		return nil, conflict("the person is %s", si.status)
	}
	ref := newID("adj")
	var out *LedgerEntry
	err = s.txAudit(ctx, a, "leave.adjust", "leave-ledger:"+personID, []string{"balance"}, func(ctx context.Context, tx *sqlTx) error {
		t, err := s.loadLeaveType(ctx, tx, typeID)
		if err != nil {
			return invalid("unknown leave type")
		}
		if !t.Tracked {
			return conflict("%s is not tracked: set an accrual mode (annual with a grant of 0 for manual grants)", t.Key)
		}
		note, err := s.keys.Seal(reason, "leave_ledger:"+ref+":note")
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO leave_ledger (person_id, type_id, entry_date, kind, halves, ref, note_sealed, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			personID, typeID, s.today(), "adjust", halves, ref, note, a.auditID(), s.now().Unix())
		if err != nil {
			return err
		}
		seq, _ := res.LastInsertId()
		out = &LedgerEntry{Seq: seq, TypeID: typeID, EntryDate: s.today(), Kind: "adjust", Halves: halves, Ref: ref}
		return nil
	})
	return out, err
}

func personIDOrAbsent(id string) string {
	if personIDRe.MatchString(id) {
		return id
	}
	return absentPersonID
}

// ---- accruals ---------------------------------------------------------------------------------

// RunAccruals grants and accrues leave for the current year, applies carry-over caps at the turn
// of the year, and returns the number of ledger entries written. It is idempotent: every entry is
// keyed by person, type, kind and period. hr or admin; the service also runs it hourly.
func (s *Service) RunAccruals(ctx context.Context, a Actor) (int, error) {
	if !Decide(a, ActLeaveConfig, Fact{}) {
		s.auditDeny(ctx, a, "leave.accrue", "leave-ledger", nil)
		return 0, ErrForbidden
	}
	n, err := s.AccrueDue(ctx)
	if err != nil {
		return n, err
	}
	if err := s.auditWrite(ctx, a, "leave.accrue", "leave-ledger", nil); err != nil {
		return n, err
	}
	return n, nil
}

// AccrueDue is RunAccruals without the actor check (the background tick).
func (s *Service) AccrueDue(ctx context.Context) (int, error) {
	today := s.now().UTC()
	year := today.Year()
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaveTypeCols+` FROM leave_types WHERE active = 1 AND accrual_mode != 'none'`)
	if err != nil {
		return 0, err
	}
	var types []*LeaveType
	for rows.Next() {
		t, err := scanLeaveType(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		types = append(types, t)
	}
	_ = rows.Close()
	prows, err := s.db.QueryContext(ctx, `SELECT id, start_date FROM people WHERE status IN ('onboarding','active','leave','offboarding') AND erased_at IS NULL AND start_date != '' AND start_date <= ?`, s.today())
	if err != nil {
		return 0, err
	}
	type person struct{ id, start string }
	var people []person
	for prows.Next() {
		var p person
		if err := prows.Scan(&p.id, &p.start); err != nil {
			_ = prows.Close()
			return 0, err
		}
		people = append(people, p)
	}
	_ = prows.Close()
	total := 0
	for _, p := range people {
		err := s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
			for _, t := range types {
				n, err := s.accruePerson(ctx, tx, t, p.id, p.start, today, year)
				if err != nil {
					return err
				}
				total += n
			}
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Service) putLedger(ctx context.Context, tx *sqlTx, person, typ, date, kind string, halves int, ref string) (int, error) {
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO leave_ledger (person_id, type_id, entry_date, kind, halves, ref, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		person, typ, date, kind, halves, ref, "system", s.now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Service) accruePerson(ctx context.Context, tx *sqlTx, t *LeaveType, person, start string, today time.Time, year int) (int, error) {
	n := 0
	yearStart := fmt.Sprintf("%04d-01-01", year)
	// Carry-over: at the turn of the year, a balance above the cap expires. Done once per year,
	// before the new year's grant.
	if t.CarryoverCapHalves >= 0 && start < yearStart {
		var prior int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(halves),0) FROM leave_ledger WHERE person_id = ? AND type_id = ? AND entry_date < ?`, person, t.ID, yearStart).Scan(&prior); err != nil {
			return 0, err
		}
		if prior > t.CarryoverCapHalves {
			k, err := s.putLedger(ctx, tx, person, t.ID, yearStart, "expire", -(prior - t.CarryoverCapHalves), fmt.Sprint(year))
			if err != nil {
				return 0, err
			}
			n += k
		}
	}
	switch t.AccrualMode {
	case "annual":
		if t.GrantHalves > 0 {
			k, err := s.putLedger(ctx, tx, person, t.ID, yearStart, "grant", t.GrantHalves, fmt.Sprint(year))
			if err != nil {
				return 0, err
			}
			n += k
		}
	case "monthly":
		for m := 1; m <= int(today.Month()) && t.GrantHalves > 0; m++ {
			d := fmt.Sprintf("%04d-%02d-01", year, m)
			if start > time.Date(year, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Format(dateLayout) {
				continue // not employed yet in that month
			}
			k, err := s.putLedger(ctx, tx, person, t.ID, d, "accrual", t.GrantHalves, fmt.Sprintf("%04d-%02d", year, m))
			if err != nil {
				return 0, err
			}
			n += k
		}
	}
	return n, nil
}

// ---- the calendar ---------------------------------------------------------------------------------

// CalendarEntry is one absence on the calendar. Colleagues get Out only: no type, no reason, no
// request id, no status, no pending requests. The person, their manager and hr also get the
// type and the status.
type CalendarEntry struct {
	PersonID  string `json:"person_id"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	StartPart string `json:"start_part"`
	EndPart   string `json:"end_part"`
	Out       bool   `json:"out"`
	RequestID string `json:"request_id,omitempty"`
	Type      string `json:"type,omitempty"`
	Status    string `json:"status,omitempty"`
}

// LeaveCalendar lists who is out between two dates (at most 92 days) in a team (the caller's own
// team when teamID is empty; hr may ask for everyone by passing "all"). Approved leave shows as
// "out" like any other, whatever the type, so a long leave looks like a holiday: this matches the
// directory, which presents the leave status as active to colleagues.
func (s *Service) LeaveCalendar(ctx context.Context, a Actor, from, to, teamID string) ([]CalendarEntry, error) {
	if !Decide(a, ActLeaveCalendar, Fact{}) {
		return nil, ErrForbidden
	}
	d1, err := dayNum(from)
	if err != nil {
		return nil, err
	}
	d2, err := dayNum(to)
	if err != nil {
		return nil, err
	}
	if d2 < d1 || d2-d1 > 92 {
		return nil, invalid("the range must be at most 92 days")
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	q := `SELECT r.id, r.person_id, t.key, r.status, r.start_date, r.end_date, r.start_part, r.end_part, p.team_id
		FROM leave_requests r JOIN leave_types t ON t.id = r.type_id JOIN people p ON p.id = r.person_id
		WHERE r.status IN ('approved','pending') AND r.start_slot <= ? AND r.end_slot >= ? AND p.erased_at IS NULL`
	args := []any{d2*2 + 1, d1 * 2}
	switch {
	case teamID == "all" && a.hrAdmin():
	case teamID == "" || teamID == "all":
		if !a.HasPerson {
			return []CalendarEntry{}, nil
		}
		var mine sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT team_id FROM people WHERE id = ?`, a.PersonID).Scan(&mine)
		if !mine.Valid {
			return []CalendarEntry{}, nil
		}
		teamID = mine.String
		fallthrough
	default:
		q += ` AND p.team_id = ?`
		args = append(args, teamID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY r.start_slot, r.id LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	type cand struct {
		e    CalendarEntry
		team sql.NullString
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.e.RequestID, &c.e.PersonID, &c.e.Type, &c.e.Status, &c.e.StartDate, &c.e.EndDate, &c.e.StartPart, &c.e.EndPart, &c.team); err != nil {
			_ = rows.Close()
			return nil, err
		}
		cs = append(cs, c)
	}
	_ = rows.Close()
	// A non-hr caller may only look at a team they belong to or lead the reporting line of.
	var myTeam sql.NullString
	if a.HasPerson {
		_ = s.db.QueryRowContext(ctx, `SELECT team_id FROM people WHERE id = ?`, a.PersonID).Scan(&myTeam)
	}
	facts := map[string]Fact{}
	out := []CalendarEntry{}
	var audited []string
	for _, c := range cs {
		f, ok := facts[c.e.PersonID]
		if !ok && a.HasPerson {
			var err error
			if f, _, err = s.factFor(ctx, s.db, a, c.e.PersonID); err != nil {
				continue
			}
			facts[c.e.PersonID] = f
		}
		if !Decide(a, ActDirRead, f) {
			continue
		}
		if !a.hrAdmin() && !f.Self && !f.Chain && (!myTeam.Valid || !c.team.Valid || myTeam.String != c.team.String) {
			continue
		}
		e := CalendarEntry{PersonID: c.e.PersonID, StartDate: c.e.StartDate, EndDate: c.e.EndDate, StartPart: c.e.StartPart, EndPart: c.e.EndPart, Out: true}
		if Decide(a, ActLeaveRead, f) {
			e.RequestID, e.Type, e.Status = c.e.RequestID, c.e.Type, c.e.Status
			if !f.Self {
				audited = append(audited, "leave-request:"+c.e.RequestID)
			}
		} else if c.e.Status != "approved" {
			continue // a pending request is nobody else's business
		}
		out = append(out, e)
	}
	if err := s.auditReadMany(ctx, a, audited, []string{"type"}); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	return out, nil
}

// ---- leave status ------------------------------------------------------------------------------------

// ReconcileLeaveStatus moves people to the leave employment status while an approved request of a
// type that sets it is running, and back afterwards. It touches only status changes it made itself
// (a leave status set by hr stays), records no note (the reason never reaches the status history),
// and the events and directory mask the leave status exactly as for any other change.
func (s *Service) ReconcileLeaveStatus(ctx context.Context) (int, error) {
	today := s.today()
	n := 0
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.person_id FROM leave_requests r JOIN leave_types t ON t.id = r.type_id JOIN people p ON p.id = r.person_id
		WHERE r.status = 'approved' AND t.sets_leave_status = 1 AND r.halves >= t.leave_status_min_halves AND r.status_applied = 0
		AND r.start_date <= ? AND r.end_date >= ? AND p.status = 'active' AND p.erased_at IS NULL`, today, today)
	if err != nil {
		return 0, err
	}
	type item struct{ id, person string }
	var starts []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.person); err != nil {
			_ = rows.Close()
			return 0, err
		}
		starts = append(starts, it)
	}
	_ = rows.Close()
	for _, it := range starts {
		err := s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
			var cur string
			if err := tx.QueryRowContext(ctx, `SELECT status FROM people WHERE id = ?`, it.person).Scan(&cur); err != nil {
				return err
			}
			if cur != string(StActive) {
				return nil
			}
			if err := s.applyStatus(ctx, tx, it.person, StActive, StLeave, today, "timeoff", "", 0); err != nil {
				return err
			}
			n++
			_, err := tx.ExecContext(ctx, `UPDATE leave_requests SET status_applied = 1 WHERE id = ?`, it.id)
			return err
		})
		if err != nil {
			return n, err
		}
	}
	rows, err = s.db.QueryContext(ctx, `SELECT r.id, r.person_id FROM leave_requests r WHERE r.status_applied = 1 AND (r.status != 'approved' OR r.end_date < ?)`, today)
	if err != nil {
		return n, err
	}
	var ends []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.person); err != nil {
			_ = rows.Close()
			return n, err
		}
		ends = append(ends, it)
	}
	_ = rows.Close()
	for _, it := range ends {
		err := s.wtx(ctx, func(ctx context.Context, tx *sqlTx) error {
			var running int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM leave_requests WHERE person_id = ? AND id != ? AND status_applied = 1 AND status = 'approved' AND end_date >= ?`, it.person, it.id, today).Scan(&running); err != nil {
				return err
			}
			if running == 0 {
				var cur string
				if err := tx.QueryRowContext(ctx, `SELECT status FROM people WHERE id = ?`, it.person).Scan(&cur); err != nil {
					return err
				}
				if cur == string(StLeave) {
					if err := s.applyStatus(ctx, tx, it.person, StLeave, StActive, today, "timeoff", "", 0); err != nil {
						return err
					}
					n++
				}
			}
			_, err := tx.ExecContext(ctx, `UPDATE leave_requests SET status_applied = 2 WHERE id = ?`, it.id)
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

package people

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// Series is a recurring 1:1 between a manager and one direct report.
type Series struct {
	ID        string `json:"id"`
	ManagerID string `json:"manager_id"`
	ReportID  string `json:"report_id"`
	Cadence   string `json:"cadence"`
	CreatedAt string `json:"created_at"`
}

// AgendaItem is a topic either participant added before the meeting.
type AgendaItem struct {
	ID        string `json:"id"`
	AuthorID  string `json:"author_id"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

// ActionItem is a follow-up from a 1:1. WorkIssueID links it to a `work` issue once one exists.
type ActionItem struct {
	ID          string `json:"id"`
	MeetingID   string `json:"meeting_id"`
	Text        string `json:"text"`
	AssigneeID  string `json:"assignee_id"`
	DueDate     string `json:"due_date,omitempty"`
	Status      string `json:"status"`
	WorkIssueID string `json:"work_issue_id,omitempty"`
}

// Meeting is one occurrence. PrivateNotes is set only for the manager of the series; for
// every other caller the field is absent, not empty.
type Meeting struct {
	ID           string       `json:"id"`
	SeriesID     string       `json:"series_id"`
	ScheduledAt  string       `json:"scheduled_at"`
	Status       string       `json:"status"`
	SharedNotes  string       `json:"shared_notes,omitempty"`
	PrivateNotes *string      `json:"private_notes,omitempty"`
	Agenda       []AgendaItem `json:"agenda,omitempty"`
	Actions      []ActionItem `json:"actions,omitempty"`
	FollowUps    []ActionItem `json:"follow_ups,omitempty"` // open actions from earlier meetings in the series
}

type seriesRow struct {
	ID, ManagerID, ReportID, Cadence string
	CreatedAt                        int64
}

// CreateSeries starts a 1:1 series with one of the caller's direct reports. Manager only.
func (s *Service) CreateSeries(ctx context.Context, a Actor, reportID, cadence string) (*Series, error) {
	f, si, err := s.personFact(ctx, a, reportID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActOneOnOneCreate, f) {
		s.auditDeny(ctx, a, "create", "oneonone:"+reportID, nil)
		return nil, ErrForbidden
	}
	if !slices.Contains([]string{"weekly", "biweekly", "monthly"}, cadence) {
		return nil, invalid("cadence must be weekly, biweekly or monthly")
	}
	if si.status != StActive && si.status != StOnboarding && si.status != StLeave {
		return nil, conflict("cannot start 1:1s with a %s record", si.status)
	}
	id := newID("ser")
	now := s.now().Unix()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO oneonone_series (id, manager_id, report_id, cadence, created_at) VALUES (?,?,?,?,?)`, id, a.PersonID, reportID, cadence, now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, conflict("a series already exists")
		}
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "create", "series:"+id, nil); err != nil {
		return nil, err
	}
	return &Series{ID: id, ManagerID: a.PersonID, ReportID: reportID, Cadence: cadence, CreatedAt: time.Unix(now, 0).UTC().Format(time.RFC3339)}, nil
}

// seriesFact loads a series and computes the caller's relationship. Participation is
// re-checked against the live reporting line on every call: a manager who is no longer the
// report's manager (or who has been terminated) loses access immediately, private notes
// included. Non-participants get ErrNotFound.
func (s *Service) seriesFact(ctx context.Context, a Actor, seriesID string) (*seriesRow, Fact, error) {
	var r seriesRow
	err := s.db.QueryRowContext(ctx, `SELECT id, manager_id, report_id, cadence, created_at FROM oneonone_series WHERE id = ?`, seriesID).Scan(&r.ID, &r.ManagerID, &r.ReportID, &r.Cadence, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Fact{}, ErrNotFound
	}
	if err != nil {
		return nil, Fact{}, err
	}
	if !a.HasPerson {
		return nil, Fact{}, ErrNotFound
	}
	f, _, err := s.factFor(ctx, s.db, a, r.ReportID)
	if err != nil {
		return nil, f, err
	}
	f.Direct = f.Direct && r.ManagerID == a.PersonID
	f.Chain, f.SameTeam = false, false
	if !Decide(a, ActOneOnOneRead, f) {
		s.auditDeny(ctx, a, "read", "series:"+seriesID, nil)
		return nil, f, ErrNotFound
	}
	return &r, f, nil
}

// ListSeries lists the caller's series (as manager or as report).
func (s *Service) ListSeries(ctx context.Context, a Actor) ([]Series, error) {
	if !a.HasPerson {
		return []Series{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM oneonone_series WHERE manager_id = ? OR report_id = ? ORDER BY created_at, id`, a.PersonID, a.PersonID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	out := []Series{}
	for _, id := range ids {
		r, _, err := s.seriesFact(ctx, a, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, Series{ID: r.ID, ManagerID: r.ManagerID, ReportID: r.ReportID, Cadence: r.Cadence, CreatedAt: time.Unix(r.CreatedAt, 0).UTC().Format(time.RFC3339)})
	}
	return out, nil
}

// ScheduleMeeting adds a meeting to a series and emits oneonone.scheduled.v1.
func (s *Service) ScheduleMeeting(ctx context.Context, a Actor, seriesID string, at time.Time) (*Meeting, error) {
	r, _, err := s.seriesFact(ctx, a, seriesID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if at.Before(now.AddDate(-1, 0, 0)) || at.After(now.AddDate(2, 0, 0)) {
		return nil, invalid("scheduled_at must be within the last year and the next two years")
	}
	id := newID("mtg")
	err = s.tx(ctx, func(tx *sqlTx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO oneonone_meetings (id, series_id, scheduled_at, created_at, updated_at) VALUES (?,?,?,?,?)`, id, seriesID, at.Unix(), now.Unix(), now.Unix()); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, EventOneOnOneScheduled, map[string]any{
			"meeting_id": id, "series_id": seriesID, "manager_party_id": r.ManagerID, "report_party_id": r.ReportID,
			"scheduled_at": at.UTC().Format(time.RFC3339), "cadence": r.Cadence,
		})
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditWrite(ctx, a, "create", "meeting:"+id, nil); err != nil {
		return nil, err
	}
	return &Meeting{ID: id, SeriesID: seriesID, ScheduledAt: at.UTC().Format(time.RFC3339), Status: "scheduled"}, nil
}

// ListMeetings returns the schedule of a series (history), newest first, without content.
func (s *Service) ListMeetings(ctx context.Context, a Actor, seriesID string) ([]Meeting, error) {
	if _, _, err := s.seriesFact(ctx, a, seriesID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, scheduled_at, status FROM oneonone_meetings WHERE series_id = ? ORDER BY scheduled_at DESC, id`, seriesID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Meeting{}
	for rows.Next() {
		m := Meeting{SeriesID: seriesID}
		var at int64
		if err := rows.Scan(&m.ID, &at, &m.Status); err != nil {
			return nil, err
		}
		m.ScheduledAt = time.Unix(at, 0).UTC().Format(time.RFC3339)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) meetingSeries(ctx context.Context, meetingID string) (string, error) {
	var sid string
	err := s.db.QueryRowContext(ctx, `SELECT series_id FROM oneonone_meetings WHERE id = ?`, meetingID).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return sid, err
}

// GetMeeting returns agenda, shared notes, actions and follow-ups to either participant, plus
// the private notes to the manager only. The read is audited (fail closed).
func (s *Service) GetMeeting(ctx context.Context, a Actor, meetingID string) (*Meeting, error) {
	sid, err := s.meetingSeries(ctx, meetingID)
	if err != nil {
		return nil, err
	}
	_, f, err := s.seriesFact(ctx, a, sid)
	if err != nil {
		return nil, err
	}
	withPrivate := Decide(a, ActOneOnOnePrivate, f)
	fields := []string{"agenda", "shared_notes", "actions"}
	if withPrivate {
		fields = append(fields, "private_notes")
	}
	if err := s.auditRead(ctx, a, "meeting:"+meetingID, fields); err != nil {
		return nil, errors.New("audit unavailable, read refused")
	}
	m := &Meeting{ID: meetingID, SeriesID: sid}
	var at int64
	var shared, private sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT scheduled_at, status, shared_notes_sealed, private_notes_sealed FROM oneonone_meetings WHERE id = ?`, meetingID).Scan(&at, &m.Status, &shared, &private); err != nil {
		return nil, err
	}
	m.ScheduledAt = time.Unix(at, 0).UTC().Format(time.RFC3339)
	if shared.Valid {
		if m.SharedNotes, err = s.keys.Open(shared.String, meetingID+":shared_notes"); err != nil {
			return nil, errors.New("stored notes cannot be decrypted")
		}
	}
	if withPrivate {
		p := ""
		if private.Valid {
			if p, err = s.keys.Open(private.String, meetingID+":private_notes"); err != nil {
				return nil, errors.New("stored notes cannot be decrypted")
			}
		}
		m.PrivateNotes = &p
	}
	ar, err := s.db.QueryContext(ctx, `SELECT id, author_id, text, created_at FROM agenda_items WHERE meeting_id = ? ORDER BY created_at, id`, meetingID)
	if err != nil {
		return nil, err
	}
	for ar.Next() {
		var it AgendaItem
		var c int64
		if err := ar.Scan(&it.ID, &it.AuthorID, &it.Text, &c); err != nil {
			_ = ar.Close()
			return nil, err
		}
		it.CreatedAt = time.Unix(c, 0).UTC().Format(time.RFC3339)
		m.Agenda = append(m.Agenda, it)
	}
	_ = ar.Close()
	const actCols = `id, meeting_id, text, assignee_id, due_date, status, work_issue_id`
	for _, q := range []struct {
		dst  *[]ActionItem
		sql  string
		args []any
	}{
		{&m.Actions, `SELECT ` + actCols + ` FROM action_items WHERE meeting_id = ? ORDER BY created_at, id`, []any{meetingID}},
		{&m.FollowUps, `SELECT ` + actCols + ` FROM action_items WHERE status = 'open' AND meeting_id != ? AND meeting_id IN (SELECT id FROM oneonone_meetings WHERE series_id = ? AND scheduled_at <= ?) ORDER BY created_at, id`, []any{meetingID, sid, at}},
	} {
		rows, err := s.db.QueryContext(ctx, q.sql, q.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var it ActionItem
			if err := rows.Scan(&it.ID, &it.MeetingID, &it.Text, &it.AssigneeID, &it.DueDate, &it.Status, &it.WorkIssueID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			*q.dst = append(*q.dst, it)
		}
		_ = rows.Close()
	}
	return m, nil
}

// writeAccess authorises a write to the shared parts of a meeting (either participant) and
// returns the series row.
func (s *Service) meetingWrite(ctx context.Context, a Actor, meetingID string) (*seriesRow, Fact, error) {
	sid, err := s.meetingSeries(ctx, meetingID)
	if err != nil {
		return nil, Fact{}, err
	}
	r, f, err := s.seriesFact(ctx, a, sid)
	if err != nil {
		return nil, f, err
	}
	if !Decide(a, ActOneOnOneWrite, f) {
		return nil, f, ErrNotFound
	}
	return r, f, nil
}

// AddAgendaItem adds a topic. Either participant.
func (s *Service) AddAgendaItem(ctx context.Context, a Actor, meetingID, text string) (*AgendaItem, error) {
	if _, _, err := s.meetingWrite(ctx, a, meetingID); err != nil {
		return nil, err
	}
	if err := validText("text", text, 1000, true); err != nil {
		return nil, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agenda_items WHERE meeting_id = ?`, meetingID).Scan(&n); err != nil {
		return nil, err
	}
	if n >= 50 {
		return nil, conflict("a meeting holds at most 50 agenda items")
	}
	it := &AgendaItem{ID: newID("agd"), AuthorID: a.PersonID, Text: text}
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agenda_items (id, meeting_id, author_id, text, created_at) VALUES (?,?,?,?,?)`, it.ID, meetingID, a.PersonID, text, now.Unix()); err != nil {
		return nil, err
	}
	it.CreatedAt = now.UTC().Format(time.RFC3339)
	return it, s.auditWrite(ctx, a, "write", "meeting:"+meetingID, []string{"agenda"})
}

// SetNotes replaces the shared notes (either participant) or the private manager notes
// (manager only). Both are sealed at rest.
func (s *Service) SetNotes(ctx context.Context, a Actor, meetingID, text string, private bool) error {
	_, f, err := s.meetingWrite(ctx, a, meetingID)
	if err != nil {
		return err
	}
	col, field := "shared_notes_sealed", "shared_notes"
	if private {
		if !Decide(a, ActOneOnOnePrivate, f) {
			s.auditDeny(ctx, a, "write", "meeting:"+meetingID, []string{"private_notes"})
			return ErrNotFound // to the report this field does not exist
		}
		col, field = "private_notes_sealed", "private_notes"
	}
	if err := validText("notes", text, 20000, false); err != nil {
		return err
	}
	var sealed any
	if text != "" {
		c, err := s.keys.Seal(text, meetingID+":"+field)
		if err != nil {
			return err
		}
		sealed = c
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE oneonone_meetings SET `+col+` = ?, updated_at = ? WHERE id = ?`, sealed, s.now().Unix(), meetingID); err != nil { //nolint:gosec // column from two constants
		return err
	}
	return s.auditWrite(ctx, a, "write", "meeting:"+meetingID, []string{field})
}

// SetMeetingStatus marks a meeting held or cancelled. Either participant.
func (s *Service) SetMeetingStatus(ctx context.Context, a Actor, meetingID, status string) error {
	if _, _, err := s.meetingWrite(ctx, a, meetingID); err != nil {
		return err
	}
	if status != "held" && status != "cancelled" && status != "scheduled" {
		return invalid("status must be scheduled, held or cancelled")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE oneonone_meetings SET status = ?, updated_at = ? WHERE id = ?`, status, s.now().Unix(), meetingID); err != nil {
		return err
	}
	return s.auditWrite(ctx, a, "write", "meeting:"+meetingID, []string{"status"})
}

// AddAction records an action item assigned to the manager or the report of the series.
func (s *Service) AddAction(ctx context.Context, a Actor, meetingID, text, assigneeID, due string) (*ActionItem, error) {
	r, _, err := s.meetingWrite(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if err := validText("text", text, 1000, true); err != nil {
		return nil, err
	}
	if assigneeID != r.ManagerID && assigneeID != r.ReportID {
		return nil, invalid("assignee_id must be the manager or the report of this series")
	}
	if due != "" {
		if err := validDate(due); err != nil {
			return nil, err
		}
	}
	it := &ActionItem{ID: newID("act"), MeetingID: meetingID, Text: text, AssigneeID: assigneeID, DueDate: due, Status: "open"}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO action_items (id, meeting_id, text, assignee_id, due_date, created_by, created_at) VALUES (?,?,?,?,?,?,?)`,
		it.ID, meetingID, text, assigneeID, due, a.PersonID, s.now().Unix()); err != nil {
		return nil, err
	}
	return it, s.auditWrite(ctx, a, "write", "meeting:"+meetingID, []string{"actions"})
}

// UpdateAction sets status and/or the linked work issue. Either participant.
func (s *Service) UpdateAction(ctx context.Context, a Actor, actionID string, status, workIssue *string) (*ActionItem, error) {
	var meetingID string
	err := s.db.QueryRowContext(ctx, `SELECT meeting_id FROM action_items WHERE id = ?`, actionID).Scan(&meetingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, _, err := s.meetingWrite(ctx, a, meetingID); err != nil {
		return nil, err
	}
	if status == nil && workIssue == nil {
		return nil, invalid("no fields to change")
	}
	if status != nil {
		if *status != "open" && *status != "done" {
			return nil, invalid("status must be open or done")
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE action_items SET status = ? WHERE id = ?`, *status, actionID); err != nil {
			return nil, err
		}
	}
	if workIssue != nil {
		if *workIssue != "" && !issueIDRe.MatchString(*workIssue) {
			return nil, invalid("work_issue_id has an invalid format")
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE action_items SET work_issue_id = ? WHERE id = ?`, *workIssue, actionID); err != nil {
			return nil, err
		}
	}
	var it ActionItem
	if err := s.db.QueryRowContext(ctx, `SELECT id, meeting_id, text, assignee_id, due_date, status, work_issue_id FROM action_items WHERE id = ?`, actionID).Scan(&it.ID, &it.MeetingID, &it.Text, &it.AssigneeID, &it.DueDate, &it.Status, &it.WorkIssueID); err != nil {
		return nil, err
	}
	return &it, s.auditWrite(ctx, a, "write", "meeting:"+meetingID, []string{"actions"})
}

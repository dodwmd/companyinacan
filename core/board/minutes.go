package board

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Minutes is one version of the minutes of a meeting. A draft is readable only by the secretary and by
// the members who attended; an approved version is immutable; a correction is a new version that must
// be approved again, and the earlier approved version stays on record.
type Minutes struct {
	ID             string `json:"id"`
	MeetingID      string `json:"meeting_id"`
	Version        int    `json:"version"`
	Status         string `json:"status"` // draft | approved | superseded
	Content        string `json:"content,omitempty"`
	ContentHidden  bool   `json:"content_withheld,omitempty"`
	ApprovedOn     string `json:"approved_on,omitempty"`
	CanApproveHint bool   `json:"-"`
}

type minRow struct {
	ID, MeetingID, Status, Author, AuthorPerson, Edited, Approved, ApprovedOn, Sum string
	Version                                                                        int
	Sealed                                                                         sql.NullString
}

const minCols = `id, meeting_id, version, status, sens_content, content_sha256, author_ref, author_person, edited_by, approved_by, IFNULL(approved_on,'')`

func scanMin(r interface{ Scan(...any) error }) (*minRow, error) {
	var m minRow
	err := r.Scan(&m.ID, &m.MeetingID, &m.Version, &m.Status, &m.Sealed, &m.Sum, &m.Author, &m.AuthorPerson, &m.Edited, &m.Approved, &m.ApprovedOn)
	return &m, err
}

func (s *Service) loadMinutes(ctx context.Context, q querier, id string) (*minRow, error) {
	if err := validID("min", id); err != nil {
		return nil, err
	}
	m, err := scanMin(q.QueryRowContext(ctx, `SELECT `+minCols+` FROM minutes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

func (s *Service) openMinutes(m *minRow) (string, error) {
	content, err := s.open(m.ID, "content", m.Sealed)
	if err != nil {
		return "", err
	}
	if contentHash(m.ID, content) != m.Sum {
		return "", errors.New("minutes failed their integrity check")
	}
	return content, nil
}

// minutesFact is the meeting fact plus whether the actor drafted or edited this version.
func (s *Service) minutesFact(ctx context.Context, a Actor, m *minRow) (*Meeting, Fact, error) {
	mt, err := s.loadMeeting(ctx, s.db, m.MeetingID)
	if err != nil {
		return nil, Fact{}, err
	}
	f, err := s.meetingFact(ctx, s.db, a, mt)
	if err != nil {
		return nil, f, err
	}
	f.Drafter = !sodOK(a, append([]string{m.Author, m.AuthorPerson}, strings.Split(m.Edited, ",")...)...)
	return mt, f, nil
}

// DraftMinutes starts a new version of the minutes as a draft. secretary. One open draft per meeting.
func (s *Service) DraftMinutes(ctx context.Context, a Actor, meetingID, content string) (*Minutes, error) {
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActMinutesDraft, f, "meeting:"+meetingID); err != nil {
		return nil, err
	}
	if err := validText("content", content, 200000, true); err != nil {
		return nil, err
	}
	if m.Status == "cancelled" || m.StartsOn > s.today() {
		return nil, conflict("minutes are drafted for a meeting that has started and was not cancelled")
	}
	open, err := s.exists(ctx, s.db, `SELECT 1 FROM minutes WHERE meeting_id = ? AND status = 'draft'`, meetingID)
	if err != nil {
		return nil, err
	}
	if open {
		return nil, conflict("there is already a draft: edit it")
	}
	id := newID("min")
	sealed, err := s.seal(id, "content", content)
	if err != nil {
		return nil, err
	}
	var version int
	err = s.txAudit(ctx, a, "write", "minutes:"+id, []string{FieldMinutesDraft}, func(ctx context.Context, tx *sqlTx) error {
		if err := tx.QueryRowContext(ctx, `SELECT IFNULL(MAX(version),0)+1 FROM minutes WHERE meeting_id = ?`, meetingID).Scan(&version); err != nil {
			return err
		}
		now := s.now().Unix()
		_, err := tx.ExecContext(ctx, `INSERT INTO minutes (id, meeting_id, version, status, sens_content, content_sha256, author_ref, author_person, created_at, updated_at) VALUES (?,?,?,'draft',?,?,?,?,?,?)`,
			id, meetingID, version, sealed, contentHash(id, content), canonSubject(a.Subject), a.PersonID, now, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Minutes{ID: id, MeetingID: meetingID, Version: version, Status: "draft", Content: content}, nil
}

// EditMinutes replaces the content of a draft (never of an approved version).
func (s *Service) EditMinutes(ctx context.Context, a Actor, id, content string) (*Minutes, error) {
	m, err := s.loadMinutes(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	_, f, err := s.minutesFact(ctx, a, m)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActMinutesDraftRead, f, "minutes:"+id); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActMinutesDraft, f, "minutes:"+id); err != nil {
		return nil, err
	}
	if err := validText("content", content, 200000, true); err != nil {
		return nil, err
	}
	if m.Status != "draft" {
		return nil, conflict("approved minutes are immutable: draft a new version")
	}
	sealed, err := s.seal(id, "content", content)
	if err != nil {
		return nil, err
	}
	edited := m.Edited
	if c := canonSubject(a.Subject); !strings.Contains(","+edited+",", ","+c+",") {
		if edited != "" {
			edited += ","
		}
		edited += c
	}
	err = s.txAudit(ctx, a, "write", "minutes:"+id, []string{FieldMinutesDraft}, func(ctx context.Context, tx *sqlTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE minutes SET sens_content = ?, content_sha256 = ?, edited_by = ?, updated_at = ? WHERE id = ? AND status = 'draft'`,
			sealed, contentHash(id, content), edited, s.now().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return conflict("the minutes were approved in the meantime")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Minutes{ID: id, MeetingID: m.MeetingID, Version: m.Version, Status: "draft", Content: content}, nil
}

// GetMinutes returns one version. A draft is invisible (404) to anyone who may not read drafts.
func (s *Service) GetMinutes(ctx context.Context, a Actor, id string) (*Minutes, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	m, err := s.loadMinutes(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	_, f, err := s.minutesFact(ctx, a, m)
	if err != nil {
		return nil, err
	}
	act, field := ActMinutesRead, FieldMinutes
	if m.Status == "draft" {
		act, field = ActMinutesDraftRead, FieldMinutesDraft
	}
	if err := s.hide(ctx, a, act, f, "minutes:"+id); err != nil {
		return nil, err
	}
	out := &Minutes{ID: m.ID, MeetingID: m.MeetingID, Version: m.Version, Status: m.Status, ApprovedOn: m.ApprovedOn}
	var fields []string
	if FieldAccess(a, false, field, f) {
		if out.Content, err = s.openMinutes(m); err != nil {
			return nil, err
		}
		fields = []string{field}
	} else {
		out.ContentHidden = true
	}
	if err := s.auditRead(ctx, a, "minutes:"+id, fields); err != nil {
		return nil, err
	}
	return out, nil
}

// ListMinutes lists the versions of a meeting's minutes the caller may see (no content).
func (s *Service) ListMinutes(ctx context.Context, a Actor, meetingID string) ([]Minutes, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	mt, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+minCols+` FROM minutes WHERE meeting_id = ? ORDER BY version`, meetingID)
	if err != nil {
		return nil, err
	}
	var all []*minRow
	for rows.Next() {
		m, err := scanMin(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, m)
	}
	_ = rows.Close()
	out := []Minutes{}
	var res []string
	for _, m := range all {
		act := ActMinutesRead
		if m.Status == "draft" {
			act = ActMinutesDraftRead
		}
		mf := f
		mf.Attended = f.Attended
		if !Decide(a, act, mf) {
			continue
		}
		out = append(out, Minutes{ID: m.ID, MeetingID: mt.ID, Version: m.Version, Status: m.Status, ApprovedOn: m.ApprovedOn})
		res = append(res, "minutes:"+m.ID)
	}
	if err := s.auditReadMany(ctx, a, "read", res, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// ApproveMinutes approves a draft. A director who attended the meeting; never the person who drafted or
// edited it (separation of duties, one function). The meeting must be held. A previous approved version
// becomes superseded and stays on record.
func (s *Service) ApproveMinutes(ctx context.Context, a Actor, id string) (*Minutes, error) {
	m, err := s.loadMinutes(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	mt, f, err := s.minutesFact(ctx, a, m)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActMinutesDraftRead, f, "minutes:"+id); err != nil {
		return nil, err
	}
	if m.Status != "draft" {
		return nil, conflict("these minutes are already %s", m.Status)
	}
	if f.Drafter && f.Member {
		s.auditDeny(ctx, a, "minutes.approve", "minutes:"+id, nil)
		return nil, ErrSeparation
	}
	if err := s.authorize(ctx, a, ActMinutesApprove, f, "minutes:"+id); err != nil {
		return nil, err
	}
	if mt.Status != "held" {
		return nil, conflict("mark the meeting held before approving its minutes")
	}
	if _, err := s.openMinutes(m); err != nil {
		return nil, err
	}
	today := s.today()
	err = s.txAudit(ctx, a, "approve", "minutes:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE minutes SET status = 'approved', approved_by = ?, approved_on = ?, updated_at = ? WHERE id = ? AND status = 'draft'`,
			canonSubject(a.Subject), today, s.now().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return conflict("the minutes changed: reload them")
		}
		_, err = tx.ExecContext(ctx, `UPDATE minutes SET status = 'superseded' WHERE meeting_id = ? AND status = 'approved' AND id != ?`, m.MeetingID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetMinutes(ctx, a, id)
}

// ---- action items -----------------------------------------------------------------------------------

// ActionInput creates an action item. WorkIssueRef links a `work` issue by opaque id.
type ActionInput struct {
	Text         string `json:"text"`
	AssigneeID   string `json:"assignee_person_id"`
	WorkIssueRef string `json:"work_issue_ref"`
	DueOn        string `json:"due_on"`
	ResolutionID string `json:"resolution_id"`
}

// ActionItem is a follow-up from a meeting.
type ActionItem struct {
	ID           string `json:"id"`
	MeetingID    string `json:"meeting_id"`
	Text         string `json:"text"`
	AssigneeID   string `json:"assignee_person_id,omitempty"`
	WorkIssueRef string `json:"work_issue_ref,omitempty"`
	DueOn        string `json:"due_on,omitempty"`
	ResolutionID string `json:"resolution_id,omitempty"`
	Status       string `json:"status"`
}

// CreateAction records an action item. secretary or chair.
func (s *Service) CreateAction(ctx context.Context, a Actor, meetingID string, in ActionInput) (*ActionItem, error) {
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActActionManage, f, "meeting:"+meetingID); err != nil {
		return nil, err
	}
	if err := validText("text", in.Text, 2000, true); err != nil {
		return nil, err
	}
	if in.AssigneeID != "" {
		if err := validPerson("assignee_person_id", in.AssigneeID); err != nil {
			return nil, err
		}
	}
	if err := validRef("work_issue_ref", in.WorkIssueRef, false); err != nil {
		return nil, err
	}
	if in.DueOn != "" {
		if _, err := parseDate(in.DueOn); err != nil {
			return nil, err
		}
	}
	if in.ResolutionID != "" {
		r, err := s.loadResolutionRow(ctx, s.db, in.ResolutionID)
		if err != nil || r.BodyID != m.BodyID {
			return nil, invalid("resolution_id is not a resolution of this body")
		}
	}
	id := newID("act")
	sealed, err := s.seal(id, "text", in.Text)
	if err != nil {
		return nil, err
	}
	err = s.txAudit(ctx, a, "write", "action:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		now := s.now().Unix()
		_, err := tx.ExecContext(ctx, `INSERT INTO action_items (id, meeting_id, resolution_id, sens_text, assignee_person, work_issue_ref, due_on, status, created_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,'open',?,?,?)`,
			id, meetingID, nullStr(in.ResolutionID), sealed, in.AssigneeID, in.WorkIssueRef, in.DueOn, canonSubject(a.Subject), now, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &ActionItem{ID: id, MeetingID: meetingID, Text: in.Text, AssigneeID: in.AssigneeID, WorkIssueRef: in.WorkIssueRef, DueOn: in.DueOn, ResolutionID: in.ResolutionID, Status: "open"}, nil
}

func (s *Service) loadAction(ctx context.Context, q querier, id string) (*ActionItem, error) {
	if err := validID("act", id); err != nil {
		return nil, err
	}
	var it ActionItem
	var sealed sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id, meeting_id, IFNULL(resolution_id,''), sens_text, assignee_person, work_issue_ref, due_on, status FROM action_items WHERE id = ?`, id).
		Scan(&it.ID, &it.MeetingID, &it.ResolutionID, &sealed, &it.AssigneeID, &it.WorkIssueRef, &it.DueOn, &it.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	it.Text, err = s.open(it.ID, "text", sealed)
	return &it, err
}

func (s *Service) actionFact(ctx context.Context, a Actor, it *ActionItem) (Fact, error) {
	m, err := s.loadMeeting(ctx, s.db, it.MeetingID)
	if err != nil {
		return Fact{}, err
	}
	f, err := s.bodyFact(ctx, s.db, a, m.BodyID)
	f.Self = a.PersonID != "" && it.AssigneeID == a.PersonID
	return f, err
}

// GetAction returns an action item to the secretary, the body's members and its assignee.
func (s *Service) GetAction(ctx context.Context, a Actor, id string) (*ActionItem, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	it, err := s.loadAction(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	f, err := s.actionFact(ctx, a, it)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActActionRead, f, "action:"+id); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "action:"+id, nil); err != nil {
		return nil, err
	}
	return it, nil
}

// ListActions lists the action items of a meeting the caller may see, or (meetingID "") those assigned to the caller.
func (s *Service) ListActions(ctx context.Context, a Actor, meetingID string) ([]*ActionItem, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	q, args := `SELECT id FROM action_items WHERE assignee_person = ? AND assignee_person != '' ORDER BY created_at DESC LIMIT 500`, []any{a.PersonID}
	if meetingID != "" {
		if err := validID("mtg", meetingID); err != nil {
			return nil, err
		}
		q, args = `SELECT id FROM action_items WHERE meeting_id = ? ORDER BY created_at LIMIT 1000`, []any{meetingID}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
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
	out := []*ActionItem{}
	var res []string
	for _, id := range ids {
		it, err := s.loadAction(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		f, err := s.actionFact(ctx, a, it)
		if err != nil {
			return nil, err
		}
		if Decide(a, ActActionRead, f) {
			out = append(out, it)
			res = append(res, "action:"+id)
		}
	}
	if err := s.auditReadMany(ctx, a, "read", res, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateAction marks an action done or cancelled. The assignee or the secretary.
func (s *Service) UpdateAction(ctx context.Context, a Actor, id, status string) (*ActionItem, error) {
	it, err := s.loadAction(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	f, err := s.actionFact(ctx, a, it)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActActionRead, f, "action:"+id); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActActionUpdate, f, "action:"+id); err != nil {
		return nil, err
	}
	if status != "done" && status != "cancelled" && status != "open" {
		return nil, invalid("status must be open, done or cancelled")
	}
	err = s.txAudit(ctx, a, "write", "action:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE action_items SET status = ?, updated_at = ? WHERE id = ?`, status, s.now().Unix(), id)
		return err
	})
	it.Status = status
	return it, err
}

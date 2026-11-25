package board

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Invitee is a person invited to a meeting who is not a member of the body (an officer, a guest).
type Invitee struct {
	PersonID string `json:"person_id"`
	Capacity string `json:"capacity"` // attendee | officer
}

// MeetingInput schedules a meeting. Notice is data: the body says how many days it needs; a shorter
// notice is accepted only with the opaque reference of the recorded waiver.
type MeetingInput struct {
	Title         string    `json:"title"`
	StartsOn      string    `json:"starts_on"`
	StartsTime    string    `json:"starts_time"`
	Mode          string    `json:"mode"`
	NoticeGivenOn string    `json:"notice_given_on"`
	WaiverRef     string    `json:"short_notice_waiver_ref"`
	Invitees      []Invitee `json:"invitees"`
}

// Meeting is a scheduled, held or cancelled meeting.
type Meeting struct {
	ID                   string    `json:"id"`
	BodyID               string    `json:"body_id"`
	Title                string    `json:"title"`
	StartsOn             string    `json:"starts_on"`
	StartsTime           string    `json:"starts_time,omitempty"`
	Mode                 string    `json:"mode"`
	Status               string    `json:"status"`
	NoticeGivenOn        string    `json:"notice_given_on"`
	NoticeDaysRequired   int       `json:"notice_days_required"`
	ShortNoticeWaiverRef string    `json:"short_notice_waiver_ref,omitempty"`
	PackIssuedOn         string    `json:"pack_issued_on,omitempty"`
	Invitees             []Invitee `json:"invitees,omitempty"`
}

const meetingCols = `id, body_id, title, starts_on, starts_time, mode, status, notice_given_on, notice_days_required, short_notice_waiver_ref, IFNULL(pack_issued_on,'')`

func scanMeeting(r interface{ Scan(...any) error }) (*Meeting, error) {
	var m Meeting
	err := r.Scan(&m.ID, &m.BodyID, &m.Title, &m.StartsOn, &m.StartsTime, &m.Mode, &m.Status, &m.NoticeGivenOn, &m.NoticeDaysRequired, &m.ShortNoticeWaiverRef, &m.PackIssuedOn)
	return &m, err
}

func (s *Service) loadMeeting(ctx context.Context, q querier, id string) (*Meeting, error) {
	if err := validID("mtg", id); err != nil {
		return nil, err
	}
	m, err := scanMeeting(q.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// meetingFact is the body fact plus the meeting-level relationship (invited, attended).
func (s *Service) meetingFact(ctx context.Context, q querier, a Actor, m *Meeting) (Fact, error) {
	f, err := s.bodyFact(ctx, q, a, m.BodyID)
	if err != nil || a.PersonID == "" {
		return f, err
	}
	f.Invited, err = s.exists(ctx, q, `SELECT 1 FROM meeting_invitees WHERE meeting_id = ? AND person_id = ?`, m.ID, a.PersonID)
	if err != nil {
		return f, err
	}
	f.Attended, err = s.exists(ctx, q, `SELECT 1 FROM attendance WHERE meeting_id = ? AND person_id = ? AND status IN ('present','remote')`, m.ID, a.PersonID)
	return f, err
}

// seeMeeting loads a meeting the caller can see, with the caller's fact.
func (s *Service) seeMeeting(ctx context.Context, a Actor, id string) (*Meeting, Fact, error) {
	m, err := s.loadMeeting(ctx, s.db, id)
	if err != nil {
		return nil, Fact{}, err
	}
	f, err := s.meetingFact(ctx, s.db, a, m)
	if err != nil {
		return nil, f, err
	}
	if err := s.hide(ctx, a, ActMeetingRead, f, "meeting:"+id); err != nil {
		return nil, f, err
	}
	return m, f, nil
}

func daysBetween(from, to string) (int, error) {
	f, err := parseDate(from)
	if err != nil {
		return 0, err
	}
	t, err := parseDate(to)
	if err != nil {
		return 0, err
	}
	return int(t.Sub(f).Hours() / 24), nil
}

// ScheduleMeeting schedules a meeting of a body and announces it (board.meeting.scheduled.v1: ids and
// the date only). secretary, or the chair of the body.
func (s *Service) ScheduleMeeting(ctx context.Context, a Actor, bodyID string, in MeetingInput) (*Meeting, error) {
	b, err := s.loadBody(ctx, s.db, bodyID)
	if err != nil {
		return nil, err
	}
	f, err := s.bodyFact(ctx, s.db, a, bodyID)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActBodyRead, f, "body:"+bodyID); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActMeetingSchedule, f, "body:"+bodyID); err != nil {
		return nil, err
	}
	if err := validLine("title", in.Title, 200, true); err != nil {
		return nil, err
	}
	if _, err := parseDate(in.StartsOn); err != nil {
		return nil, err
	}
	if in.StartsTime != "" {
		if _, err := time.Parse("15:04", in.StartsTime); err != nil {
			return nil, invalid("starts_time must be HH:MM")
		}
	}
	if in.Mode == "" {
		in.Mode = "in-person"
	}
	if in.Mode != "in-person" && in.Mode != "remote" && in.Mode != "hybrid" {
		return nil, invalid("mode must be in-person, remote or hybrid")
	}
	if in.NoticeGivenOn == "" {
		in.NoticeGivenOn = s.today()
	}
	gap, err := daysBetween(in.NoticeGivenOn, in.StartsOn)
	if err != nil {
		return nil, err
	}
	if gap < 0 {
		return nil, invalid("the meeting cannot start before notice is given")
	}
	if gap < b.NoticeDays {
		if in.WaiverRef == "" {
			return nil, invalid("the body needs %d days notice and only %d were given: record the waiver and pass its reference in short_notice_waiver_ref", b.NoticeDays, gap)
		}
		if err := validRef("short_notice_waiver_ref", in.WaiverRef, true); err != nil {
			return nil, err
		}
	} else {
		in.WaiverRef = ""
	}
	if len(in.Invitees) > 200 {
		return nil, invalid("at most 200 invitees")
	}
	for _, iv := range in.Invitees {
		if err := validPerson("invitee person_id", iv.PersonID); err != nil {
			return nil, err
		}
		if iv.Capacity != "attendee" && iv.Capacity != "officer" {
			return nil, invalid("invitee capacity must be attendee or officer")
		}
	}
	id := newID("mtg")
	now := s.now().Unix()
	err = s.txAudit(ctx, a, "write", "meeting:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meetings (id, body_id, title, starts_on, starts_time, mode, status, notice_given_on, notice_days_required, short_notice_waiver_ref, created_by, created_at, updated_at)
			VALUES (?,?,?,?,?,?,'scheduled',?,?,?,?,?,?)`, id, bodyID, in.Title, in.StartsOn, in.StartsTime, in.Mode, in.NoticeGivenOn, b.NoticeDays, in.WaiverRef, canonSubject(a.Subject), now, now); err != nil {
			return err
		}
		for _, iv := range in.Invitees {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meeting_invitees (meeting_id, person_id, capacity, added_by) VALUES (?,?,?,?)`, id, iv.PersonID, iv.Capacity, canonSubject(a.Subject)); err != nil {
				return err
			}
		}
		return s.enqueue(ctx, tx, EventMeetingScheduled, map[string]any{"meeting_id": id, "body_id": bodyID, "starts_on": in.StartsOn})
	})
	if err != nil {
		return nil, err
	}
	m, err := s.loadMeeting(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	m.Invitees = in.Invitees
	return m, nil
}

// GetMeeting returns a meeting the caller can see.
func (s *Service) GetMeeting(ctx context.Context, a Actor, id string) (*Meeting, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	m, _, err := s.seeMeeting(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "meeting:"+id, nil); err != nil {
		return nil, err
	}
	return m, nil
}

// ListMeetings lists a body's meetings the caller can see.
func (s *Service) ListMeetings(ctx context.Context, a Actor, bodyID string) ([]*Meeting, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if _, err := s.loadBody(ctx, s.db, bodyID); err != nil {
		return nil, err
	}
	bf, err := s.bodyFact(ctx, s.db, a, bodyID)
	if err != nil {
		return nil, err
	}
	if err := s.hide(ctx, a, ActBodyRead, bf, "body:"+bodyID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE body_id = ? ORDER BY starts_on DESC, id LIMIT 1000`, bodyID)
	if err != nil {
		return nil, err
	}
	var all []*Meeting
	for rows.Next() {
		m, err := scanMeeting(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, m)
	}
	_ = rows.Close()
	out := []*Meeting{}
	var res []string
	for _, m := range all {
		f, err := s.meetingFact(ctx, s.db, a, m)
		if err != nil {
			return nil, err
		}
		if Decide(a, ActMeetingRead, f) {
			out = append(out, m)
			res = append(res, "meeting:"+m.ID)
		}
	}
	if err := s.auditReadMany(ctx, a, "read", res, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// SetMeetingStatus marks a scheduled meeting held or cancelled. secretary (or the chair).
func (s *Service) SetMeetingStatus(ctx context.Context, a Actor, id, status string) (*Meeting, error) {
	m, f, err := s.seeMeeting(ctx, a, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActMeetingSchedule, f, "meeting:"+id); err != nil {
		return nil, err
	}
	if status != "held" && status != "cancelled" {
		return nil, invalid("status must be held or cancelled")
	}
	if m.Status != "scheduled" {
		return nil, conflict("the meeting is already %s", m.Status)
	}
	if status == "held" && m.StartsOn > s.today() {
		return nil, conflict("the meeting has not started")
	}
	err = s.txAudit(ctx, a, "write", "meeting:"+id, nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE meetings SET status = ?, updated_at = ? WHERE id = ? AND status = 'scheduled'`, status, s.now().Unix(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	m.Status = status
	return m, nil
}

// ---- agenda --------------------------------------------------------------------------------------

// AgendaInput adds an agenda item.
type AgendaInput struct {
	Title        string `json:"title"`
	Kind         string `json:"kind"` // information | discussion | resolution | closed
	ResolutionID string `json:"resolution_id"`
}

// AgendaItem is one line of the agenda.
type AgendaItem struct {
	ID           string `json:"id"`
	Position     int    `json:"position"`
	Title        string `json:"title"`
	Kind         string `json:"kind"`
	ResolutionID string `json:"resolution_id,omitempty"`
}

// AddAgendaItem appends an item. secretary or chair. Only while the meeting is scheduled.
func (s *Service) AddAgendaItem(ctx context.Context, a Actor, meetingID string, in AgendaInput) (*AgendaItem, error) {
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActAgendaManage, f, "meeting:"+meetingID); err != nil {
		return nil, err
	}
	if err := validLine("title", in.Title, 200, true); err != nil {
		return nil, err
	}
	switch in.Kind {
	case "information", "discussion", "resolution", "closed":
	default:
		return nil, invalid("kind must be information, discussion, resolution or closed")
	}
	if m.Status != "scheduled" {
		return nil, conflict("the agenda of a %s meeting does not change", m.Status)
	}
	if in.ResolutionID != "" {
		r, err := s.loadResolutionRow(ctx, s.db, in.ResolutionID)
		if err != nil || r.BodyID != m.BodyID {
			return nil, invalid("resolution_id is not a resolution of this body")
		}
	}
	id := newID("agi")
	it := &AgendaItem{ID: id, Title: in.Title, Kind: in.Kind, ResolutionID: in.ResolutionID}
	err = s.txAudit(ctx, a, "write", "meeting:"+meetingID+"/agenda", nil, func(ctx context.Context, tx *sqlTx) error {
		var pos int
		if err := tx.QueryRowContext(ctx, `SELECT IFNULL(MAX(position),0)+1 FROM agenda_items WHERE meeting_id = ?`, meetingID).Scan(&pos); err != nil {
			return err
		}
		it.Position = pos
		_, err := tx.ExecContext(ctx, `INSERT INTO agenda_items (id, meeting_id, position, title, kind, resolution_id, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, meetingID, pos, in.Title, in.Kind, nullStr(in.ResolutionID), canonSubject(a.Subject), s.now().Unix())
		return err
	})
	return it, err
}

// ListAgenda returns the agenda of a meeting the caller can see.
func (s *Service) ListAgenda(ctx context.Context, a Actor, meetingID string) ([]AgendaItem, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if _, _, err := s.seeMeeting(ctx, a, meetingID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, position, title, kind, IFNULL(resolution_id,'') FROM agenda_items WHERE meeting_id = ? ORDER BY position`, meetingID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AgendaItem{}
	for rows.Next() {
		var it AgendaItem
		if err := rows.Scan(&it.ID, &it.Position, &it.Title, &it.Kind, &it.ResolutionID); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "meeting:"+meetingID+"/agenda", nil); err != nil {
		return nil, err
	}
	return out, nil
}

// ---- attendance and quorum ----------------------------------------------------------------------

// AttendanceInput records one person's attendance.
type AttendanceInput struct {
	Status string `json:"status"` // present | remote | apology | absent
}

// Attendee is one attendance record.
type Attendee struct {
	PersonID string `json:"person_id"`
	Status   string `json:"status"`
	Capacity string `json:"capacity"` // member | observer | invitee
}

// RecordAttendance records or corrects attendance until the meeting has approved minutes. secretary.
func (s *Service) RecordAttendance(ctx context.Context, a Actor, meetingID, person, status string) (*Attendee, error) {
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActAttendanceRecord, f, "meeting:"+meetingID); err != nil {
		return nil, err
	}
	if err := validPerson("person_id", person); err != nil {
		return nil, err
	}
	switch status {
	case "present", "remote", "apology", "absent":
	default:
		return nil, invalid("status must be present, remote, apology or absent")
	}
	if m.Status == "cancelled" {
		return nil, conflict("the meeting was cancelled")
	}
	if m.StartsOn > s.today() && (status == "present" || status == "remote") {
		return nil, conflict("the meeting has not started")
	}
	cap, err := s.capacityOf(ctx, s.db, m, person)
	if err != nil {
		return nil, err
	}
	if cap == "" {
		return nil, invalid("the person is neither a member of the body nor invited to the meeting")
	}
	err = s.txAudit(ctx, a, "write", "meeting:"+meetingID+"/attendance", nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO attendance (meeting_id, person_id, status, recorded_by, recorded_at) VALUES (?,?,?,?,?)
			ON CONFLICT (meeting_id, person_id) DO UPDATE SET status = excluded.status, recorded_by = excluded.recorded_by, recorded_at = excluded.recorded_at`,
			meetingID, person, status, canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		return nil, conflict("attendance can no longer change: %v", err)
	}
	return &Attendee{PersonID: person, Status: status, Capacity: cap}, nil
}

func (s *Service) capacityOf(ctx context.Context, q querier, m *Meeting, person string) (string, error) {
	role, err := s.memberRole(ctx, q, m.BodyID, person, m.StartsOn)
	if err != nil {
		return "", err
	}
	switch role {
	case "chair", "member":
		return "member", nil
	case "observer":
		return "observer", nil
	}
	in, err := s.exists(ctx, q, `SELECT 1 FROM meeting_invitees WHERE meeting_id = ? AND person_id = ?`, m.ID, person)
	if in {
		return "invitee", err
	}
	return "", err
}

// ListAttendance returns who attended. Members of the body, the secretary and approved-minutes readers.
func (s *Service) ListAttendance(ctx context.Context, a Actor, meetingID string) ([]Attendee, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActMinutesRead, f) {
		s.auditDeny(ctx, a, "read", "meeting:"+meetingID+"/attendance", nil)
		return nil, ErrForbidden
	}
	rows, err := s.db.QueryContext(ctx, `SELECT person_id, status FROM attendance WHERE meeting_id = ? ORDER BY person_id`, meetingID)
	if err != nil {
		return nil, err
	}
	var out []Attendee
	for rows.Next() {
		var at Attendee
		if err := rows.Scan(&at.PersonID, &at.Status); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, at)
	}
	_ = rows.Close()
	for i := range out {
		out[i].Capacity, _ = s.capacityOf(ctx, s.db, m, out[i].PersonID)
	}
	if out == nil {
		out = []Attendee{}
	}
	if err := s.auditRead(ctx, a, "meeting:"+meetingID+"/attendance", nil); err != nil {
		return nil, err
	}
	return out, nil
}

// QuorumResult is the quorum position of a meeting. All counts are integers; the rule is data on the body.
type QuorumResult struct {
	Eligible int  `json:"eligible"` // members in term counted towards quorum
	Present  int  `json:"present"`
	Required int  `json:"required"`
	Met      bool `json:"met"`
}

// quorumFor counts the members in term on the meeting day, minus the excluded people when the body
// excludes recused members, and how many of those are present.
func (s *Service) quorumFor(ctx context.Context, q querier, b *Body, m *Meeting, excluded map[string]bool) (QuorumResult, error) {
	members, err := s.votingMembers(ctx, q, b.ID, m.StartsOn)
	if err != nil {
		return QuorumResult{}, err
	}
	var r QuorumResult
	for _, p := range members {
		if b.QuorumExcludesRecused && excluded[p] {
			continue
		}
		r.Eligible++
		ok, err := s.exists(ctx, q, `SELECT 1 FROM attendance WHERE meeting_id = ? AND person_id = ? AND status IN ('present','remote')`, m.ID, p)
		if err != nil {
			return r, err
		}
		if ok {
			r.Present++
		}
	}
	r.Required = QuorumRequired(r.Eligible, b.QuorumNum, b.QuorumDen, b.QuorumMin)
	r.Met = r.Present >= r.Required
	return r, nil
}

// Quorum computes the quorum position of a meeting (no resolution: nobody is excluded).
func (s *Service) Quorum(ctx context.Context, a Actor, meetingID string) (*QuorumResult, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActPackRead, f) && !Decide(a, ActMinutesDraftRead, f) {
		s.auditDeny(ctx, a, "read", "meeting:"+meetingID+"/quorum", nil)
		return nil, ErrForbidden
	}
	b, err := s.loadBody(ctx, s.db, m.BodyID)
	if err != nil {
		return nil, err
	}
	r, err := s.quorumFor(ctx, s.db, b, m, nil)
	if err != nil {
		return nil, err
	}
	if err := s.auditRead(ctx, a, "meeting:"+meetingID+"/quorum", nil); err != nil {
		return nil, err
	}
	return &r, nil
}

// ---- board pack -----------------------------------------------------------------------------------

// PackInput adds a document reference to a meeting's pack. Only metadata: the bytes live in the
// documents capability.
type PackInput struct {
	DocRef    string `json:"doc_ref"`
	Title     string `json:"title"`
	Note      string `json:"note"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// PackItem is one document in the pack.
type PackItem struct {
	ID            string `json:"id"`
	DocRef        string `json:"doc_ref"`
	Title         string `json:"title"`
	Note          string `json:"note,omitempty"`
	MediaType     string `json:"media_type,omitempty"`
	SizeBytes     int64  `json:"size_bytes"`
	SHA256        string `json:"sha256,omitempty"`
	Position      int    `json:"position"`
	Supplementary bool   `json:"supplementary"`
}

// Pack is the assembled board pack of a meeting.
type Pack struct {
	MeetingID string     `json:"meeting_id"`
	IssuedOn  string     `json:"issued_on,omitempty"`
	Items     []PackItem `json:"items"`
}

// AddPackItem appends a document reference. After the pack is issued, additions are marked
// supplementary: the issued list is never altered. secretary.
func (s *Service) AddPackItem(ctx context.Context, a Actor, meetingID string, in PackInput) (*PackItem, error) {
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActPackManage, f, "meeting:"+meetingID); err != nil {
		return nil, err
	}
	if err := validRef("doc_ref", in.DocRef, true); err != nil {
		return nil, err
	}
	if err := validLine("title", in.Title, 200, true); err != nil {
		return nil, err
	}
	if err := validText("note", in.Note, 1000, false); err != nil {
		return nil, err
	}
	if err := validLine("media_type", in.MediaType, 100, false); err != nil {
		return nil, err
	}
	if in.SizeBytes < 0 || (in.SHA256 != "" && !sha256Re.MatchString(in.SHA256)) {
		return nil, invalid("size_bytes must not be negative and sha256 must be 64 hex digits")
	}
	if m.Status == "cancelled" {
		return nil, conflict("the meeting was cancelled")
	}
	id := newID("pki")
	sup := m.PackIssuedOn != ""
	title, err := s.seal(id, "title", in.Title)
	if err != nil {
		return nil, err
	}
	note, err := s.sealNull(id, "note", in.Note)
	if err != nil {
		return nil, err
	}
	var pos int
	err = s.txAudit(ctx, a, "write", "meeting:"+meetingID+"/pack", []string{FieldPack}, func(ctx context.Context, tx *sqlTx) error {
		if err := tx.QueryRowContext(ctx, `SELECT IFNULL(MAX(position),0)+1 FROM pack_items WHERE meeting_id = ?`, meetingID).Scan(&pos); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pack_items (id, meeting_id, doc_ref, sens_title, sens_note, media_type, size_bytes, sha256, position, supplementary, added_by, added_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, meetingID, in.DocRef, title, note, in.MediaType, in.SizeBytes, in.SHA256, pos, b2i(sup), canonSubject(a.Subject), s.now().Unix())
		return err
	})
	if err != nil {
		if isUnique(err) {
			return nil, conflict("this document is already in the pack")
		}
		return nil, err
	}
	return &PackItem{ID: id, DocRef: in.DocRef, Title: in.Title, Note: in.Note, MediaType: in.MediaType, SizeBytes: in.SizeBytes, SHA256: in.SHA256, Position: pos, Supplementary: sup}, nil
}

// IssuePack marks the pack issued today: the list at this point is the pack the members received.
func (s *Service) IssuePack(ctx context.Context, a Actor, meetingID string) (*Meeting, error) {
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, a, ActPackManage, f, "meeting:"+meetingID); err != nil {
		return nil, err
	}
	if m.PackIssuedOn != "" {
		return nil, conflict("the pack was issued on %s", m.PackIssuedOn)
	}
	if m.Status != "scheduled" {
		return nil, conflict("the meeting is %s", m.Status)
	}
	today := s.today()
	err = s.txAudit(ctx, a, "write", "meeting:"+meetingID+"/pack", nil, func(ctx context.Context, tx *sqlTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE meetings SET pack_issued_on = ?, updated_at = ? WHERE id = ? AND pack_issued_on IS NULL`, today, s.now().Unix(), meetingID)
		return err
	})
	m.PackIssuedOn = today
	return m, err
}

// GetPack returns the pack to those who may read it; every item read is audited before it is returned.
func (s *Service) GetPack(ctx context.Context, a Actor, meetingID string) (*Pack, error) {
	ctx, cancel := readCtx(ctx)
	defer cancel()
	m, f, err := s.seeMeeting(ctx, a, meetingID)
	if err != nil {
		return nil, err
	}
	if !Decide(a, ActPackRead, f) {
		s.auditDeny(ctx, a, "read", "meeting:"+meetingID+"/pack", []string{FieldPack})
		return nil, ErrNotFound // the pack is confidential: a caller who cannot read it is not told it exists
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, doc_ref, sens_title, sens_note, media_type, size_bytes, sha256, position, supplementary FROM pack_items WHERE meeting_id = ? ORDER BY position`, meetingID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	p := &Pack{MeetingID: meetingID, IssuedOn: m.PackIssuedOn, Items: []PackItem{}}
	var res []string
	for rows.Next() {
		var it PackItem
		var title, note sql.NullString
		var sup int
		if err := rows.Scan(&it.ID, &it.DocRef, &title, &note, &it.MediaType, &it.SizeBytes, &it.SHA256, &it.Position, &sup); err != nil {
			return nil, err
		}
		if it.Title, err = s.open(it.ID, "title", title); err != nil {
			return nil, err
		}
		if it.Note, err = s.open(it.ID, "note", note); err != nil {
			return nil, err
		}
		it.Supplementary = sup == 1
		p.Items = append(p.Items, it)
		res = append(res, "pack_item:"+it.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.auditReadMany(ctx, a, "read", res, []string{FieldPack}); err != nil {
		return nil, err
	}
	return p, nil
}

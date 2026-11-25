package people

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Page handlers. Each one calls Service methods only; what the caller may see or do is decided
// there (policy.go). A handler never inspects roles.

const (
	maxDirScan    = 5 // pages of 100 people read for one search
	dirPageSize   = 100
	maxDirResults = 50
	maxChain      = 12
	maxHomeItems  = 8
	maxSeriesScan = 20
)

func displayName(p *Person) string {
	n := p.GivenName
	if p.PreferredName != "" {
		n = p.PreferredName
	}
	return strings.TrimSpace(n + " " + p.FamilyName)
}

// namer resolves person ids to display names through GetPerson, so a person the caller may not see
// stays anonymous ("a colleague"). Results are cached for the request.
type namer struct {
	ctx   context.Context
	svc   *Service
	a     Actor
	u     *UI
	cache map[string]*Person
}

func (u *UI) namer(c *uiCtx) *namer {
	return &namer{ctx: c.r.Context(), svc: u.Svc, a: c.a, u: u, cache: map[string]*Person{}}
}

func (n *namer) person(id string) *Person {
	if id == "" {
		return nil
	}
	if p, ok := n.cache[id]; ok {
		return p
	}
	p, err := n.svc.GetPerson(n.ctx, n.a, id)
	if err != nil {
		p = nil
	}
	n.cache[id] = p
	return p
}

func (n *namer) name(id string) string {
	if p := n.person(id); p != nil {
		return displayName(p)
	}
	return n.u.msg("person.unknown")
}

// orgNames maps org unit ids to names (departments, teams, positions) for display.
func (u *UI) orgNames(c *uiCtx) map[string]string {
	out := map[string]string{}
	for _, kind := range []string{"departments", "teams", "positions"} {
		l, err := u.Svc.ListOrgUnits(c.r.Context(), c.a, kind)
		if err != nil {
			continue
		}
		for _, o := range l {
			out[o.ID] = o.Name
		}
	}
	return out
}

func (u *UI) statusLabel(s Status) string { return u.msg("status." + string(s)) }

func (c *uiCtx) form(name string) string { return strings.TrimSpace(c.r.PostForm.Get(name)) }

func (c *uiCtx) redirect(path string) error {
	http.Redirect(c.w, c.r, path, http.StatusSeeOther)
	return nil
}

func esc(id string) string { return url.PathEscape(id) }

// ---- home ----------------------------------------------------------------------------------

type taskRow struct {
	ID, Title, Due, About string
	Overdue               bool
}

type meetingRow struct {
	ID, With, When, Status, Cadence string
}

type goalRow struct {
	ID, Title, Period, Status, Scope, Owner string
	Progress                                int
}

type homeData struct {
	Name           string
	Tasks          []taskRow
	Meetings       []meetingRow
	Goals          []goalRow
	ApprovalsWired bool
	Approvals      []ApprovalItem
}

func (u *UI) home(c *uiCtx) error {
	ctx := c.r.Context()
	n := u.namer(c)
	d := homeData{Name: n.name(c.a.PersonID)}
	today := u.Now().UTC().Format(dateLayout)
	if tasks, err := u.Svc.MyTasks(ctx, c.a); err == nil {
		for i, t := range tasks {
			if i == maxHomeItems*2 {
				break
			}
			row := taskRow{ID: t.ID, Title: t.Title, Due: t.DueDate, Overdue: t.DueDate != "" && t.DueDate < today}
			if t.PersonID != c.a.PersonID {
				row.About = n.name(t.PersonID)
			}
			d.Tasks = append(d.Tasks, row)
		}
	}
	rows, err := u.upcoming(c, n)
	if err != nil {
		return err
	}
	d.Meetings = rows
	if gs, err := u.Svc.ListGoals(ctx, c.a, GoalFilter{OwnerID: c.a.PersonID, Scope: "person"}); err == nil {
		for _, g := range gs {
			if g.Status == "active" && g.Progress < 100 && len(d.Goals) < maxHomeItems {
				d.Goals = append(d.Goals, goalRow{ID: g.ID, Title: g.Title, Period: g.Period, Progress: g.Progress})
			}
		}
	}
	if u.Approvals != nil {
		d.ApprovalsWired = true
		if items, err := u.Approvals.Waiting(ctx, c.ss.subject); err == nil {
			d.Approvals = items
		} else {
			u.Log.Warn("approvals seam failed", "error", err)
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "home.html", "nav.home", d)
	return nil
}

func (u *UI) upcoming(c *uiCtx, n *namer) ([]meetingRow, error) {
	series, err := u.Svc.ListSeries(c.r.Context(), c.a)
	if err != nil {
		return nil, err
	}
	var rows []meetingRow
	cutoff := u.Now().Add(-12 * time.Hour)
	for i, s := range series {
		if i == maxSeriesScan {
			break
		}
		ms, err := u.Svc.ListMeetings(c.r.Context(), c.a, s.ID)
		if err != nil {
			continue
		}
		other := s.ReportID
		if other == c.a.PersonID {
			other = s.ManagerID
		}
		for _, m := range ms {
			at, perr := time.Parse(time.RFC3339, m.ScheduledAt)
			if m.Status == "scheduled" && perr == nil && at.After(cutoff) {
				rows = append(rows, meetingRow{ID: m.ID, With: n.name(other), When: m.ScheduledAt, Status: m.Status, Cadence: s.Cadence})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].When < rows[j].When })
	if len(rows) > 5 {
		rows = rows[:5]
	}
	return rows, nil
}

func (u *UI) completeTask(c *uiCtx) error {
	if _, err := u.Svc.CompleteTask(c.r.Context(), c.a, c.r.PathValue("id")); err != nil {
		return err
	}
	return c.redirect(withOK("/", "task_done"))
}

// ---- profile -------------------------------------------------------------------------------

type personView struct {
	P                               *Person
	Name, Manager, ManagerID        string
	Department, Team, Position      string
	Status                          string
	Reports                         []orgNode
	Goals                           []goalRow
	SeriesID                        string
	Self                            bool
	CanSensitive                    bool
	SensitivePath                   string
	Phone, PreferredName, WorkPhone string
}

func (u *UI) personView(c *uiCtx, p *Person) (*personView, *namer) {
	n := u.namer(c)
	org := u.orgNames(c)
	v := &personView{P: p, Name: displayName(p), Status: u.statusLabel(p.Status), Department: org[p.DepartmentID], Team: org[p.TeamID], Position: org[p.PositionID], Self: p.ID == c.a.PersonID}
	if p.ManagerID != "" {
		if m := n.person(p.ManagerID); m != nil {
			v.Manager, v.ManagerID = displayName(m), m.ID
		}
	}
	return v, n
}

func (u *UI) profile(c *uiCtx) error {
	p, err := u.Svc.GetPerson(c.r.Context(), c.a, c.a.PersonID)
	if err != nil {
		return err
	}
	v, _ := u.personView(c, p)
	_, serr := u.Svc.SensitiveFieldsFor(c.r.Context(), c.a, p.ID)
	v.CanSensitive = serr == nil
	v.SensitivePath = "/me/sensitive"
	u.render(c.w, c.r, c, http.StatusOK, "profile.html", "nav.profile", v)
	return nil
}

func (u *UI) profileSave(c *uiCtx) error {
	pn, wp := c.form("preferred_name"), c.form("work_phone")
	if _, err := u.Svc.UpdatePerson(c.r.Context(), c.a, c.a.PersonID, PersonPatch{PreferredName: &pn, WorkPhone: &wp}); err != nil {
		return err
	}
	return c.redirect(withOK("/me", "saved"))
}

// ---- sensitive screen ----------------------------------------------------------------------

type sensRow struct {
	Field, Label string
	Value        string
	Available    bool
}

type sensData struct {
	Name     string
	Action   string
	Fields   []sensRow
	Revealed bool
	Self     bool
}

func (c *uiCtx) subjectID() string {
	if id := c.r.PathValue("id"); id != "" {
		return id
	}
	return c.a.PersonID
}

func (u *UI) sensitiveBase(c *uiCtx) (string, *Person, []string, error) {
	id := c.subjectID()
	allowed, err := u.Svc.SensitiveFieldsFor(c.r.Context(), c.a, id)
	if err != nil {
		return "", nil, nil, err
	}
	p, err := u.Svc.GetPerson(c.r.Context(), c.a, id)
	if err != nil {
		return "", nil, nil, err
	}
	return id, p, allowed, nil
}

func (u *UI) sensitiveRows(allowed []string, vals map[string]any) []sensRow {
	out := make([]sensRow, 0, len(SensitiveFields))
	for _, f := range SensitiveFields {
		row := sensRow{Field: f, Label: u.msg("sens." + f)}
		for _, a := range allowed {
			if a == f {
				row.Available = true
			}
		}
		if v, ok := vals[f]; ok {
			switch x := v.(type) {
			case nil:
				row.Value = u.msg("sens.not_recorded")
			case Salary:
				row.Value = fmt.Sprintf("%d.%02d %s / %s", x.Amount/100, x.Amount%100, x.Currency, x.Period)
			default:
				row.Value = fmt.Sprint(x)
			}
		}
		out = append(out, row)
	}
	return out
}

// sensitiveNotice shows what could be revealed and that revealing is audited. It reads no
// sensitive value, so it writes no read audit event.
func (u *UI) sensitiveNotice(c *uiCtx) error {
	id, p, allowed, err := u.sensitiveBase(c)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "sensitive.html", "sens.title", sensData{Name: displayName(p), Action: sensPath(c, id), Fields: u.sensitiveRows(allowed, nil), Self: id == c.a.PersonID})
	return nil
}

func sensPath(c *uiCtx, id string) string {
	if id == c.a.PersonID {
		return "/me/sensitive"
	}
	return "/people/" + esc(id) + "/sensitive"
}

// sensitiveReveal reads the permitted fields through GetSensitive, which audits the read per field
// before any value is returned.
func (u *UI) sensitiveReveal(c *uiCtx) error {
	id, p, allowed, err := u.sensitiveBase(c)
	if err != nil {
		return err
	}
	if id != c.a.PersonID && !u.reveals.allow(c.ss.subject) {
		c.w.Header().Set("Retry-After", "30")
		u.renderError(c.w, c.r, c.ss, c.a, http.StatusTooManyRequests, "err.busy_title", "err.busy")
		return nil
	}
	vals, err := u.Svc.GetSensitive(c.r.Context(), c.a, id, nil)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "sensitive.html", "sens.title", sensData{Name: displayName(p), Action: sensPath(c, id), Fields: u.sensitiveRows(allowed, vals), Revealed: true, Self: id == c.a.PersonID})
	return nil
}

// ---- directory and people ------------------------------------------------------------------

type dirRow struct {
	ID, Name, Email, Title, Team, Location, Status string
}

type dirData struct {
	Q         string
	Rows      []dirRow
	Truncated bool
}

func (u *UI) directory(c *uiCtx) error {
	q := strings.TrimSpace(c.r.URL.Query().Get("q"))
	if len([]rune(q)) > 64 {
		q = string([]rune(q)[:64])
	}
	needle := strings.ToLower(q)
	org := u.orgNames(c)
	d := dirData{Q: q}
	var cursor int64
scan:
	for page := 0; page < maxDirScan; page++ {
		items, next, err := u.Svc.ListPeople(c.r.Context(), c.a, PersonFilter{AfterSeq: cursor, Limit: dirPageSize})
		if err != nil {
			return err
		}
		for i := range items {
			p := &items[i]
			hay := strings.ToLower(displayName(p) + " " + p.GivenName + " " + p.FamilyName + " " + p.WorkEmail + " " + p.Location + " " + org[p.TeamID] + " " + org[p.PositionID])
			if needle != "" && !strings.Contains(hay, needle) {
				continue
			}
			if len(d.Rows) == maxDirResults {
				d.Truncated = true
				break scan
			}
			d.Rows = append(d.Rows, dirRow{ID: p.ID, Name: displayName(p), Email: p.WorkEmail, Title: org[p.PositionID], Team: org[p.TeamID], Location: p.Location, Status: u.statusLabel(p.Status)})
		}
		if next == 0 {
			break
		}
		cursor = next
		if page == maxDirScan-1 {
			d.Truncated = true
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "directory.html", "nav.directory", d)
	return nil
}

func (u *UI) person(c *uiCtx) error {
	id := c.r.PathValue("id")
	p, err := u.Svc.GetPerson(c.r.Context(), c.a, id)
	if err != nil {
		return err
	}
	v, n := u.personView(c, p)
	if reps, err := u.Svc.Reports(c.r.Context(), c.a, id); err == nil {
		for _, r := range reps {
			v.Reports = append(v.Reports, orgNode{ID: r.ID, Name: displayName(&r), Status: u.statusLabel(r.Status)})
		}
	}
	if gs, err := u.Svc.ListGoals(c.r.Context(), c.a, GoalFilter{OwnerID: id, Scope: "person"}); err == nil {
		for _, g := range gs {
			if len(v.Goals) < maxHomeItems {
				v.Goals = append(v.Goals, goalRow{ID: g.ID, Title: g.Title, Period: g.Period, Status: g.Status, Progress: g.Progress})
			}
		}
	}
	if !v.Self {
		if series, err := u.Svc.ListSeries(c.r.Context(), c.a); err == nil {
			for _, s := range series {
				if s.ReportID == id || s.ManagerID == id {
					v.SeriesID = s.ID
				}
			}
		}
	}
	_ = n
	_, serr := u.Svc.SensitiveFieldsFor(c.r.Context(), c.a, id)
	v.CanSensitive = serr == nil
	v.SensitivePath = sensPath(c, id)
	u.render(c.w, c.r, c, http.StatusOK, "person.html", "nav.directory", v)
	return nil
}

// ---- org chart and team --------------------------------------------------------------------

type orgNode struct {
	ID, Name, Status string
}

type orgData struct {
	Chain     []orgNode // top of the chain first, ends before Subject
	Subject   orgNode
	Reports   []orgNode
	Truncated bool
}

// org shows the manager chain above a person (bounded to maxChain levels, never revisiting an id,
// so a corrupted cyclic chain ends) and their direct reports. Everything goes through GetPerson
// and Reports, so masking is the directory's: leave shows as active to colleagues.
func (u *UI) org(c *uiCtx) error {
	id := c.r.PathValue("id")
	if id == "" {
		id = c.a.PersonID
	}
	p, err := u.Svc.GetPerson(c.r.Context(), c.a, id)
	if err != nil {
		return err
	}
	d := orgData{Subject: orgNode{ID: p.ID, Name: displayName(p), Status: u.statusLabel(p.Status)}}
	seen := map[string]bool{p.ID: true}
	cur := p
	var chain []orgNode
	for i := 0; i < maxChain && cur.ManagerID != "" && !seen[cur.ManagerID]; i++ {
		m, err := u.Svc.GetPerson(c.r.Context(), c.a, cur.ManagerID)
		if err != nil {
			break // an invisible manager ends the chain
		}
		seen[m.ID] = true
		chain = append(chain, orgNode{ID: m.ID, Name: displayName(m), Status: u.statusLabel(m.Status)})
		cur = m
	}
	if len(chain) == maxChain {
		d.Truncated = true
	}
	for i := len(chain) - 1; i >= 0; i-- {
		d.Chain = append(d.Chain, chain[i])
	}
	reps, err := u.Svc.Reports(c.r.Context(), c.a, id)
	if err != nil {
		return err
	}
	for i := range reps {
		d.Reports = append(d.Reports, orgNode{ID: reps[i].ID, Name: displayName(&reps[i]), Status: u.statusLabel(reps[i].Status)})
	}
	u.render(c.w, c.r, c, http.StatusOK, "org.html", "nav.org", d)
	return nil
}

type teamRow struct {
	ID, Name, Status, SeriesID string
}

func (u *UI) team(c *uiCtx) error {
	reps, err := u.Svc.Reports(c.r.Context(), c.a, c.a.PersonID)
	if err != nil {
		return err
	}
	series, _ := u.Svc.ListSeries(c.r.Context(), c.a)
	byReport := map[string]string{}
	for _, s := range series {
		if s.ManagerID == c.a.PersonID {
			byReport[s.ReportID] = s.ID
		}
	}
	rows := make([]teamRow, 0, len(reps))
	for i := range reps {
		rows = append(rows, teamRow{ID: reps[i].ID, Name: displayName(&reps[i]), Status: u.statusLabel(reps[i].Status), SeriesID: byReport[reps[i].ID]})
	}
	u.render(c.w, c.r, c, http.StatusOK, "team.html", "nav.team", rows)
	return nil
}

// ---- 1:1s ----------------------------------------------------------------------------------

type uiSeriesRow struct {
	ID, With, Cadence string
}

func (u *UI) seriesRows(c *uiCtx, n *namer) ([]uiSeriesRow, map[string]Series, error) {
	series, err := u.Svc.ListSeries(c.r.Context(), c.a)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]uiSeriesRow, 0, len(series))
	byID := map[string]Series{}
	for _, s := range series {
		other := s.ReportID
		if other == c.a.PersonID {
			other = s.ManagerID
		}
		rows = append(rows, uiSeriesRow{ID: s.ID, With: n.name(other), Cadence: u.msg("cadence." + s.Cadence)})
		byID[s.ID] = s
	}
	return rows, byID, nil
}

func (u *UI) seriesList(c *uiCtx) error {
	rows, _, err := u.seriesRows(c, u.namer(c))
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "oneonones.html", "nav.oneonones", rows)
	return nil
}

func (u *UI) seriesCreate(c *uiCtx) error {
	s, err := u.Svc.CreateSeries(c.r.Context(), c.a, c.form("report_id"), c.form("cadence"))
	if err != nil {
		return err
	}
	return c.redirect("/oneonones/" + esc(s.ID))
}

type seriesData struct {
	ID, With, Cadence string
	Meetings          []meetingRow
}

func (u *UI) seriesPage(c *uiCtx) error {
	id := c.r.PathValue("id")
	n := u.namer(c)
	_, byID, err := u.seriesRows(c, n)
	if err != nil {
		return err
	}
	s, ok := byID[id]
	if !ok {
		return ErrNotFound
	}
	ms, err := u.Svc.ListMeetings(c.r.Context(), c.a, id)
	if err != nil {
		return err
	}
	other := s.ReportID
	if other == c.a.PersonID {
		other = s.ManagerID
	}
	d := seriesData{ID: id, With: n.name(other), Cadence: u.msg("cadence." + s.Cadence)}
	for _, m := range ms {
		d.Meetings = append(d.Meetings, meetingRow{ID: m.ID, When: m.ScheduledAt, Status: u.msg("mstatus." + m.Status)})
	}
	u.render(c.w, c.r, c, http.StatusOK, "series.html", "nav.oneonones", d)
	return nil
}

func (u *UI) meetingSchedule(c *uiCtx) error {
	at, err := time.ParseInLocation("2006-01-02T15:04", c.form("scheduled_at"), time.UTC)
	if err != nil {
		return &InvalidError{Msg: u.msg("err.bad_datetime")}
	}
	m, err := u.Svc.ScheduleMeeting(c.r.Context(), c.a, c.r.PathValue("id"), at)
	if err != nil {
		return err
	}
	return c.redirect("/meetings/" + esc(m.ID))
}

type actionRow struct {
	ActionItem
	Assignee string
	Done     bool
}

type agendaRow struct {
	Author, Text string
}

type meetingData struct {
	ID, SeriesID, With, When, Status string
	Agenda                           []agendaRow
	Shared                           string
	HasPrivate                       bool
	Private                          string
	Actions, FollowUps               []actionRow
	Participants                     []orgNode
}

func (u *UI) meetingPage(c *uiCtx) error {
	m, err := u.Svc.GetMeeting(c.r.Context(), c.a, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	n := u.namer(c)
	_, byID, err := u.seriesRows(c, n)
	if err != nil {
		return err
	}
	d := meetingData{ID: m.ID, SeriesID: m.SeriesID, When: m.ScheduledAt, Status: u.msg("mstatus." + m.Status), Shared: m.SharedNotes}
	if s, ok := byID[m.SeriesID]; ok {
		d.Participants = []orgNode{{ID: s.ManagerID, Name: n.name(s.ManagerID)}, {ID: s.ReportID, Name: n.name(s.ReportID)}}
		other := s.ReportID
		if other == c.a.PersonID {
			other = s.ManagerID
		}
		d.With = n.name(other)
	}
	if m.PrivateNotes != nil {
		d.HasPrivate, d.Private = true, *m.PrivateNotes
	}
	for _, a := range m.Agenda {
		d.Agenda = append(d.Agenda, agendaRow{Author: n.name(a.AuthorID), Text: a.Text})
	}
	toRows := func(in []ActionItem) []actionRow {
		var out []actionRow
		for _, a := range in {
			out = append(out, actionRow{ActionItem: a, Assignee: n.name(a.AssigneeID), Done: a.Status == "done"})
		}
		return out
	}
	d.Actions, d.FollowUps = toRows(m.Actions), toRows(m.FollowUps)
	u.render(c.w, c.r, c, http.StatusOK, "meeting.html", "nav.oneonones", d)
	return nil
}

func (u *UI) meetingAgenda(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Svc.AddAgendaItem(c.r.Context(), c.a, id, c.form("text")); err != nil {
		return err
	}
	return c.redirect(withOK("/meetings/"+esc(id), "saved"))
}

func (u *UI) meetingNotes(private bool) uiHandler {
	return func(c *uiCtx) error {
		id := c.r.PathValue("id")
		// Notes are free text: keep the user's line breaks and surrounding space.
		if err := u.Svc.SetNotes(c.r.Context(), c.a, id, c.r.PostForm.Get("text"), private); err != nil {
			return err
		}
		return c.redirect(withOK("/meetings/"+esc(id), "saved"))
	}
}

func (u *UI) meetingAction(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Svc.AddAction(c.r.Context(), c.a, id, c.form("text"), c.form("assignee_id"), c.form("due_date")); err != nil {
		return err
	}
	return c.redirect(withOK("/meetings/"+esc(id), "saved"))
}

func (u *UI) meetingStatus(c *uiCtx) error {
	id := c.r.PathValue("id")
	if err := u.Svc.SetMeetingStatus(c.r.Context(), c.a, id, c.form("status")); err != nil {
		return err
	}
	return c.redirect(withOK("/meetings/"+esc(id), "saved"))
}

func (u *UI) actionStatus(c *uiCtx) error {
	st := c.form("status")
	it, err := u.Svc.UpdateAction(c.r.Context(), c.a, c.r.PathValue("id"), &st, nil)
	if err != nil {
		return err
	}
	return c.redirect(withOK("/meetings/"+esc(it.MeetingID), "saved"))
}

// ---- goals ---------------------------------------------------------------------------------

type goalsData struct {
	Heading string
	Scope   string
	Rows    []goalRow
	Teams   []OrgUnit
	Mine    bool
}

func (u *UI) goalRows(c *uiCtx, gs []Goal, n *namer) []goalRow {
	rows := make([]goalRow, 0, len(gs))
	for _, g := range gs {
		r := goalRow{ID: g.ID, Title: g.Title, Period: g.Period, Status: u.msg("gstatus." + g.Status), Scope: u.msg("scope." + g.Scope), Progress: g.Progress}
		if g.OwnerID != "" {
			r.Owner = n.name(g.OwnerID)
		}
		rows = append(rows, r)
	}
	return rows
}

func (u *UI) goals(c *uiCtx) error {
	q := c.r.URL.Query()
	n := u.namer(c)
	flt := GoalFilter{Scope: "person", OwnerID: c.a.PersonID}
	d := goalsData{Heading: u.msg("goals.mine"), Scope: "person", Mine: true}
	switch {
	case q.Get("scope") == "team" || q.Get("scope") == "company":
		flt = GoalFilter{Scope: q.Get("scope")}
		d.Heading, d.Scope, d.Mine = u.msg("goals."+q.Get("scope")), q.Get("scope"), false
	case q.Get("owner") != "" && q.Get("owner") != c.a.PersonID:
		flt.OwnerID = q.Get("owner")
		d.Heading, d.Mine = u.msg("goals.of", n.name(flt.OwnerID)), false
	}
	gs, err := u.Svc.ListGoals(c.r.Context(), c.a, flt)
	if err != nil {
		return err
	}
	d.Rows = u.goalRows(c, gs, n)
	if l, err := u.Svc.ListOrgUnits(c.r.Context(), c.a, "teams"); err == nil {
		d.Teams = l
	}
	u.render(c.w, c.r, c, http.StatusOK, "goals.html", "nav.goals", d)
	return nil
}

func (u *UI) goalCreate(c *uiCtx) error {
	g, err := u.Svc.CreateGoal(c.r.Context(), c.a, GoalInput{Scope: c.form("scope"), TeamID: c.form("team_id"), Title: c.form("title"), Description: c.r.PostForm.Get("description"),
		Period: c.form("period"), Visibility: c.form("visibility"), Status: c.form("status"), ParentID: c.form("parent_id")})
	if err != nil {
		return err
	}
	return c.redirect(withOK("/goals/"+esc(g.ID), "saved"))
}

type updateRow struct {
	Author, Comment, When string
	Value                 *float64
}

type goalData struct {
	G          *Goal
	Owner      string
	Scope      string
	Visibility string
	Status     string
	Parent     *goalRow
	Children   []goalRow
	Updates    []updateRow
	CanWrite   bool
	Statuses   []string
	Visibs     []string
	Candidates []goalRow
}

func (u *UI) goal(c *uiCtx) error {
	id := c.r.PathValue("id")
	g, err := u.Svc.GetGoal(c.r.Context(), c.a, id)
	if err != nil {
		return err
	}
	n := u.namer(c)
	d := goalData{G: g, Scope: u.msg("scope." + g.Scope), Visibility: u.msg("vis." + g.Visibility), Status: u.msg("gstatus." + g.Status),
		CanWrite: u.Svc.CanWriteGoal(c.r.Context(), c.a, id), Statuses: goalStatuses, Visibs: goalVisibilities}
	if g.OwnerID != "" {
		d.Owner = n.name(g.OwnerID)
	}
	if g.ParentID != "" {
		if pg, err := u.Svc.GetGoal(c.r.Context(), c.a, g.ParentID); err == nil {
			r := u.goalRows(c, []Goal{*pg}, n)[0]
			d.Parent = &r
		}
	}
	if ch, err := u.Svc.ListGoals(c.r.Context(), c.a, GoalFilter{ParentID: id}); err == nil {
		d.Children = u.goalRows(c, ch, n)
	}
	if ups, err := u.Svc.ListUpdates(c.r.Context(), c.a, id); err == nil {
		for i, up := range ups {
			if i == 30 {
				break
			}
			d.Updates = append(d.Updates, updateRow{Author: n.name(up.AuthorID), Comment: up.Comment, When: up.CreatedAt, Value: up.Value})
		}
	}
	if d.CanWrite {
		for _, sc := range []string{"company", "team"} {
			if l, err := u.Svc.ListGoals(c.r.Context(), c.a, GoalFilter{Scope: sc}); err == nil {
				for _, x := range u.goalRows(c, l, n) {
					if x.ID != id {
						d.Candidates = append(d.Candidates, x)
					}
				}
			}
		}
		if len(d.Candidates) > 50 {
			d.Candidates = d.Candidates[:50]
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "goal.html", "nav.goals", d)
	return nil
}

func (u *UI) goalUpdate(c *uiCtx) error {
	id := c.r.PathValue("id")
	var p GoalPatch
	set := func(name string, dst **string) {
		if _, ok := c.r.PostForm[name]; ok {
			v := c.form(name)
			*dst = &v
		}
	}
	set("title", &p.Title)
	set("period", &p.Period)
	set("visibility", &p.Visibility)
	set("status", &p.Status)
	set("parent_id", &p.ParentID)
	if _, ok := c.r.PostForm["description"]; ok {
		v := c.r.PostForm.Get("description")
		p.Description = &v
	}
	if _, err := u.Svc.UpdateGoal(c.r.Context(), c.a, id, p); err != nil {
		return err
	}
	return c.redirect(withOK("/goals/"+esc(id), "saved"))
}

func (u *UI) number(c *uiCtx, name string) (float64, error) {
	v, err := strconv.ParseFloat(c.form(name), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, &InvalidError{Msg: u.msg("err.bad_number")}
	}
	return v, nil
}

func (u *UI) krCreate(c *uiCtx) error {
	id := c.r.PathValue("id")
	start, err := u.number(c, "start_value")
	if err != nil {
		return err
	}
	target, err := u.number(c, "target_value")
	if err != nil {
		return err
	}
	if _, err := u.Svc.AddKeyResult(c.r.Context(), c.a, id, KeyResultInput{Title: c.form("title"), Unit: c.form("unit"), Start: start, Target: target}); err != nil {
		return err
	}
	return c.redirect(withOK("/goals/"+esc(id), "saved"))
}

func (u *UI) krProgress(c *uiCtx) error {
	v, err := u.number(c, "value")
	if err != nil {
		return err
	}
	g, err := u.Svc.RecordProgress(c.r.Context(), c.a, c.r.PathValue("id"), v, c.r.PostForm.Get("comment"))
	if err != nil {
		return err
	}
	return c.redirect(withOK("/goals/"+esc(g.ID), "saved"))
}

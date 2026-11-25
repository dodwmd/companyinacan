package people

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Time off and reviews pages (Service methods of E14b). As elsewhere, every decision is the
// Service's; the handlers only fetch, shape and render. Two rules are about what is NOT drawn:
//   - the calendar shows colleagues as "out" and nothing else, because the service gives colleagues
//     nothing else; type and status appear only when the service returned them;
//   - while a review cycle is open, feedback pages draw no response counts and no per-reviewer
//     state, even when the service returns counts to the viewer (anonymity).

const (
	maxCalendarDays = 92
	maxListRows     = 100
	maxAnswerKeys   = 64
)

var (
	reviewIDRe     = regexp.MustCompile(`^rev_[0-9a-f]{24}$`)
	errBadAnswers  = &InvalidError{Msg: "the answers could not be read"}
	errBadDate     = &InvalidError{Msg: "enter a valid date"}
	leaveDecisions = map[string]bool{"approve": true, "reject": false}
)

// soft ignores "you may not see that" answers for optional page sections.
func soft(err error) error {
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (u *UI) partLabel(part string) string {
	switch part {
	case "pm":
		return u.msg("part.pm")
	case "am":
		return u.msg("part.am")
	}
	return ""
}

// ---- time off -------------------------------------------------------------------------------

type uiLeaveRow struct {
	ID, Person, Type, Status, Start, End, Days string
	StatusKey                                  string
}

type balanceRow struct {
	Type, Days, Pending string
}

type timeoffData struct {
	Types    []LeaveType
	Balances []balanceRow
	Mine     []uiLeaveRow
}

func halfText(halves int) string {
	d := strconv.Itoa(halves / 2)
	if halves%2 == 1 {
		return d + ".5"
	}
	return d
}

func (u *UI) leaveRow(r LeaveRequest, person string) uiLeaveRow {
	start, end := fmtDate(r.StartDate), fmtDate(r.EndDate)
	if p := u.partLabel(r.StartPart); p != "" {
		start += " (" + p + ")"
	}
	if p := u.partLabel(r.EndPart); p != "" {
		end += " (" + p + ")"
	}
	return uiLeaveRow{ID: r.ID, Person: person, Type: r.Type, Status: u.msg("lstatus." + r.Status), StatusKey: r.Status, Start: start, End: end, Days: halfText(r.Halves)}
}

func (u *UI) timeoff(c *uiCtx) error {
	ctx := c.r.Context()
	d := timeoffData{}
	types, err := u.Svc.ListLeaveTypes(ctx, c.a)
	if err := soft(err); err != nil {
		return err
	}
	for _, t := range types {
		if t.Active {
			d.Types = append(d.Types, t)
		}
	}
	bals, err := u.Svc.LeaveBalances(ctx, c.a, c.a.PersonID, "")
	if err := soft(err); err != nil {
		return err
	}
	for _, b := range bals {
		d.Balances = append(d.Balances, balanceRow{Type: b.Type, Days: halfText(b.Halves), Pending: halfText(b.Reserved)})
	}
	mine, err := u.Svc.ListLeaveRequests(ctx, c.a, LeaveFilter{PersonID: c.a.PersonID})
	if err := soft(err); err != nil {
		return err
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].StartDate > mine[j].StartDate })
	for i, r := range mine {
		if i == maxListRows {
			break
		}
		d.Mine = append(d.Mine, u.leaveRow(r, ""))
	}
	u.render(c.w, c.r, c, http.StatusOK, "timeoff.html", "nav.timeoff", d)
	return nil
}

func (u *UI) timeoffCreate(c *uiCtx) error {
	ctx := c.r.Context()
	lr, err := u.Svc.CreateLeaveRequest(ctx, c.a, LeaveRequestIn{TypeID: c.form("type_id"), StartDate: c.form("start_date"), EndDate: c.form("end_date"),
		StartPart: c.form("start_part"), EndPart: c.form("end_part"), Note: c.r.PostForm.Get("note")})
	if err != nil {
		return err
	}
	if c.form("action") == "submit" {
		if _, err := u.Svc.SubmitLeaveRequest(ctx, c.a, lr.ID); err != nil {
			return err
		}
	}
	return c.redirect(withOK("/timeoff/"+esc(lr.ID), "saved"))
}

type leaveDetail struct {
	R                          LeaveRequest
	Row                        uiLeaveRow
	Person                     string
	Self, CanDecide, CanCancel bool
	CanSubmit                  bool
}

func (u *UI) timeoffDetail(c *uiCtx) error {
	lr, err := u.Svc.GetLeaveRequest(c.r.Context(), c.a, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	n := u.namer(c)
	self := lr.PersonID == c.a.PersonID
	d := leaveDetail{R: *lr, Person: n.name(lr.PersonID), Self: self}
	d.Row = u.leaveRow(*lr, d.Person)
	d.CanSubmit = self && lr.Status == "draft"
	d.CanCancel = lr.Status == "draft" || lr.Status == "pending" || lr.Status == "approved"
	d.CanDecide = !self && lr.Status == "pending" // the Service still decides who may
	u.render(c.w, c.r, c, http.StatusOK, "leave.html", "nav.timeoff", d)
	return nil
}

func (u *UI) timeoffSubmit(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Svc.SubmitLeaveRequest(c.r.Context(), c.a, id); err != nil {
		return err
	}
	return c.redirect(withOK("/timeoff/"+esc(id), "saved"))
}

func (u *UI) timeoffCancel(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Svc.CancelLeaveRequest(c.r.Context(), c.a, id); err != nil {
		return err
	}
	return c.redirect(withOK("/timeoff/"+esc(id), "saved"))
}

func (u *UI) timeoffDecide(c *uiCtx) error {
	id := c.r.PathValue("id")
	approve, ok := leaveDecisions[c.form("decision")]
	if !ok {
		return &InvalidError{Msg: u.msg("err.bad_form")}
	}
	if _, err := u.Svc.DecideLeaveRequest(c.r.Context(), c.a, id, approve); err != nil {
		return err
	}
	return c.redirect(withOK("/timeoff/queue", "saved"))
}

func (u *UI) timeoffQueue(c *uiCtx) error {
	all, err := u.Svc.ListLeaveRequests(c.r.Context(), c.a, LeaveFilter{Status: "pending"})
	if err := soft(err); err != nil {
		return err
	}
	n := u.namer(c)
	var rows []uiLeaveRow
	for _, r := range all {
		if r.PersonID == c.a.PersonID || len(rows) == maxListRows {
			continue // one's own requests are decided by someone else
		}
		rows = append(rows, u.leaveRow(r, n.name(r.PersonID)))
	}
	u.render(c.w, c.r, c, http.StatusOK, "queue.html", "timeoff.queue", rows)
	return nil
}

type calRow struct {
	Person, Start, End, Type, Status string
	Detail                           bool
}

type calData struct {
	From, To string
	Days     int
	Rows     []calRow
}

func (u *UI) calendar(c *uiCtx) error {
	q := c.r.URL.Query()
	from := q.Get("from")
	if from == "" {
		from = u.Now().UTC().Format(dateLayout)
	}
	days, _ := strconv.Atoi(q.Get("days"))
	if days < 1 {
		days = 28
	}
	days = min(days, maxCalendarDays)
	t0, err := time.Parse(dateLayout, from)
	if err != nil {
		return errBadDate
	}
	to := t0.AddDate(0, 0, days-1).Format(dateLayout)
	entries, err := u.Svc.LeaveCalendar(c.r.Context(), c.a, from, to, "")
	if err != nil {
		return err
	}
	names := map[string]string{}
	if ts, err := u.Svc.ListLeaveTypes(c.r.Context(), c.a); err == nil {
		for _, t := range ts {
			names[t.Key] = t.Name
		}
	}
	n := u.namer(c)
	d := calData{From: from, To: to, Days: days}
	for _, e := range entries {
		if len(d.Rows) == 300 {
			break
		}
		row := calRow{Person: n.name(e.PersonID), Start: fmtDate(e.StartDate), End: fmtDate(e.EndDate)}
		if p := u.partLabel(e.StartPart); p != "" {
			row.Start += " (" + p + ")"
		}
		if p := u.partLabel(e.EndPart); p != "" {
			row.End += " (" + p + ")"
		}
		if e.Type != "" { // the Service returned it, so the viewer may see it
			row.Detail = true
			row.Type = e.Type
			if nm, ok := names[e.Type]; ok {
				row.Type = nm
			}
			row.Status = u.msg("lstatus." + e.Status)
		}
		d.Rows = append(d.Rows, row)
	}
	u.render(c.w, c.r, c, http.StatusOK, "calendar.html", "timeoff.calendar", d)
	return nil
}

// ---- reviews ---------------------------------------------------------------------------------

type reviewRow struct {
	ID, Person, Cycle, State, Status string
}

type reviewsData struct {
	Rows []reviewRow
}

func (u *UI) reviews(c *uiCtx) error {
	ctx := c.r.Context()
	list, err := u.Svc.ListReviews(ctx, c.a, "")
	if err := soft(err); err != nil {
		return err
	}
	cycles := map[string]string{}
	if cs, err := u.Svc.ListCycles(ctx, c.a); err == nil {
		for _, cy := range cs {
			cycles[cy.ID] = cy.Name
		}
	}
	n := u.namer(c)
	d := reviewsData{}
	for _, r := range list {
		if len(d.Rows) == maxListRows {
			break
		}
		d.Rows = append(d.Rows, reviewRow{ID: r.ID, Person: n.name(r.PersonID), Cycle: cycles[r.CycleID], State: u.msg("cstate." + r.CycleState), Status: u.msg("rstatus." + r.Status)})
	}
	u.render(c.w, c.r, c, http.StatusOK, "reviews.html", "nav.reviews", d)
	return nil
}

type qField struct {
	ID, Text, Value string
	Rating          bool
	Max             []int
}

type qaRow struct {
	Question, Answer string
}

type ratingRow struct {
	Question string
	Average  string
	N        int
}

type commentRow struct {
	Question, Text, Reviewer string
}

type feedbackData struct {
	Title    string
	Shown    bool
	Required int
	Ratings  []ratingRow
	Comments []commentRow
}

type nomRow struct {
	ID, Reviewer, Kind, Status string
	Decide                     bool
}

type personOpt struct{ ID, Name string }

type reviewData struct {
	ID, Person, PersonID, Cycle, State, Status string
	Anonymous                                  bool
	Caps                                       ReviewCaps
	SelfForm, MgrForm                          []qField
	SelfRead, MgrRead                          []qaRow
	MgrRating                                  string
	MgrSummary                                 string
	RatingChoices                              []int
	HasManager                                 bool
	CalRating                                  string
	CalNotes, CalRejected                      string
	HasCalibration                             bool
	OutcomeRating                              int
	OutcomeSummary                             string
	HasOutcome, Released                       bool
	Feedback                                   []feedbackData
	Nominations                                []nomRow
	Candidates                                 []personOpt
	AckAt                                      string
	Acked                                      bool
	Self                                       bool
}

func ratingChoices(max int) []int {
	out := make([]int, 0, max)
	for i := 1; i <= max; i++ {
		out = append(out, i)
	}
	return out
}

func buildFields(qs []Question, existing Answers, max int) []qField {
	var out []qField
	for _, q := range qs {
		f := qField{ID: q.ID, Text: q.Text, Rating: q.Type == "rating"}
		if f.Rating {
			f.Max = ratingChoices(max)
		}
		if a, ok := existing[q.ID]; ok {
			if a.Rating != nil {
				f.Value = strconv.Itoa(*a.Rating)
			} else {
				f.Value = a.Text
			}
		}
		out = append(out, f)
	}
	return out
}

func buildRead(qs []Question, ans Answers) []qaRow {
	var out []qaRow
	for _, q := range qs {
		a, ok := ans[q.ID]
		if !ok {
			continue
		}
		v := a.Text
		if a.Rating != nil {
			v = strconv.Itoa(*a.Rating)
		}
		out = append(out, qaRow{Question: q.Text, Answer: v})
	}
	return out
}

func (u *UI) review(c *uiCtx) error {
	ctx := c.r.Context()
	id := c.r.PathValue("id")
	v, err := u.Svc.GetReview(ctx, c.a, id)
	if err != nil {
		return err
	}
	cy, cerr := u.Svc.GetCycle(ctx, c.a, v.CycleID)
	if cerr != nil {
		cy = &Cycle{Questions: v.Questions, RatingMax: defaultRatingMax}
	}
	n := u.namer(c)
	caps := u.Svc.ReviewCapabilities(ctx, c.a, id)
	d := reviewData{ID: id, Person: n.name(v.PersonID), PersonID: v.PersonID, Cycle: cy.Name, State: u.msg("cstate." + v.CycleState), Status: u.msg("rstatus." + v.Status),
		Anonymous: cy.Anonymity == "anonymous", Caps: caps, Self: v.PersonID == c.a.PersonID, RatingChoices: ratingChoices(max(cy.RatingMax, 1))}
	qText := map[string]string{}
	for _, q := range cy.Questions {
		qText[q.ID] = q.Text
	}
	var selfQs, mgrQs []Question
	for _, q := range cy.Questions {
		if q.Audience == "all" || q.Audience == "self" {
			selfQs = append(selfQs, q)
		}
		if q.Audience == "all" || q.Audience == "manager" {
			mgrQs = append(mgrQs, q)
		}
	}
	if caps.SelfWrite {
		var cur Answers
		if v.Self != nil {
			cur = v.Self.Answers
		}
		d.SelfForm = buildFields(selfQs, cur, cy.RatingMax)
	} else if v.Self != nil {
		d.SelfRead = buildRead(selfQs, v.Self.Answers)
	}
	if caps.ManagerWrite {
		var cur Answers
		if v.Manager != nil {
			cur = v.Manager.Answers
			if v.Manager.Rating != nil {
				d.MgrRating = strconv.Itoa(*v.Manager.Rating)
			}
			d.MgrSummary = v.Manager.Summary
		}
		d.MgrForm = buildFields(mgrQs, cur, cy.RatingMax)
	} else if v.Manager != nil {
		d.HasManager = true
		d.MgrRead = buildRead(mgrQs, v.Manager.Answers)
		if v.Manager.Rating != nil {
			d.MgrRating = strconv.Itoa(*v.Manager.Rating)
		}
		d.MgrSummary = v.Manager.Summary
	}
	// Calibration is hr's working data. The Service never returns it to the subject; the check on
	// the subject here is a second lock on the same door, not an authorisation rule.
	if v.Calibration != nil && !d.Self {
		d.HasCalibration = true
		if v.Calibration.Rating != nil {
			d.CalRating = strconv.Itoa(*v.Calibration.Rating)
		}
		d.CalNotes, d.CalRejected = v.Calibration.Notes, v.Calibration.LastRejection
	}
	if v.Outcome != nil {
		d.HasOutcome, d.OutcomeRating, d.OutcomeSummary, d.Released = true, v.Outcome.Rating, v.Outcome.Summary, v.Outcome.Released
	}
	open := v.CycleState == "open"
	for _, fv := range []*FeedbackView{v.Peer, v.Upward} {
		if fv == nil {
			continue
		}
		f := feedbackData{Title: u.msg("kind." + fv.Kind), Shown: fv.Shown, Required: fv.Required}
		for _, r := range fv.Ratings {
			f.Ratings = append(f.Ratings, ratingRow{Question: qText[r.QuestionID], Average: strconv.FormatFloat(r.Average, 'f', 1, 64), N: r.N})
		}
		for _, cm := range fv.Comments {
			row := commentRow{Question: qText[cm.QuestionID], Text: cm.Text}
			if cm.ReviewerID != "" {
				row.Reviewer = n.name(cm.ReviewerID)
			}
			f.Comments = append(f.Comments, row)
		}
		// While the cycle is open nothing is drawn but "not yet": no counts, no state per reviewer.
		if open {
			f.Shown, f.Ratings, f.Comments = false, nil, nil
		}
		d.Feedback = append(d.Feedback, f)
	}
	if v.AcknowledgedAt != "" {
		d.Acked, d.AckAt = true, v.AcknowledgedAt
	}
	if caps.Nominate || caps.NominationDecide || d.Self {
		if noms, err := u.Svc.ListNominations(ctx, c.a, id); err == nil {
			for _, nm := range noms {
				if len(d.Nominations) == maxListRows {
					break
				}
				d.Nominations = append(d.Nominations, nomRow{ID: nm.ID, Reviewer: n.name(nm.ReviewerID), Kind: u.msg("kind." + nm.Kind), Status: u.msg("nstatus." + nm.Status), Decide: caps.NominationDecide && nm.Status == "proposed"})
			}
		}
	}
	if caps.Nominate {
		if people, _, err := u.Svc.ListPeople(ctx, c.a, PersonFilter{Limit: dirPageSize}); err == nil {
			for i := range people {
				if people[i].ID != v.PersonID {
					d.Candidates = append(d.Candidates, personOpt{ID: people[i].ID, Name: displayName(&people[i])})
				}
			}
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "review.html", "nav.reviews", d)
	return nil
}

// answersFromForm reads the answer fields: r_<question id> for ratings, t_<question id> for text.
// Types and ids are checked by the Service against the cycle's questions.
func (u *UI) answersFromForm(pf url.Values) (Answers, error) {
	if len(pf) > maxAnswerKeys+8 {
		return nil, errBadAnswers
	}
	out := Answers{}
	for key, vals := range pf {
		if len(vals) == 0 {
			continue
		}
		v := strings.TrimSpace(vals[0])
		switch {
		case strings.HasPrefix(key, "r_") && v != "":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, &InvalidError{Msg: u.msg("err.bad_number")}
			}
			out[key[2:]] = Answer{Rating: &n}
		case strings.HasPrefix(key, "t_") && v != "":
			out[key[2:]] = Answer{Text: v}
		}
	}
	return out, nil
}

func (u *UI) reviewSelf(c *uiCtx) error {
	id := c.r.PathValue("id")
	ans, err := u.answersFromForm(c.r.PostForm)
	if err != nil {
		return err
	}
	if err := u.Svc.SaveSelf(c.r.Context(), c.a, id, ans, c.form("submit") != ""); err != nil {
		return err
	}
	return c.redirect(withOK("/reviews/"+esc(id), "saved"))
}

func (u *UI) reviewManager(c *uiCtx) error {
	id := c.r.PathValue("id")
	ans, err := u.answersFromForm(c.r.PostForm)
	if err != nil {
		return err
	}
	in := ManagerIn{Answers: ans, Summary: c.r.PostForm.Get("summary"), Submit: c.form("submit") != ""}
	if r := c.form("rating"); r != "" {
		n, err := strconv.Atoi(r)
		if err != nil {
			return &InvalidError{Msg: u.msg("err.bad_number")}
		}
		in.Rating = &n
	}
	if err := u.Svc.SaveManager(c.r.Context(), c.a, id, in); err != nil {
		return err
	}
	return c.redirect(withOK("/reviews/"+esc(id), "saved"))
}

func (u *UI) reviewAck(c *uiCtx) error {
	id := c.r.PathValue("id")
	if err := u.Svc.Acknowledge(c.r.Context(), c.a, id, c.r.PostForm.Get("comment")); err != nil {
		return err
	}
	return c.redirect(withOK("/reviews/"+esc(id), "saved"))
}

func (u *UI) nominate(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Svc.Nominate(c.r.Context(), c.a, id, c.form("reviewer_id"), c.form("kind")); err != nil {
		return err
	}
	return c.redirect(withOK("/reviews/"+esc(id), "saved"))
}

func (u *UI) nominationDecide(c *uiCtx) error {
	approve, ok := leaveDecisions[c.form("decision")]
	if !ok {
		return &InvalidError{Msg: u.msg("err.bad_form")}
	}
	if err := u.Svc.DecideNomination(c.r.Context(), c.a, c.r.PathValue("id"), approve); err != nil {
		return err
	}
	back := "/reviews"
	if rid := c.form("review_id"); reviewIDRe.MatchString(rid) {
		back += "/" + rid
	}
	return c.redirect(withOK(back, "saved"))
}

// ---- feedback requests (the reviewer's inbox) ------------------------------------------------

type feedbackRow struct {
	ID, Subject, Kind string
	Anonymous         bool
	Fields            []qField
}

func (u *UI) feedbackRows(c *uiCtx) ([]feedbackRow, error) {
	reqs, err := u.Svc.MyFeedbackRequests(c.r.Context(), c.a)
	if err != nil {
		return nil, err
	}
	n := u.namer(c)
	var out []feedbackRow
	for _, r := range reqs {
		out = append(out, feedbackRow{ID: r.ID, Subject: n.name(r.SubjectID), Kind: u.msg("kind." + r.Kind), Anonymous: r.Anonymous, Fields: buildFields(r.Questions, nil, defaultRatingMax)})
	}
	return out, nil
}

func (u *UI) feedbackInbox(c *uiCtx) error {
	rows, err := u.feedbackRows(c)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "feedback.html", "nav.feedback", rows)
	return nil
}

func (u *UI) feedbackForm(c *uiCtx) error {
	rows, err := u.feedbackRows(c)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == c.r.PathValue("id") {
			u.render(c.w, c.r, c, http.StatusOK, "feedback_form.html", "nav.feedback", r)
			return nil
		}
	}
	return ErrNotFound
}

func (u *UI) feedbackSubmit(c *uiCtx) error {
	ans, err := u.answersFromForm(c.r.PostForm)
	if err != nil {
		return err
	}
	if err := u.Svc.SubmitFeedback(c.r.Context(), c.a, c.r.PathValue("id"), ans); err != nil {
		return err
	}
	return c.redirect(withOK("/feedback", "sent"))
}

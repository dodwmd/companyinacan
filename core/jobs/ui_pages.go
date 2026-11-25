package jobs

import (
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ---- shared view helpers -------------------------------------------------------------------

type stageView struct{ Key, Name, Kind string }

// stageViews loads the pipeline once per request.
func (u *UI) stageViews(c *uiCtx) ([]stageView, error) {
	ss, err := u.Store.ListStages(c.ctx())
	if err != nil {
		return nil, err
	}
	out := make([]stageView, len(ss))
	for i, s := range ss {
		out[i] = stageView{s.Key, s.Name, s.Kind}
	}
	return out, nil
}

func stageName(ss []stageView, key string) string {
	for _, s := range ss {
		if s.Key == key {
			return s.Name
		}
	}
	return key
}

func cursorParam(c *uiCtx) int64 {
	n, _ := strconv.ParseInt(c.r.URL.Query().Get("cursor"), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

func (u *UI) redirect(c *uiCtx, path, flash string) error {
	if flash != "" {
		path = withOK(path, flash)
	}
	http.Redirect(c.w, c.r, path, http.StatusSeeOther)
	return nil
}

// funnelRow is one stage of a funnel.
type funnelRow struct {
	Name  string
	Count int
}

func funnelRows(ss []stageView, f *Funnel) []funnelRow {
	var out []funnelRow
	for _, s := range ss {
		if s.Kind == KindHired {
			continue
		}
		out = append(out, funnelRow{Name: s.Name, Count: f.Active[s.Key]})
	}
	return out
}

// ---- home ----------------------------------------------------------------------------------

type homeJob struct {
	Job    *Job
	Active int
}

type homeData struct {
	Jobs       []homeJob
	Funnel     []funnelRow
	Hired      int
	Rejected   int
	Interviews []Interview
	Jobsboard  bool
}

func (u *UI) home(c *uiCtx) error {
	caps := c.p.Caps()
	d := homeData{}
	if caps.ReadJobs {
		d.Jobsboard = true
		ss, err := u.stageViews(c)
		if err != nil {
			return err
		}
		f, err := u.Store.FunnelCounts(c.ctx(), c.p, "")
		if err != nil {
			return err
		}
		d.Funnel, d.Hired, d.Rejected = funnelRows(ss, f), f.Hired, f.Rejected
		js, _, err := u.Store.ListJobs(c.ctx(), c.p, JobFilter{Limit: 20})
		if err != nil {
			return err
		}
		for _, j := range js {
			jf, err := u.Store.FunnelCounts(c.ctx(), c.p, j.ID)
			if err != nil {
				return err
			}
			n := 0
			for _, v := range jf.Active {
				n += v
			}
			d.Jobs = append(d.Jobs, homeJob{Job: j, Active: n})
		}
	} else if caps.Score {
		is, _, err := u.Store.ListMyInterviews(c.ctx(), c.p, 0, listLimit)
		if err != nil {
			return err
		}
		d.Interviews = is
	}
	u.render(c.w, c.r, c, http.StatusOK, "home.html", "home.title", d)
	return nil
}

func (u *UI) interviews(c *uiCtx) error {
	cur := cursorParam(c)
	is, next, err := u.Store.ListMyInterviews(c.ctx(), c.p, cur, listLimit)
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "interviews.html", "interviews.title", struct {
		Items []Interview
		Next  int64
	}{is, next})
	return nil
}

// ---- requisitions --------------------------------------------------------------------------

type jobListData struct {
	Jobs   []*Job
	Status string
	Next   int64
}

func (u *UI) jobList(c *uiCtx) error {
	status := c.r.URL.Query().Get("status")
	js, next, err := u.Store.ListJobs(c.ctx(), c.p, JobFilter{Status: status, Cursor: cursorParam(c), Limit: listLimit})
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "jobs.html", "jobs.title", jobListData{Jobs: js, Status: status, Next: next})
	return nil
}

type sectionField struct {
	N             int
	Heading, Body string
}

type jobForm struct {
	ID, Version                                      string
	Title, Department, City, Region, Country, Postal string
	Remote, Employment, Countries                    string
	SalaryMin, SalaryMax, Currency, Period           string
	PublishSalary                                    bool
	HiringManager, Headcount, ValidThrough           string
	Sections                                         []sectionField
	Remotes, Employments, Periods                    []string
	Edit                                             bool
}

const maxSections = 20

func newJobForm() jobForm {
	f := jobForm{Remote: "onsite", Employment: "full_time", Headcount: "1", Currency: "USD", Period: "year"}
	f.Remotes = RemoteKinds
	for k := range EmploymentTypes {
		f.Employments = append(f.Employments, k)
	}
	slices.Sort(f.Employments)
	for k := range SalaryPeriods {
		f.Periods = append(f.Periods, k)
	}
	slices.Sort(f.Periods)
	return f
}

func padSections(ss []sectionField, extra int) []sectionField {
	for len(ss) < maxSections && extra > 0 {
		ss = append(ss, sectionField{N: len(ss)})
		extra--
	}
	return ss
}

func (u *UI) jobNew(c *uiCtx) error {
	f := newJobForm()
	f.Sections = padSections(nil, 3)
	if _, err := u.Store.ListStages(c.ctx()); err != nil { // keeps the page bounded by the same deadline
		return err
	}
	return u.renderJobForm(c, f)
}

func (u *UI) renderJobForm(c *uiCtx, f jobForm) error {
	// the form is drawn for anyone the Store would let write jobs; saving asks the Store again
	if err := u.Store.Require(c.ctx(), c.p, ActJobWrite); err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "jobform.html", "jobform.title", f)
	return nil
}

func (u *UI) jobEdit(c *uiCtx) error {
	j, err := u.Store.GetJob(c.ctx(), c.p, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	f := newJobForm()
	f.Edit, f.ID, f.Version = true, j.ID, strconv.FormatInt(j.Version, 10)
	f.Title, f.Department, f.City, f.Region, f.Country, f.Postal = j.Title, j.Department, j.Location.City, j.Location.Region, j.Location.Country, j.Location.PostalCode
	f.Remote, f.Employment, f.Countries = j.Remote, j.EmploymentType, strings.Join(j.ApplicantCountries, ", ")
	if j.Salary != nil {
		f.SalaryMin, f.SalaryMax, f.Currency, f.Period = fmtMoney(j.Salary.Min), fmtMoney(j.Salary.Max), j.Salary.Currency, j.Salary.Period
	}
	f.PublishSalary, f.HiringManager, f.Headcount = j.PublishSalary, j.HiringManager, strconv.Itoa(j.Headcount)
	if j.ValidThrough != nil {
		f.ValidThrough = j.ValidThrough.Format("2006-01-02")
	}
	for i, s := range j.Sections {
		f.Sections = append(f.Sections, sectionField{N: i, Heading: s.Heading, Body: s.Body})
	}
	f.Sections = padSections(f.Sections, 2)
	return u.renderJobForm(c, f)
}

// formSalary reads the pay range fields: both empty means no salary.
func formSalary(c *uiCtx) (*Salary, error) {
	v := c.r.PostForm
	lo, hi := strings.TrimSpace(v.Get("salary_min")), strings.TrimSpace(v.Get("salary_max"))
	if lo == "" && hi == "" {
		return nil, nil
	}
	min, err1 := strconv.ParseFloat(lo, 64)
	max, err2 := strconv.ParseFloat(hi, 64)
	if err1 != nil || err2 != nil {
		return nil, invalid("salary", "minimum and maximum must be numbers")
	}
	return &Salary{Min: min, Max: max, Currency: strings.ToUpper(strings.TrimSpace(v.Get("salary_currency"))), Period: v.Get("salary_period")}, nil
}

func formSections(c *uiCtx) []Section {
	var out []Section
	for i := 0; i < maxSections; i++ {
		h, b := strings.TrimSpace(c.r.PostForm.Get("section_heading_"+strconv.Itoa(i))), strings.TrimSpace(c.r.PostForm.Get("section_body_"+strconv.Itoa(i)))
		if h == "" && b == "" {
			continue
		}
		out = append(out, Section{Heading: h, Body: b})
	}
	return out
}

func formCountries(s string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		out = append(out, strings.ToUpper(f))
	}
	return out
}

func (u *UI) jobCreate(c *uiCtx) error {
	v := c.r.PostForm
	sal, err := formSalary(c)
	if err != nil {
		return err
	}
	hc, err := strconv.Atoi(strings.TrimSpace(v.Get("headcount")))
	if err != nil {
		return invalid("headcount", "must be a whole number")
	}
	in := NewJob{Title: v.Get("title"), Department: v.Get("department"), Location: Location{City: v.Get("city"), Region: v.Get("region"), Country: strings.ToUpper(v.Get("country")), PostalCode: v.Get("postal_code")},
		Remote: v.Get("remote"), ApplicantCountries: formCountries(v.Get("applicant_countries")), EmploymentType: v.Get("employment_type"), Salary: sal,
		PublishSalary: v.Get("publish_salary") == "on", Sections: formSections(c), HiringManager: strings.TrimSpace(v.Get("hiring_manager")), Headcount: hc}
	if d := strings.TrimSpace(v.Get("valid_through")); d != "" {
		in.ValidThrough = &d
	}
	j, err := u.Store.CreateJob(c.ctx(), c.p, in)
	if err != nil {
		return err
	}
	return u.redirect(c, "/jobs/"+j.ID, "created")
}

func (u *UI) jobSave(c *uiCtx) error {
	v := c.r.PostForm
	ver, err := strconv.ParseInt(v.Get("version"), 10, 64)
	if err != nil {
		return invalid("version", "the form is out of date; reload it")
	}
	sal, err := formSalary(c)
	if err != nil {
		return err
	}
	hc, err := strconv.Atoi(strings.TrimSpace(v.Get("headcount")))
	if err != nil {
		return invalid("headcount", "must be a whole number")
	}
	title, dept, remote, emp, hm := v.Get("title"), v.Get("department"), v.Get("remote"), v.Get("employment_type"), strings.TrimSpace(v.Get("hiring_manager"))
	loc := Location{City: v.Get("city"), Region: v.Get("region"), Country: strings.ToUpper(v.Get("country")), PostalCode: v.Get("postal_code")}
	countries, secs, pub := formCountries(v.Get("applicant_countries")), formSections(c), v.Get("publish_salary") == "on"
	in := JobPatch{Title: &title, Department: &dept, Location: &loc, Remote: &remote, ApplicantCountries: &countries, EmploymentType: &emp,
		Salary: sal, ClearSalary: sal == nil, PublishSalary: &pub, Sections: &secs, HiringManager: &hm, Headcount: &hc}
	if d := strings.TrimSpace(v.Get("valid_through")); d != "" {
		in.ValidThrough = &d
	}
	id := c.r.PathValue("id")
	if _, err := u.Store.PatchJob(c.ctx(), c.p, id, in, &ver); err != nil {
		return err
	}
	return u.redirect(c, "/jobs/"+id, "saved")
}

func (u *UI) jobTransition(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Store.TransitionJob(c.ctx(), c.p, id, c.r.PostForm.Get("to"), c.r.PostForm.Get("reason")); err != nil {
		return err
	}
	return u.redirect(c, "/jobs/"+id, "saved")
}

func (u *UI) jobApproval(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Store.RequestApproval(c.ctx(), c.p, id); err != nil {
		return err
	}
	return u.redirect(c, "/jobs/"+id, "approval_requested")
}

// ---- job page and pipeline board -----------------------------------------------------------

type card struct {
	ID, Name   string
	Prev, Next *stageView
}

type column struct {
	Stage stageView
	Count int
	Cards []card
	More  int
}

type jobPageData struct {
	Job         *Job
	Columns     []column
	Rejected    int
	Withdrawn   int
	Hired       int
	CareersURL  string
	Live        bool
	CanApproval bool
	Moves       []string
	CloseReason []string
	Cap         Caps
}

func (u *UI) jobPage(c *uiCtx) error {
	id := c.r.PathValue("id")
	j, err := u.Store.GetJob(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	ss, err := u.stageViews(c)
	if err != nil {
		return err
	}
	f, err := u.Store.FunnelCounts(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	d := jobPageData{Job: j, Rejected: f.Rejected, Withdrawn: f.Withdrawn, Hired: f.Hired, Cap: c.p.Caps(), CloseReason: CloseReasons}
	d.CareersURL = u.Site.JobURL(j)
	d.Live = j.Status == StatusOpen
	d.CanApproval = (j.Status == StatusDraft || j.Status == StatusPaused) && (j.Approval == ApprovalNone || j.Approval == ApprovalRejected)
	switch j.Status {
	case StatusDraft, StatusPaused:
		d.Moves = []string{StatusOpen}
	case StatusOpen:
		d.Moves = []string{StatusPaused}
	}
	for i := range ss {
		st := ss[i]
		if st.Kind == KindHired {
			continue // hired applications are counted below, not drawn
		}
		col := column{Stage: st, Count: f.Active[st.Key]}
		vs, _, err := u.Store.ListApplications(c.ctx(), c.p, AppFilter{JobID: id, Stage: st.Key, Status: AppActive, Limit: boardLimit})
		if err != nil {
			return err
		}
		for _, v := range vs {
			cd := card{ID: v.ID, Name: v.CandidateName}
			if i > 0 {
				p := ss[i-1]
				cd.Prev = &p
			}
			if i+1 < len(ss) && ss[i+1].Kind != KindHired {
				n := ss[i+1]
				cd.Next = &n
			}
			col.Cards = append(col.Cards, cd)
		}
		col.More = max(0, col.Count-len(col.Cards))
		d.Columns = append(d.Columns, col)
	}
	u.render(c.w, c.r, c, http.StatusOK, "job.html", "job.title", d)
	return nil
}

// ---- application page ----------------------------------------------------------------------

type histRow struct {
	From, To, Status, Actor string
	At                      string
	Note                    string
	HasNote                 bool
}

type appPageData struct {
	A             *ApplicationView
	StageName     string
	Stages        []stageView
	History       []histRow
	Prev, Next    *stageView
	Offer         bool // the application sits in an offer stage and is active
	Active        bool
	ResumeLink    bool
	ResumeScan    string
	Interviewers  []string
	Mine          *Scorecard
	Others        []Scorecard
	Assigned      bool
	Blind         bool
	RejectReasons []string
	Cap           Caps
	Subject       string
	Ratings       []int
	Recs          []string
	CareersHint   bool
}

func (u *UI) appPage(c *uiCtx) error {
	id := c.r.PathValue("id")
	a, err := u.Store.GetApplication(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	ss, err := u.stageViews(c)
	if err != nil {
		return err
	}
	d := appPageData{A: a, Stages: ss, StageName: stageName(ss, a.Stage), Cap: c.p.Caps(), Subject: c.p.Subject, RejectReasons: RejectReasons, Ratings: []int{1, 2, 3, 4}, Recs: Recommendations}
	d.Active = a.Status == AppActive
	for i, s := range ss {
		if s.Key != a.Stage {
			continue
		}
		d.Offer = d.Active && s.Kind == KindOffer
		if d.Active {
			if i > 0 {
				p := ss[i-1]
				d.Prev = &p
			}
			if i+1 < len(ss) && ss[i+1].Kind != KindHired {
				n := ss[i+1]
				d.Next = &n
			}
		}
	}
	for _, h := range a.History {
		r := histRow{From: stageName(ss, h.From), To: stageName(ss, h.To), Status: h.Status, Actor: h.Actor, At: fmtWhen(h.At)}
		if h.Note != nil {
			r.Note, r.HasNote = *h.Note, true
		}
		d.History = append(d.History, r)
	}
	if a.Resume != nil {
		d.ResumeScan = a.Resume.Scan
		d.ResumeLink = a.Resume.Scan != string(ScanInfected)
	}
	d.Interviewers = a.Interviewers
	if d.Cap.Score {
		assigned, err := u.Store.IsAssigned(c.ctx(), c.p, id)
		if err != nil {
			return err
		}
		d.Assigned = assigned
		l, err := u.Store.ListScorecards(c.ctx(), c.p, id)
		if err != nil {
			return err
		}
		mySubmitted := false
		for i := range l.Items {
			sc := l.Items[i]
			if sc.Author == c.p.Subject {
				d.Mine = &sc
				mySubmitted = sc.Status == "submitted"
			} else {
				d.Others = append(d.Others, sc)
			}
		}
		// Nothing about colleagues' feedback is drawn until the caller has submitted their own:
		// no count, no names, no status. The notice is a constant for everyone on the panel.
		d.Blind = (d.Assigned || l.OthersHidden) && !mySubmitted
	}
	u.render(c.w, c.r, c, http.StatusOK, "application.html", "app.title", d)
	return nil
}

func (u *UI) appMove(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Store.MoveApplicationNote(c.ctx(), c.p, id, c.r.PostForm.Get("to"), strings.TrimSpace(c.r.PostForm.Get("note"))); err != nil {
		return err
	}
	return u.redirect(c, back(c, "/applications/"+id), "moved")
}

var returnRE = regexp.MustCompile(`^/jobs/job_[0-9a-f]{16}$`)

// back returns where a board or page form wants to go afterwards: only a local path of this UI.
func back(c *uiCtx, def string) string {
	if r := c.r.PostForm.Get("return"); returnRE.MatchString(r) {
		return r
	}
	return def
}

func (u *UI) appReject(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Store.RejectApplication(c.ctx(), c.p, id, c.r.PostForm.Get("reason"), strings.TrimSpace(c.r.PostForm.Get("note"))); err != nil {
		return err
	}
	return u.redirect(c, "/applications/"+id, "rejected")
}

func (u *UI) appHire(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Store.HireApplication(c.ctx(), c.p, id); err != nil {
		return err
	}
	return u.redirect(c, "/applications/"+id, "hired")
}

func (u *UI) appAssign(c *uiCtx) error {
	id := c.r.PathValue("id")
	if err := u.Store.AssignInterviewer(c.ctx(), c.p, id, strings.TrimSpace(c.r.PostForm.Get("subject"))); err != nil {
		return err
	}
	return u.redirect(c, "/applications/"+id, "assigned")
}

func (u *UI) appUnassign(c *uiCtx) error {
	id := c.r.PathValue("id")
	if err := u.Store.RemoveInterviewer(c.ctx(), c.p, id, strings.TrimSpace(c.r.PostForm.Get("subject"))); err != nil {
		return err
	}
	return u.redirect(c, "/applications/"+id, "unassigned")
}

// appResume streams the stored file as an attachment under a fixed content type, never inline.
func (u *UI) appResume(c *uiCtx) error {
	m, rc, err := u.Store.OpenResume(c.ctx(), c.p, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	h := c.w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+safeFilename(m.Name)+`"`)
	h.Set("Content-Length", strconv.FormatInt(m.Size, 10))
	h.Set("X-Resume-Scan", m.Scan)
	c.w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(c.w, rc)
	return nil
}

// safeFilename keeps a download name to plain characters: whatever the applicant called the file,
// it cannot add header lines or path parts.
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "resume"
	}
	return b.String()
}

// ---- reveal of candidate data --------------------------------------------------------------

type revealData struct {
	AppID       string
	Name        string
	Candidate   *Candidate
	CoverLetter *string
	RejectNote  *string
	Restricted  []string
	CanEdit     bool
}

// appReveal shows the sealed and contact fields of the candidate behind an application. It is a
// POST so a link or a prefetch cannot trigger it; the Store audits the read before returning.
func (u *UI) appReveal(c *uiCtx) error {
	if ok, _ := u.reveals.allow(c.ss.subject); !ok {
		c.w.Header().Set("Retry-After", "30")
		u.renderError(c.w, c.r, c.ss, c.p, http.StatusTooManyRequests, "err.busy_title", "err.busy")
		return nil
	}
	id := c.r.PathValue("id")
	a, err := u.Store.GetApplication(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	cand, err := u.Store.GetCandidate(c.ctx(), c.p, a.CandidateID)
	if err != nil {
		return err
	}
	d := revealData{AppID: id, Name: cand.Name, Candidate: cand, CoverLetter: a.CoverLetter, RejectNote: a.RejectNote, CanEdit: cand.Notes != nil && c.p.Caps().EditCandidate}
	d.Restricted = cand.RestrictedFields
	u.render(c.w, c.r, c, http.StatusOK, "reveal.html", "reveal.title", d)
	return nil
}

func (u *UI) appNotes(c *uiCtx) error {
	id := c.r.PathValue("id")
	a, err := u.Store.GetApplication(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	notes := strings.TrimSpace(c.r.PostForm.Get("notes"))
	if _, err := u.Store.PatchCandidate(c.ctx(), c.p, a.CandidateID, CandidatePatch{Notes: &notes}); err != nil {
		return err
	}
	return u.redirect(c, "/applications/"+id, "saved")
}

func (u *UI) appScorecard(c *uiCtx) error {
	id := c.r.PathValue("id")
	rating, _ := strconv.Atoi(c.r.PostForm.Get("rating"))
	in := ScorecardInput{Rating: rating, Recommendation: c.r.PostForm.Get("recommendation"), Comments: strings.TrimSpace(c.r.PostForm.Get("comments"))}
	if _, err := u.Store.SaveScorecard(c.ctx(), c.p, id, in); err != nil {
		return err
	}
	if c.r.PostForm.Get("action") == "submit" {
		if _, err := u.Store.SubmitScorecard(c.ctx(), c.p, id); err != nil {
			return err
		}
		return u.redirect(c, "/applications/"+id, "submitted")
	}
	return u.redirect(c, "/applications/"+id, "saved")
}

// ---- candidates and talent pool ------------------------------------------------------------

type candidateApp struct {
	ID, Job, Stage, Status string
}

type candidatePageData struct {
	C       *CandidateSummary
	Apps    []candidateApp
	Tags    string
	CanEdit bool
	CanPool bool
	Stages  []stageView
}

func (u *UI) candidatePage(c *uiCtx) error {
	id := c.r.PathValue("id")
	cs, err := u.Store.GetCandidateSummary(c.ctx(), c.p, id)
	if err != nil {
		return err
	}
	ss, err := u.stageViews(c)
	if err != nil {
		return err
	}
	vs, _, err := u.Store.ListApplications(c.ctx(), c.p, AppFilter{CandidateID: id, Limit: listLimit})
	if err != nil {
		return err
	}
	d := candidatePageData{C: cs, Tags: strings.Join(cs.Tags, ", "), CanEdit: c.p.Caps().EditCandidate, CanPool: c.p.Caps().TalentPool, Stages: ss}
	for _, v := range vs {
		d.Apps = append(d.Apps, candidateApp{ID: v.ID, Job: v.JobTitle, Stage: stageName(ss, v.Stage), Status: v.Status})
	}
	u.render(c.w, c.r, c, http.StatusOK, "candidate.html", "candidate.title", d)
	return nil
}

func (u *UI) candidateTags(c *uiCtx) error {
	id := c.r.PathValue("id")
	tags := []string{}
	for _, t := range strings.FieldsFunc(c.r.PostForm.Get("tags"), func(r rune) bool { return r == ',' || r == ' ' }) {
		tags = append(tags, strings.ToLower(t))
	}
	if _, err := u.Store.PatchCandidate(c.ctx(), c.p, id, CandidatePatch{Tags: &tags}); err != nil {
		return err
	}
	return u.redirect(c, "/candidates/"+id, "saved")
}

func (u *UI) candidatePool(c *uiCtx) error {
	id := c.r.PathValue("id")
	if _, err := u.Store.SetTalentPool(c.ctx(), c.p, id, c.r.PostForm.Get("member") == "on"); err != nil {
		return err
	}
	return u.redirect(c, "/candidates/"+id, "saved")
}

type talentRow struct {
	ID, Name, Source string
	Tags             []string
}

type talentData struct {
	Q, Tag string
	Rows   []talentRow
	Next   int64
}

func (u *UI) talent(c *uiCtx) error {
	q := strings.TrimSpace(c.r.URL.Query().Get("q"))
	tag := strings.TrimSpace(c.r.URL.Query().Get("tag"))
	cs, next, err := u.Store.SearchTalentPool(c.ctx(), c.p, q, tag, cursorParam(c), listLimit)
	if err != nil {
		return err
	}
	d := talentData{Q: q, Tag: tag, Next: next}
	for _, cd := range cs {
		d.Rows = append(d.Rows, talentRow{ID: cd.ID, Name: cd.Name, Source: cd.Source, Tags: cd.Tags})
	}
	u.render(c.w, c.r, c, http.StatusOK, "talent.html", "talent.title", d)
	return nil
}

// ---- reports and settings ------------------------------------------------------------------

type eeoField struct {
	Name       string
	Cells      []eeoCell
	Suppressed bool
}

type eeoCell struct {
	Value string
	Count int
}

type eeoData struct {
	Responses int
	HasTotal  bool
	MinCell   int
	Fields    []eeoField
}

func (u *UI) eeoReport(c *uiCtx) error {
	rep, err := u.Store.EEOAggregate(c.ctx(), c.p)
	if err != nil {
		return err
	}
	d := eeoData{MinCell: rep.MinCell}
	if rep.Responses != nil {
		d.Responses, d.HasTotal = *rep.Responses, true
	}
	for _, f := range EEOFields {
		ef := eeoField{Name: f}
		for _, cell := range rep.Fields[f] {
			if cell.Count == nil {
				// The Store hides a whole question when any answer is rare; the UI then draws no
				// answer at all, because the name of a rare answer is itself a disclosure.
				ef.Suppressed = true
				break
			}
			ef.Cells = append(ef.Cells, eeoCell{Value: cell.Value, Count: *cell.Count})
		}
		if ef.Suppressed {
			ef.Cells = nil
		}
		d.Fields = append(d.Fields, ef)
	}
	u.render(c.w, c.r, c, http.StatusOK, "eeo.html", "eeo.title", d)
	return nil
}

type settingsData struct {
	Stages []Stage
	Boards []BoardCaps
}

func (u *UI) settings(c *uiCtx) error {
	if err := u.Store.Require(c.ctx(), c.p, ActJobRead); err != nil {
		return err
	}
	ss, err := u.Store.ListStages(c.ctx())
	if err != nil {
		return err
	}
	u.render(c.w, c.r, c, http.StatusOK, "settings.html", "settings.title", settingsData{Stages: ss, Boards: u.Store.Boards()})
	return nil
}

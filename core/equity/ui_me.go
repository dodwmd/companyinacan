package equity

// The home page and the holder's own pages. Everything under /me goes through the Service's
// self-service methods, which answer for the caller's own confirmed records only, whatever other
// roles the caller has.

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

type homeData struct {
	Subject string
	// Notice explains why self-service or administration is not offered.
	DirectoryDown bool
	Mine          *MyPositionsPage
	Counts        bool
	Pending       int64
	VestingSoon   int64
}

func (u *UI) home(c *uiCtx) error {
	d := homeData{Subject: c.a.Subject, DirectoryDown: c.a.DirectoryDown}
	if c.caps.Self {
		p, err := u.Svc.MyPositions(c.ctx(), c.a, 0)
		if err != nil {
			return err
		}
		d.Mine = p
	}
	if c.caps.Grants {
		pending, vesting, err := u.Svc.SummaryCounts(c.ctx(), c.a)
		if err != nil {
			return err
		}
		d.Counts, d.Pending, d.VestingSoon = true, pending, vesting
	}
	return u.view(c, "home.html", "home.title", d)
}

type myData struct {
	Page      *MyPositionsPage
	Exercises []Exercise
	Notice    string
}

func (u *UI) myEquity(c *uiCtx) error {
	p, err := u.Svc.MyPositions(c.ctx(), c.a, c.after())
	if err != nil {
		return err
	}
	ex, err := u.Svc.MyConfirmedExercises(c.ctx(), c.a)
	if err != nil {
		return err
	}
	return u.view(c, "me.html", "mine.title", myData{Page: p, Exercises: ex, Notice: Notice})
}

func (u *UI) myDocuments(c *uiCtx) error {
	p, err := u.Svc.MyPositions(c.ctx(), c.a, c.after())
	if err != nil {
		return err
	}
	return u.view(c, "documents.html", "docs.title", myData{Page: p, Notice: Notice})
}

type grantData struct {
	Position  Position
	Exercises []Exercise
	Schedule  []string
	Idem      string
	Notice    string
}

// scheduleText describes a vesting schedule in words, one line per part.
func (u *UI) scheduleText(s Schedule) []string {
	var out []string
	if s.Kind == "periodic" {
		out = append(out, u.msg("sched.periodic", s.Months, s.CliffMonths, u.label("interval", s.Interval)))
	}
	for _, t := range s.Tranches {
		when := fmtDate(t.Date)
		if t.Milestone {
			when = u.msg("sched.milestone")
		}
		out = append(out, u.msg("sched.tranche", t.Label, fmt.Sprintf("%d.%02d%%", t.BasisPoints/100, t.BasisPoints%100), when))
	}
	rounding := s.Rounding
	if rounding == "" {
		rounding = "floor"
	}
	out = append(out, u.label("rounding", rounding))
	for _, a := range s.Acceleration {
		out = append(out, u.msg("sched.acceleration", a.Name, fmt.Sprintf("%d.%02d%%", a.PercentBP/100, a.PercentBP%100)))
	}
	return out
}

func (u *UI) myGrantData(c *uiCtx) (*grantData, error) {
	p, ex, err := u.Svc.MyPosition(c.ctx(), c.a, c.id())
	if err != nil {
		return nil, err
	}
	return &grantData{Position: *p, Exercises: ex, Schedule: u.scheduleText(p.Grant.Schedule), Idem: session.NewToken(), Notice: Notice}, nil
}

func (u *UI) myGrantPage(c *uiCtx) error {
	d, err := u.myGrantData(c)
	if err != nil {
		return err
	}
	return u.view(c, "mygrant.html", "mine.grant_title", d)
}

// myStatement is the printable summary of one grant.
func (u *UI) myStatement(c *uiCtx) error {
	d, err := u.myGrantData(c)
	if err != nil {
		return err
	}
	return u.view(c, "statement.html", "docs.statement_title", d)
}

type reviewData struct {
	Preview ExercisePreview
	Idem    string
	Error   string
	Notice  string
}

func (u *UI) myPreview(c *uiCtx) (*ExercisePreview, error) {
	q, err := ParseQty(c.form("quantity"))
	if err != nil {
		return nil, err
	}
	return u.Svc.PreviewMyExercise(c.ctx(), c.a, c.id(), q)
}

// myExerciseReview shows what a request would be before it is sent.
func (u *UI) myExerciseReview(c *uiCtx) error {
	p, err := u.myPreview(c)
	if err != nil {
		return err
	}
	return u.view(c, "review.html", "ex.review_title", reviewData{Preview: *p, Idem: c.form("idem"), Notice: Notice})
}

func (u *UI) myExerciseRequest(c *uiCtx) error {
	q, err := ParseQty(c.form("quantity"))
	if err != nil {
		return err
	}
	if c.form("accept") != "yes" {
		p, err := u.myPreview(c)
		if err != nil {
			return err
		}
		u.render(c.w, c.r, c, http.StatusBadRequest, "review.html", "ex.review_title",
			reviewData{Preview: *p, Idem: c.form("idem"), Notice: Notice, Error: u.msg("ex.accept_required")}, "")
		return nil
	}
	idem := c.form("idem")
	if idem == "" {
		return &InvalidError{Msg: u.msg("err.bad_form")}
	}
	_, err = u.Svc.RequestMyExercise(c.ctx(), c.a, c.id(), q, "ui:"+idem, []byte(c.id()+" "+q.String()))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	c.done("/me/grants/"+c.id(), "flash.exercise_requested")
	return nil
}

func (u *UI) myExerciseCancel(c *uiCtx) error {
	grant, err := u.Svc.CancelMyExercise(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	c.done("/me/grants/"+grant, "flash.exercise_cancelled")
	return nil
}

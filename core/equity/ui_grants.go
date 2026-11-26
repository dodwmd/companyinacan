package equity

// Grants and exercises across holders: drafting and submitting, the approval status, vesting, and
// the exercise chain. Every refusal (role, separation of duties, state) is the Service's.

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

var grantStatuses = []string{GrantDraft, GrantPending, GrantApproved, GrantActive, GrantCancelled, GrantForfeited, GrantExpired}

var exerciseStatuses = []string{ExRequested, ExApproved, ExPaid, ExIssued, ExRejected, ExCancelled}

type grantLine struct {
	Grant
	Holder string
}

type grantsData struct {
	Entity       Entity
	Items        []grantLine
	Status, Next string
	Statuses     []string
}

func (u *UI) grants(c *uiCtx) error {
	status := c.query("status")
	page, err := u.Svc.ListGrants(c.ctx(), c.a, c.id(), GrantFilter{Status: status, After: c.after(), Limit: 50})
	if err != nil {
		return err
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	var ids []string
	for _, g := range page.Items {
		ids = append(ids, g.StakeholderID)
	}
	names, err := u.holderNames(c, ids)
	if err != nil {
		return err
	}
	d := grantsData{Entity: *e, Status: status, Next: page.Next, Statuses: grantStatuses}
	for _, g := range page.Items {
		d.Items = append(d.Items, grantLine{g, names[g.StakeholderID]})
	}
	return u.view(c, "grants.html", "grant.list_title", d)
}

// ---- drafting --------------------------------------------------------------------------------

type option struct{ Value, Label string }

type grantForm struct {
	Plan, Stakeholder, Type, Quantity, Strike, GrantDate, VestStart, Months, Cliff, Interval, Rounding, Expiry, PTEP, Notes string
	Idem, Error                                                                                                             string
}

type grantFormData struct {
	Entity  Entity
	Plans   []option
	Holders []option
	Form    grantForm
}

func (u *UI) grantFormPage(c *uiCtx, form grantForm, status int) error {
	if !c.caps.GrantDraft {
		return u.denyDraft(c)
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	plans, err := u.Svc.ListPlans(c.ctx(), c.a, e.ID)
	if err != nil {
		return err
	}
	holders, err := u.Svc.ListStakeholders(c.ctx(), c.a, e.ID)
	if err != nil {
		return err
	}
	var ids []string
	for _, h := range holders {
		ids = append(ids, h.ID)
	}
	names, err := u.holderNames(c, ids)
	if err != nil {
		return err
	}
	d := grantFormData{Entity: *e, Form: form}
	for _, p := range plans {
		if p.Status == "active" {
			d.Plans = append(d.Plans, option{p.ID, p.Name})
		}
	}
	for _, h := range holders {
		if !h.Erased {
			d.Holders = append(d.Holders, option{h.ID, names[h.ID] + " (" + u.label("kind", h.Kind) + ")"})
		}
	}
	u.render(c.w, c.r, c, status, "grantnew.html", "grant.new_title", d, "")
	return nil
}

// denyDraft says why the draft form is not offered; the Service refuses the verbs themselves.
func (u *UI) denyDraft(c *uiCtx) error {
	if c.caps.NeedsPerson {
		return ErrIdentityRequired
	}
	return ErrForbidden
}

func (u *UI) grantNew(c *uiCtx) error {
	return u.grantFormPage(c, grantForm{Type: "NSO", Months: "48", Cliff: "12", Interval: "monthly", Rounding: "floor", PTEP: "90", Idem: session.NewToken()}, http.StatusOK)
}

// grantInput reads the form. Quantities and the strike are parsed textually into exact units.
func grantInput(f grantForm, currency string) (GrantInput, error) {
	in := GrantInput{StakeholderID: f.Stakeholder, Type: f.Type, Currency: currency, GrantDate: f.GrantDate, VestStart: f.VestStart,
		ExpiryDate: f.Expiry, Notes: f.Notes, Schedule: Schedule{Kind: "periodic", Interval: f.Interval, Rounding: f.Rounding}}
	var err error
	if in.Quantity, err = ParseQty(f.Quantity); err != nil {
		return in, err
	}
	strike := f.Strike
	if strike == "" {
		strike = "0"
	}
	if in.StrikeMinor, err = ParseMinor(strike, currency); err != nil {
		return in, err
	}
	for _, n := range []struct {
		label, v string
		to       *int
	}{{"months", f.Months, &in.Schedule.Months}, {"cliff_months", f.Cliff, &in.Schedule.CliffMonths}, {"post_termination_exercise_days", f.PTEP, &in.PTEPDays}} {
		if *n.to, err = strconv.Atoi(n.v); err != nil {
			return in, invalid("%s must be a whole number", n.label)
		}
	}
	return in, nil
}

func (u *UI) grantCreate(c *uiCtx) error {
	f := grantForm{Plan: c.form("plan_id"), Stakeholder: c.form("stakeholder_id"), Type: c.form("type"), Quantity: c.form("quantity"), Strike: c.form("strike"),
		GrantDate: c.form("grant_date"), VestStart: c.form("vesting_start"), Months: c.form("months"), Cliff: c.form("cliff_months"), Interval: c.form("interval"),
		Rounding: c.form("rounding"), Expiry: c.form("expiry_date"), PTEP: c.form("ptep_days"), Notes: c.form("notes"), Idem: c.form("idem")}
	id, err := u.createGrant(c, f)
	if err != nil {
		if status, msg, ok := u.refused(c, err); ok {
			f.Error = msg
			return u.grantFormPage(c, f, status)
		}
		return err
	}
	c.done("/company/grants/"+id, "flash.grant_created")
	return nil
}

func (u *UI) createGrant(c *uiCtx, f grantForm) (string, error) {
	e, err := u.Svc.GetEntity(c.ctx(), c.a, c.id())
	if err != nil {
		return "", err
	}
	plans, err := u.Svc.ListPlans(c.ctx(), c.a, e.ID)
	if err != nil {
		return "", err
	}
	known := false
	for _, p := range plans {
		known = known || p.ID == f.Plan
	}
	if !known {
		return "", invalid("choose a plan of this entity")
	}
	in, err := grantInput(f, e.Currency)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	key := ""
	if f.Idem != "" {
		key = "ui:" + f.Idem
	}
	id, _, err := u.Svc.CreateGrant(c.ctx(), c.a, f.Plan, in, key, body)
	return id, err
}

// ---- one grant -------------------------------------------------------------------------------

type grantViewData struct {
	Entity    Entity
	Grant     Grant
	Holder    string
	Pending   string // how the holder record still has to be confirmed
	Plan      string
	Vesting   VestingView
	Schedule  []string
	Exercises []Exercise
}

func (u *UI) grantRender(c *uiCtx, id, flash string) error {
	g, err := u.Svc.GetGrant(c.ctx(), c.a, id)
	if err != nil {
		return err
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, g.EntityID)
	if err != nil {
		return err
	}
	v, err := u.Svc.Vesting(c.ctx(), c.a, id, "")
	if err != nil {
		return err
	}
	st, err := u.Svc.GetStakeholder(c.ctx(), c.a, g.StakeholderID)
	if err != nil {
		return err
	}
	names, err := u.holderNames(c, []string{g.StakeholderID})
	if err != nil {
		return err
	}
	plans, err := u.Svc.ListPlans(c.ctx(), c.a, g.EntityID)
	if err != nil {
		return err
	}
	ex, err := u.Svc.ListExercises(c.ctx(), c.a, g.EntityID, "")
	if err != nil {
		return err
	}
	d := grantViewData{Entity: *e, Grant: *g, Holder: names[g.StakeholderID], Pending: u.Svc.ConfirmationNeeded(*st), Vesting: *v, Schedule: u.scheduleText(g.Schedule)}
	for _, p := range plans {
		if p.ID == g.PlanID {
			d.Plan = p.Name
		}
	}
	for _, x := range ex {
		if x.GrantID == id {
			d.Exercises = append(d.Exercises, x)
		}
	}
	u.render(c.w, c.r, c, http.StatusOK, "grant.html", "grant.title", d, flash)
	return nil
}

func (u *UI) grantPage(c *uiCtx) error {
	if !c.caps.Grants {
		return ErrForbidden // a holder's own grant is under /me
	}
	return u.grantRender(c, c.id(), "")
}

func (u *UI) grantSubmit(c *uiCtx) error {
	if _, err := u.Svc.SubmitGrant(c.ctx(), c.a, c.id()); err != nil {
		return err
	}
	c.done("/company/grants/"+c.id(), "flash.grant_submitted")
	return nil
}

func (u *UI) grantReject(c *uiCtx) error {
	if _, err := u.Svc.RejectGrant(c.ctx(), c.a, c.id(), c.form("reason")); err != nil {
		return err
	}
	c.done("/company/grants/"+c.id(), "flash.grant_rejected")
	return nil
}

func (u *UI) grantDelete(c *uiCtx) error {
	a, err := u.stepUp(c)
	if err != nil {
		return err
	}
	g, err := u.Svc.GetGrant(c.ctx(), a, c.id())
	if err != nil {
		return err
	}
	if err := u.Svc.DeleteGrant(c.ctx(), a, c.id()); err != nil {
		return err
	}
	c.a, c.caps = a, u.Svc.Capabilities(a)
	u.render(c.w, c.r, c, http.StatusOK, "deleted.html", "grant.deleted_title", g.EntityID, "")
	return nil
}

// grantStep is an irreversible step of a grant: it is confirmed by a step-up and made as the
// caller the step-up found.
func (u *UI) grantStep(c *uiCtx, flash string, fn func(a Actor, id string) (*Grant, error)) error {
	a, err := u.stepUp(c)
	if err != nil {
		return err
	}
	if _, err := fn(a, c.id()); err != nil {
		return err
	}
	c.a, c.caps = a, u.Svc.Capabilities(a)
	return u.grantRender(c, c.id(), flash)
}

func (u *UI) grantApprove(c *uiCtx) error {
	return u.grantStep(c, "flash.grant_approved", func(a Actor, id string) (*Grant, error) { return u.Svc.ApproveGrant(c.ctx(), a, id) })
}

func (u *UI) grantActivate(c *uiCtx) error {
	on := c.form("activated_on")
	return u.grantStep(c, "flash.grant_activated", func(a Actor, id string) (*Grant, error) { return u.Svc.ActivateGrant(c.ctx(), a, id, on) })
}

// ---- exercises -------------------------------------------------------------------------------

type exerciseLine struct {
	Exercise
	Holder string
}

type exercisesData struct {
	Entity   Entity
	Items    []exerciseLine
	Status   string
	Statuses []string
}

func (u *UI) exercises(c *uiCtx) error {
	status := c.query("status")
	list, err := u.Svc.ListExercises(c.ctx(), c.a, c.id(), status)
	if err != nil {
		return err
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	var ids []string
	for _, x := range list {
		ids = append(ids, x.StakeholderID)
	}
	names, err := u.holderNames(c, ids)
	if err != nil {
		return err
	}
	d := exercisesData{Entity: *e, Status: status, Statuses: exerciseStatuses}
	for _, x := range list {
		d.Items = append(d.Items, exerciseLine{x, names[x.StakeholderID]})
	}
	return u.view(c, "exercises.html", "ex.list_title", d)
}

type exerciseData struct {
	Entity   Entity
	Exercise Exercise
	Holder   string
	Due      string // the amount due as a plain decimal, as the payment form takes it
	Today    string
}

func (u *UI) exerciseRender(c *uiCtx, x *Exercise, flash string) error {
	e, err := u.Svc.GetEntity(c.ctx(), c.a, x.EntityID)
	if err != nil {
		return err
	}
	names, err := u.holderNames(c, []string{x.StakeholderID})
	if err != nil {
		return err
	}
	d := exerciseData{Entity: *e, Exercise: *x, Holder: names[x.StakeholderID], Due: FormatMinor(x.DueMinor, x.Currency), Today: u.Svc.today()}
	u.render(c.w, c.r, c, http.StatusOK, "exercise.html", "ex.title", d, flash)
	return nil
}

func (u *UI) exerciseByID(c *uiCtx, flash string) error {
	x, err := u.Svc.GetExercise(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	return u.exerciseRender(c, x, flash)
}

func (u *UI) exercisePage(c *uiCtx) error {
	if !c.caps.Grants {
		return ErrForbidden // a holder's own requests are under /me
	}
	return u.exerciseByID(c, "")
}

// exerciseDone redirects back to the exercise after a step that can still be withdrawn.
func (u *UI) exerciseDone(c *uiCtx, flash string, err error) error {
	if err != nil {
		return err
	}
	c.done("/company/exercises/"+c.id(), flash)
	return nil
}

func (u *UI) exerciseApprove(c *uiCtx) error {
	_, err := u.Svc.ApproveExercise(c.ctx(), c.a, c.id())
	return u.exerciseDone(c, "flash.exercise_approved", err)
}

func (u *UI) exerciseReject(c *uiCtx) error {
	_, err := u.Svc.RejectExercise(c.ctx(), c.a, c.id())
	return u.exerciseDone(c, "flash.exercise_rejected", err)
}

func (u *UI) exerciseCancel(c *uiCtx) error {
	_, err := u.Svc.CancelExercise(c.ctx(), c.a, c.id())
	return u.exerciseDone(c, "flash.exercise_cancelled", err)
}

// exercisePayment records money received: it cannot be undone (a paid exercise cannot be
// cancelled), so it is confirmed by a step-up.
func (u *UI) exercisePayment(c *uiCtx) error {
	// the currency of the amount is the exercise's, never the form's
	x, err := u.Svc.GetExercise(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	amount, err := ParseMinor(c.form("amount"), x.Currency)
	if err != nil {
		return err
	}
	a, err := u.stepUp(c)
	if err != nil {
		return err
	}
	x, err = u.Svc.RecordPayment(c.ctx(), a, c.id(), PaymentInput{AmountMinor: amount, PaidOn: c.form("paid_on"), Reference: c.form("reference")})
	if err != nil {
		return err
	}
	c.a, c.caps = a, u.Svc.Capabilities(a)
	return u.exerciseRender(c, x, "flash.exercise_paid")
}

// exerciseIssue appends the shares to the ledger, which nothing can undo.
func (u *UI) exerciseIssue(c *uiCtx) error {
	a, err := u.stepUp(c)
	if err != nil {
		return err
	}
	x, err := u.Svc.IssueShares(c.ctx(), a, c.id())
	if err != nil {
		return err
	}
	c.a, c.caps = a, u.Svc.Capabilities(a)
	return u.exerciseRender(c, x, "flash.exercise_issued")
}

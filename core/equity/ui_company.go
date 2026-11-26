package equity

// The administration and finance pages: entities, pools, the cap table, exports, stakeholders and
// the holder verification queue.

import (
	"bytes"
	"errors"
	"mime"
	"net/http"
)

type companiesData struct{ Entities []Entity }

func (u *UI) companies(c *uiCtx) error {
	es, err := u.Svc.ListEntities(c.ctx(), c.a)
	if err != nil {
		return err
	}
	return u.view(c, "companies.html", "company.title", companiesData{es})
}

type planLine struct {
	Plan      Plan
	ClassName string
	Pool      PoolStatus
}

type companyData struct {
	Entity  Entity
	Classes []ShareClass
	Plans   []planLine
}

func classNames(cs []ShareClass) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		m[c.ID] = c.Name
	}
	return m
}

func (u *UI) company(c *uiCtx) error {
	e, err := u.Svc.GetEntity(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	d := companyData{Entity: *e}
	if d.Classes, err = u.Svc.ListShareClasses(c.ctx(), c.a, e.ID); err != nil {
		return err
	}
	plans, err := u.Svc.ListPlans(c.ctx(), c.a, e.ID)
	if err != nil {
		return err
	}
	names := classNames(d.Classes)
	for _, p := range plans {
		ps, err := u.Svc.Pool(c.ctx(), c.a, p.ID, "")
		if err != nil {
			return err
		}
		d.Plans = append(d.Plans, planLine{Plan: p, ClassName: names[p.ClassID], Pool: *ps})
	}
	return u.view(c, "company.html", "company.title", d)
}

// ---- cap table -------------------------------------------------------------------------------

type holderRow struct {
	HolderLine
	Classes []classShares
}

type classShares struct {
	Name   string
	Shares Qty
}

type capData struct {
	Entity     Entity
	Table      CapTable
	Rows       []holderRow
	AsOf, View string
	Names      bool
	Full       bool
}

func (u *UI) capTable(c *uiCtx) error {
	// the cap table is checked first, so a caller without it learns nothing about the entity
	view, asof := c.query("view"), c.query("as_of")
	if view == "" {
		view = "fully-diluted"
	}
	names := c.query("names") != "no"
	ct, err := u.Svc.GetCapTable(c.ctx(), c.a, c.id(), asof, view, names)
	if err != nil {
		return err
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	d := capData{Entity: *e, Table: *ct, AsOf: ct.AsOf, View: view, Names: names, Full: view == "fully-diluted"}
	byClass := map[string]string{}
	for _, l := range ct.Classes {
		byClass[l.ClassID] = l.Name
	}
	for _, h := range ct.Holders {
		r := holderRow{HolderLine: h}
		for _, s := range h.Shares {
			r.Classes = append(r.Classes, classShares{byClass[s.ClassID], s.Shares})
		}
		d.Rows = append(d.Rows, r)
	}
	return u.view(c, "captable.html", "cap.title", d)
}

// ---- exports ---------------------------------------------------------------------------------

type exportsData struct{ Entity Entity }

func (u *UI) exportsPage(c *uiCtx) error {
	if !c.caps.Export {
		return ErrForbidden // the form only; the download asks the policy again
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, c.id())
	if err != nil {
		return err
	}
	return u.view(c, "exports.html", "export.title", exportsData{*e})
}

// exportDownload is a POST with the CSRF token: an export is the highest-risk read, so no link or
// image elsewhere can start one.
func (u *UI) exportDownload(c *uiCtx) error {
	name, asof := c.form("name"), c.form("as_of")
	var buf bytes.Buffer
	if err := u.Svc.ExportCSV(c.ctx(), c.a, c.id(), name, asof, &buf); err != nil {
		return err
	}
	file := "equity-" + name
	if _, err := parseDate(asof); err == nil {
		file += "-" + asof
	}
	h := c.w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file + ".csv"}))
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(buf.Bytes())
	return nil
}

// ---- stakeholders ----------------------------------------------------------------------------

type holderLine struct {
	Stakeholder
	Name    string
	Pending string
}

type stakeholdersData struct {
	Entity Entity
	Items  []holderLine
	Form   stakeholderForm
}

type stakeholderForm struct {
	Kind, PersonID, Name, Email, Error string
}

// holderNames is the names of the records, or none for a caller who may not read identities.
func (u *UI) holderNames(c *uiCtx, ids []string) (map[string]string, error) {
	n, err := u.Svc.HolderNames(c.ctx(), c.a, ids)
	if errors.Is(err, ErrForbidden) {
		return map[string]string{}, nil
	}
	return n, err
}

func (u *UI) stakeholderList(c *uiCtx, entityID string, form stakeholderForm, status int) error {
	list, err := u.Svc.ListStakeholders(c.ctx(), c.a, entityID)
	if err != nil {
		return err
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, entityID)
	if err != nil {
		return err
	}
	var ids []string
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	names, err := u.holderNames(c, ids)
	if err != nil {
		return err
	}
	d := stakeholdersData{Entity: *e, Form: form}
	for _, s := range list {
		d.Items = append(d.Items, holderLine{Stakeholder: s, Name: names[s.ID], Pending: u.Svc.ConfirmationNeeded(s)})
	}
	u.render(c.w, c.r, c, status, "stakeholders.html", "holder.list_title", d, "")
	return nil
}

func (u *UI) stakeholders(c *uiCtx) error {
	return u.stakeholderList(c, c.id(), stakeholderForm{Kind: "employee"}, http.StatusOK)
}

// refused reports a refusal that belongs on the form that was sent, with the status and message.
func (u *UI) refused(c *uiCtx, err error) (int, string, bool) {
	var inv *InvalidError
	if !errors.As(err, &inv) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrSeparation) {
		return 0, "", false
	}
	status, _, msg := u.explain(c.r, err)
	return status, msg, true
}

func (u *UI) stakeholderCreate(c *uiCtx) error {
	f := stakeholderForm{Kind: c.form("kind"), PersonID: c.form("person_id"), Name: c.form("name"), Email: c.form("email")}
	st, err := u.Svc.CreateStakeholder(c.ctx(), c.a, c.id(), StakeholderInput{Kind: f.Kind, PersonID: f.PersonID, Identity: Identity{Name: f.Name, Email: f.Email}})
	if err != nil {
		if status, msg, ok := u.refused(c, err); ok {
			f.Error = msg
			return u.stakeholderList(c, c.id(), f, status)
		}
		return err
	}
	c.done("/company/stakeholders/"+st.ID, "flash.holder_created")
	return nil
}

type stakeholderData struct {
	Entity  Entity
	Holder  Stakeholder
	Name    string
	Email   string
	TaxID   bool
	Pending string
	Mode    string
	Grants  []Grant
}

func (u *UI) stakeholderRender(c *uiCtx, id, flash string) error {
	st, err := u.Svc.GetStakeholder(c.ctx(), c.a, id)
	if err != nil {
		return err
	}
	idn, err := u.Svc.GetIdentity(c.ctx(), c.a, id)
	if err != nil {
		return err
	}
	e, err := u.Svc.GetEntity(c.ctx(), c.a, st.EntityID)
	if err != nil {
		return err
	}
	g, err := u.Svc.ListGrants(c.ctx(), c.a, st.EntityID, GrantFilter{StakeholderID: id, Limit: 100})
	if err != nil {
		return err
	}
	d := stakeholderData{Entity: *e, Holder: *st, Name: idn.Name, Email: idn.Email, TaxID: idn.TaxID != "",
		Pending: u.Svc.ConfirmationNeeded(*st), Mode: u.Svc.Mode(), Grants: g.Items}
	u.render(c.w, c.r, c, http.StatusOK, "stakeholder.html", "holder.title", d, flash)
	return nil
}

func (u *UI) stakeholderPage(c *uiCtx) error { return u.stakeholderRender(c, c.id(), "") }

// confirmation runs one of the two ways a second person confirms a holder record, after a step-up:
// there is no way to take a confirmation back.
func (u *UI) confirmation(c *uiCtx, flash string, fn func(a Actor, id string) (*Stakeholder, error)) error {
	a, err := u.stepUp(c)
	if err != nil {
		return err
	}
	if _, err := fn(a, c.id()); err != nil {
		return err
	}
	c.a, c.caps = a, u.Svc.Capabilities(a)
	return u.stakeholderRender(c, c.id(), flash)
}

func (u *UI) stakeholderVerify(c *uiCtx) error {
	return u.confirmation(c, "flash.holder_verified", func(a Actor, id string) (*Stakeholder, error) { return u.Svc.VerifyPerson(c.ctx(), a, id) })
}

func (u *UI) stakeholderAttest(c *uiCtx) error {
	return u.confirmation(c, "flash.holder_attested", func(a Actor, id string) (*Stakeholder, error) { return u.Svc.AttestExternal(c.ctx(), a, id) })
}

// ---- the verification queue ------------------------------------------------------------------

type reviewRow struct {
	ReviewItem
	Name, Entity string
}

type queueData struct {
	Items []reviewRow
	Mode  string
}

func (u *UI) identityQueue(c *uiCtx) error {
	items, err := u.Svc.IdentityReview(c.ctx(), c.a)
	if err != nil {
		return err
	}
	es, err := u.Svc.ListEntities(c.ctx(), c.a)
	if err != nil {
		return err
	}
	entities := map[string]string{}
	for _, e := range es {
		entities[e.ID] = e.Name
	}
	if len(items) > 500 {
		items = items[:500]
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.StakeholderID)
	}
	names, err := u.holderNames(c, ids)
	if err != nil {
		return err
	}
	d := queueData{Mode: u.Svc.Mode()}
	for _, it := range items {
		d.Items = append(d.Items, reviewRow{ReviewItem: it, Name: names[it.StakeholderID], Entity: entities[it.EntityID]})
	}
	return u.view(c, "identity.html", "identity.title", d)
}

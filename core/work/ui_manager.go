package work

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The manager views and saved filters (specs/065-work-manager).

type uiScope struct {
	Projects []Project // the unarchived projects the caller can see
	Selected string
}

type uiWorkloadView struct {
	uiScope
	W *Workload
}

type uiAgeingView struct {
	uiScope
	A *Ageing
}

type uiStaleView struct {
	uiScope
	S *Stale
}

// scope reads the project choice of a report. ok is false when a page has already been written.
func (u *webUI) scope(c *uiCtx) (s uiScope, ok bool) {
	s.Selected = c.r.URL.Query().Get("project")
	if s.Selected != "" && !projectKeyRE.MatchString(s.Selected) {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return s, false
	}
	ps, err := u.a.store.ListProjects(c.r.Context(), c.p)
	if err != nil {
		c.fail(err, "/ui/")
		return s, false
	}
	for _, p := range ps {
		if !p.Archived {
			s.Projects = append(s.Projects, p)
		}
	}
	return s, true
}

func (c *uiCtx) renderTeam(tab, page, title string, d any) {
	b := c.base(title, "team")
	b.TeamTab, b.D = tab, d
	c.render(http.StatusOK, page, b)
}

func (u *webUI) workloadPage(c *uiCtx) {
	s, ok := u.scope(c)
	if !ok {
		return
	}
	w, err := u.a.store.Workload(c.r.Context(), c.p, s.Selected)
	if err != nil {
		c.fail(err, "/ui/team/workload")
		return
	}
	c.renderTeam("workload", "workload", msg("workload.title"), &uiWorkloadView{uiScope: s, W: w})
}

func (u *webUI) ageingPage(c *uiCtx) {
	s, ok := u.scope(c)
	if !ok {
		return
	}
	a, err := u.a.store.Ageing(c.r.Context(), c.p, s.Selected)
	if err != nil {
		c.fail(err, "/ui/team/ageing")
		return
	}
	c.renderTeam("ageing", "ageing", msg("ageing.title"), &uiAgeingView{uiScope: s, A: a})
}

func (u *webUI) stalePage(c *uiCtx) {
	s, ok := u.scope(c)
	if !ok {
		return
	}
	days := 0
	if v := c.r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxStaleDays {
			c.errorView(http.StatusBadRequest, msg("err.invalid"), &uiErrorView{Message: msg("stale.bad_days", maxStaleDays), Back: "/ui/team/stale"})
			return
		}
		days = n
	}
	st, err := u.a.store.StaleWork(c.r.Context(), c.p, s.Selected, days)
	if err != nil {
		c.fail(err, "/ui/team/stale")
		return
	}
	c.renderTeam("stale", "stale", msg("stale.title"), &uiStaleView{uiScope: s, S: st})
}

// ---- saved filters ------------------------------------------------------------------------

type uiFiltersView struct {
	Filters  []SavedFilter
	Projects []Project
	Sorts    []string
	CanAdmin bool
}

type uiFilterView struct {
	Filter     *SavedFilter
	Issues     []Issue
	Sprints    map[int64]string
	CanDelete  bool
	OpenIn     string // the same search on the project page
	Page       int
	Prev, Next string
}

func (u *webUI) filtersPage(c *uiCtx) {
	fs, err := u.a.store.ListFilters(c.r.Context(), c.p)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	s, ok := u.scope(c)
	if !ok {
		return
	}
	b := c.base(msg("filters.title"), "filters")
	b.D = &uiFiltersView{Filters: fs, Projects: s.Projects, Sorts: uiSorts, CanAdmin: c.p.Can(PermAdmin)}
	c.render(http.StatusOK, "filters", b)
}

func (c *uiCtx) filterID() (int64, bool) {
	id, err := strconv.ParseInt(c.r.PathValue("filter"), 10, 64)
	if err != nil || id < 1 {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return 0, false
	}
	return id, true
}

func (u *webUI) filterPage(c *uiCtx) {
	id, ok := c.filterID()
	if !ok {
		return
	}
	ctx := c.r.Context()
	f, err := u.a.store.GetFilter(ctx, c.p, id)
	if err != nil {
		c.fail(err, "/ui/filters")
		return
	}
	page, _ := strconv.Atoi(c.r.URL.Query().Get("page"))
	page = min(max(page, 1), uiMaxPage)
	issues, next, err := u.a.store.ListIssues(ctx, c.p, f.Project, f.Query, f.Sort, uiPageSize, (page-1)*uiPageSize)
	if err != nil {
		c.fail(err, "/ui/filters")
		return
	}
	self := "/ui/filters/" + strconv.FormatInt(id, 10)
	v := &uiFilterView{Filter: f, Issues: issues, Sprints: map[int64]string{}, Page: page,
		CanDelete: c.p.Can(PermWrite) && (f.Mine || c.p.Can(PermAdmin))}
	if f.Project != "" {
		q := url.Values{"scope": {"all"}}
		if f.Query != "" {
			q.Set("q", f.Query)
		}
		if f.Sort != "rank" {
			q.Set("sort", f.Sort)
		}
		v.OpenIn = "/ui/projects/" + f.Project + "?" + q.Encode()
	}
	if page > 1 {
		v.Prev = self + "?page=" + strconv.Itoa(page-1)
	}
	if next >= 0 && page < uiMaxPage {
		v.Next = self + "?page=" + strconv.Itoa(page+1)
	}
	b := c.base(f.Name, "filters")
	b.D = v
	c.render(http.StatusOK, "filter", b)
}

func (u *webUI) filterCreate(c *uiCtx) {
	in := NewFilter{
		Name: c.formStr("name"), Project: c.formStr("project"), Query: strings.TrimSpace(c.r.PostFormValue("query")),
		Sort: c.formStr("sort"), Shared: c.formStr("shared") == "1",
	}
	f, err := u.a.store.CreateFilter(c.r.Context(), c.p, in)
	if err != nil {
		c.fail(err, "/ui/filters")
		return
	}
	c.redirect("/ui/filters/"+strconv.FormatInt(f.ID, 10), "filter_saved")
}

func (u *webUI) filterDelete(c *uiCtx) {
	id, ok := c.filterID()
	if !ok {
		return
	}
	if err := u.a.store.DeleteFilter(c.r.Context(), c.p, id); err != nil {
		c.fail(err, "/ui/filters")
		return
	}
	c.redirect("/ui/filters", "filter_deleted")
}

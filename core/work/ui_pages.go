package work

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	uiPageSize  = 25
	uiMaxPage   = 400 // 400 * 25 = the store's offset ceiling
	uiMaxPeople = 20
)

var (
	filterValueRE = regexp.MustCompile(`^[A-Za-z0-9_.@:+-]{1,100}$`)
	personRE      = regexp.MustCompile(`^[A-Za-z0-9_.@:+-]{1,254}$`)
	uiSorts       = []string{"rank", "-priority", "priority", "due", "-created", "created", "-updated", "updated"}
)

func (c *uiCtx) projectKey() (string, bool) {
	k := c.r.PathValue("key")
	if !projectKeyRE.MatchString(k) {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return "", false
	}
	return k, true
}

func (c *uiCtx) issueKey() (string, bool) {
	k := c.r.PathValue("issue")
	if !issueKeyRE.MatchString(k) {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return "", false
	}
	return k, true
}

func (c *uiCtx) formStr(name string) string { return strings.TrimSpace(c.r.PostFormValue(name)) }

// ---- project list ------------------------------------------------------------------------

type uiProjectsView struct{ Projects []Project }

func (u *webUI) projects(c *uiCtx) {
	ps, err := u.a.store.ListProjects(c.r.Context(), c.p)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	b := c.base(msg("projects.title"), "projects")
	b.D = &uiProjectsView{Projects: ps}
	c.render(http.StatusOK, "projects", b)
}

// ---- project home: filters, backlog list, inline create ---------------------------------------

type uiFilter struct{ Q, Status, Priority, Type, Assignee, Label, Sort, Scope string }

type uiProjectView struct {
	Project    *Project
	Filter     uiFilter
	Issues     []Issue
	Sprints    map[int64]string
	Page       int
	Prev, Next string
	Idem       string
	Types      []string
	Priorities []string
	Sorts      []string
	Statuses   []Status
	Query      string
	Members    []Member
}

// queryFromFilter composes the search-language query the filter form stands for. Free text goes
// through the language's own parser (the store validates it); the structured fields are limited to
// a conservative character set so they cannot smuggle extra terms, quotes or negations.
func queryFromFilter(f uiFilter) (string, []ProblemField) {
	var probs []ProblemField
	parts := []string{}
	if strings.TrimSpace(f.Q) != "" {
		parts = append(parts, strings.TrimSpace(f.Q))
	}
	for _, t := range []struct{ field, val string }{{"status", f.Status}, {"priority", f.Priority}, {"type", f.Type}, {"assignee", f.Assignee}, {"label", f.Label}} {
		if t.val == "" {
			continue
		}
		if !filterValueRE.MatchString(t.val) {
			probs = append(probs, ProblemField{Field: t.field, Message: msg("filter.invalid")})
			continue
		}
		parts = append(parts, t.field+":"+t.val)
	}
	if f.Scope != "all" {
		parts = append(parts, "is:open")
	}
	return strings.Join(parts, " "), probs
}

func filterFromURL(v url.Values) uiFilter {
	f := uiFilter{Q: v.Get("q"), Status: v.Get("status"), Priority: v.Get("priority"), Type: v.Get("type"), Assignee: v.Get("assignee"), Label: v.Get("label"), Sort: v.Get("sort"), Scope: v.Get("scope")}
	if !slices.Contains(uiSorts, f.Sort) {
		f.Sort = "rank"
	}
	if f.Scope != "all" {
		f.Scope = "open"
	}
	if len(f.Q) > maxQueryLen {
		f.Q = f.Q[:maxQueryLen]
	}
	return f
}

func (f uiFilter) values() url.Values {
	v := url.Values{}
	for k, val := range map[string]string{"q": f.Q, "status": f.Status, "priority": f.Priority, "type": f.Type, "assignee": f.Assignee, "label": f.Label} {
		if val != "" {
			v.Set(k, val)
		}
	}
	if f.Sort != "rank" {
		v.Set("sort", f.Sort)
	}
	if f.Scope == "all" {
		v.Set("scope", "all")
	}
	return v
}

func (u *webUI) projectPage(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	ctx := c.r.Context()
	proj, err := u.a.store.GetProject(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	q := c.r.URL.Query()
	f := filterFromURL(q)
	page, _ := strconv.Atoi(q.Get("page"))
	page = min(max(page, 1), uiMaxPage)
	query, probs := queryFromFilter(f)
	if len(probs) > 0 {
		c.errorView(http.StatusBadRequest, msg("err.invalid"), &uiErrorView{Message: msg("err.invalid_msg"), Problems: probs, Back: "/ui/projects/" + key})
		return
	}
	issues, next, err := u.a.store.ListIssues(ctx, c.p, key, query, f.Sort, uiPageSize, (page-1)*uiPageSize)
	if err != nil {
		var ie *InvalidError
		if asInvalid(err, &ie) {
			c.errorView(http.StatusBadRequest, msg("err.search"), &uiErrorView{Message: msg("err.search_msg"), Problems: ie.Problems, Back: "/ui/projects/" + key})
			return
		}
		c.fail(err, "/ui/projects/"+key)
		return
	}
	sprints, err := u.a.store.ListSprints(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	names := map[int64]string{}
	for _, s := range sprints {
		names[s.ID] = s.Name
	}
	pageURL := func(n int) string {
		v := f.values()
		if n > 1 {
			v.Set("page", strconv.Itoa(n))
		}
		if len(v) == 0 {
			return "/ui/projects/" + key
		}
		return "/ui/projects/" + key + "?" + v.Encode()
	}
	v := &uiProjectView{Project: proj, Filter: f, Issues: issues, Sprints: names, Page: page, Idem: randToken(16), Types: []string{"epic", "story", "task", "bug"}, Priorities: Priorities, Sorts: uiSorts, Statuses: proj.Workflow.Statuses, Query: query}
	if v.Members, err = u.a.store.ListMembers(ctx, c.p, key); err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	if page > 1 {
		v.Prev = pageURL(page - 1)
	}
	if next >= 0 && page < uiMaxPage {
		v.Next = pageURL(page + 1)
	}
	b := c.base(proj.Key+" "+proj.Name, "projects")
	b.Project, b.ProjectTab, b.D = proj.Key, "backlog", v
	c.render(http.StatusOK, "project", b)
}

// createIssue is the inline create form of the backlog. The form carries a random idempotency key,
// so a double click or a resubmitted form creates one issue.
func (u *webUI) createIssue(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	back := "/ui/projects/" + key
	var probs problems
	in := CreateIssue{Type: c.formStr("type"), Title: c.formStr("title"), Description: strings.ReplaceAll(c.r.PostFormValue("description"), "\r\n", "\n"), Priority: c.formStr("priority")}
	if a := c.formStr("assignee"); a != "" {
		in.Assignee = &a
	}
	if d := c.formStr("due_date"); d != "" {
		in.DueDate = &d
	}
	if e := c.formStr("estimate"); e != "" {
		if n, err := strconv.ParseFloat(e, 64); err != nil {
			probs.add("estimate", "%s", msg("filter.number"))
		} else {
			in.Estimate = &n
		}
	}
	in.Labels = splitLabels(c.r.PostFormValue("labels"))
	if err := probs.err(); err != nil {
		c.fail(err, back)
		return
	}
	idem := c.formStr("idem")
	if len(idem) < 8 || len(idem) > 128 {
		idem = ""
	}
	is, _, err := u.a.store.CreateIssue(c.r.Context(), c.p, key, in, idem)
	if err != nil {
		c.fail(err, back)
		return
	}
	_ = is
	c.redirect(back, "created")
}

func splitLabels(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' })
	if f == nil {
		return []string{}
	}
	return f
}

// ---- manager dashboard ---------------------------------------------------------------------

type uiDashboardGroup struct {
	Person string
	Issues []Issue
}

type uiDashboardView struct {
	Input     string
	Groups    []uiDashboardGroup
	Truncated bool
	Searched  bool
}

// dashboard shows open P0 and P1 issues for a list of people given in the URL. The list is just
// a query parameter: no people service is called, and the store's scope rules still apply.
func (u *webUI) dashboard(c *uiCtx) {
	raw := c.r.URL.Query().Get("assignees")
	v := &uiDashboardView{Input: raw}
	b := c.base(msg("dashboard.title"), "team")
	b.TeamTab, b.D = "dashboard", v
	people := []string{}
	for _, p := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if !slices.Contains(people, p) {
			people = append(people, p)
		}
	}
	if len(people) == 0 {
		c.render(http.StatusOK, "dashboard", b)
		return
	}
	if len(raw) > 4000 || len(people) > uiMaxPeople {
		c.errorView(http.StatusBadRequest, msg("err.invalid"), &uiErrorView{Message: msg("dashboard.too_many", uiMaxPeople), Back: "/ui/dashboard"})
		return
	}
	for _, p := range people {
		if !personRE.MatchString(p) || p == "me" || p == "none" { // the search language gives these words a meaning
			c.errorView(http.StatusBadRequest, msg("err.invalid"), &uiErrorView{Message: msg("dashboard.bad_person"), Back: "/ui/dashboard"})
			return
		}
	}
	const limit = 100
	issues, next, err := u.a.store.ListIssues(c.r.Context(), c.p, "", "assignee:"+strings.Join(people, ",")+" priority:P0,P1 is:open", "priority", limit, 0)
	if err != nil {
		c.fail(err, "/ui/dashboard")
		return
	}
	v.Searched, v.Truncated = true, next >= 0
	for _, p := range people {
		g := uiDashboardGroup{Person: p}
		for _, is := range issues {
			if is.Assignee != nil && *is.Assignee == p {
				g.Issues = append(g.Issues, is)
			}
		}
		v.Groups = append(v.Groups, g)
	}
	c.render(http.StatusOK, "dashboard", b)
}

// ---- epics ---------------------------------------------------------------------------------

type uiEpic struct {
	Issue
	Total, Done, Percent int
}

type uiEpicsView struct {
	Project    *Project
	Epics      []uiEpic
	Prev, Next string
	Approx     bool
}

func (u *webUI) epics(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	ctx := c.r.Context()
	proj, err := u.a.store.GetProject(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	page, _ := strconv.Atoi(c.r.URL.Query().Get("page"))
	page = min(max(page, 1), uiMaxPage)
	const per = 20 // the search language takes at most 20 values per term
	eps, next, err := u.a.store.ListIssues(ctx, c.p, key, "type:epic", "rank", per, (page-1)*per)
	if err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	v := &uiEpicsView{Project: proj}
	idx := map[string]int{}
	var keys []string
	for i, e := range eps {
		v.Epics = append(v.Epics, uiEpic{Issue: e})
		idx[e.Key] = i
		keys = append(keys, e.Key)
	}
	if len(keys) > 0 {
		// At most ten pages of children are read: progress stays bounded for a huge epic and the
		// page says so (Approx) instead of silently undercounting.
		off := 0
		for n := 0; n < 10; n++ {
			kids, nx, err := u.a.store.ListIssues(ctx, c.p, key, "parent:"+strings.Join(keys, ","), "created", 100, off)
			if err != nil {
				c.fail(err, "/ui/projects/"+key)
				return
			}
			for _, k := range kids {
				if k.Parent == nil {
					continue
				}
				if i, ok := idx[*k.Parent]; ok {
					v.Epics[i].Total++
					if k.StatusCategory == CategoryDone {
						v.Epics[i].Done++
					}
				}
			}
			if nx < 0 {
				break
			}
			if n == 9 {
				v.Approx = true
			}
			off = nx
		}
		for i := range v.Epics {
			if t := v.Epics[i].Total; t > 0 {
				v.Epics[i].Percent = v.Epics[i].Done * 100 / t
			}
		}
	}
	if page > 1 {
		v.Prev = "/ui/projects/" + key + "/epics?page=" + strconv.Itoa(page-1)
	}
	if next >= 0 && page < uiMaxPage {
		v.Next = "/ui/projects/" + key + "/epics?page=" + strconv.Itoa(page+1)
	}
	b := c.base(msg("epics.title"), "projects")
	b.Project, b.ProjectTab, b.D = key, "epics", v
	c.render(http.StatusOK, "epics", b)
}

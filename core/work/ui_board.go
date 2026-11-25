package work

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ---- board ---------------------------------------------------------------------------------

type uiMove struct {
	To   string
	Back bool
}

type uiCard struct {
	Issue
	Moves    []uiMove
	Up, Down string // neighbour keys in the same column ("" at the ends)
}

type uiColumn struct {
	Status, Category string
	Count            int
	Cards            []uiCard
}

type uiSprintTab struct {
	Label, Href string
	Current     bool
}

type uiBoardView struct {
	Project   *Project
	Board     *Board
	Columns   []uiColumn
	Tabs      []uiSprintTab
	Selection string
	NoActive  bool
	Limit     int
	Self      string // where actions return to
}

// board shows one column per workflow status. Moving a card is a button per allowed transition
// (no drag and drop), and ordering is up/down buttons; each carries the card's version, so a card
// somebody else just moved is reported instead of silently overwritten.
func (u *webUI) board(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	ctx := c.r.Context()
	sel := c.r.URL.Query().Get("sprint")
	if sel == "" {
		sel = "active"
	}
	proj, err := u.a.store.GetProject(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	bd, err := u.a.store.GetBoard(ctx, c.p, key, sel)
	if err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	sprints, err := u.a.store.ListSprints(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	v := &uiBoardView{Project: proj, Board: bd, Selection: sel, NoActive: sel == "active" && bd.Sprint == nil, Limit: boardColumnLimit}
	base := "/ui/projects/" + key + "/board"
	v.Self = base
	if r := c.r.URL.Query().Get("sprint"); r != "" {
		if _, ok := safeNext(base + "?sprint=" + url.QueryEscape(r)); ok {
			v.Self = base + "?sprint=" + url.QueryEscape(r)
		}
	}
	v.Tabs = []uiSprintTab{
		{msg("board.active"), base, sel == "active"},
		{msg("board.backlog"), base + "?sprint=backlog", sel == "backlog"},
		{msg("board.all"), base + "?sprint=all", sel == "all"},
	}
	for _, sp := range sprints {
		if sp.State == "planned" {
			id := strconv.FormatInt(sp.ID, 10)
			v.Tabs = append(v.Tabs, uiSprintTab{sp.Name, base + "?sprint=" + id, sel == id})
		}
	}
	order := map[string]int{}
	for i, s := range proj.Workflow.Statuses {
		order[s.Name] = i
	}
	for _, col := range bd.Columns {
		uc := uiColumn{Status: col.Status, Category: col.Category, Count: col.Count}
		for i, is := range col.Issues {
			card := uiCard{Issue: is}
			for _, to := range proj.Workflow.Next(is.Status) {
				card.Moves = append(card.Moves, uiMove{To: to, Back: order[to] < order[is.Status]})
			}
			if i > 0 {
				card.Up = col.Issues[i-1].Key
			}
			if i+1 < len(col.Issues) {
				card.Down = col.Issues[i+1].Key
			}
			uc.Cards = append(uc.Cards, card)
		}
		v.Columns = append(v.Columns, uc)
	}
	title := proj.Key + " " + msg("board.title")
	b := c.base(title, "projects")
	b.Project, b.ProjectTab, b.D = key, "board", v
	c.render(http.StatusOK, "board", b)
}

// ---- sprint planning -----------------------------------------------------------------------

type uiSprintBlock struct {
	Sprint
	Issues   []Issue
	More     bool
	Estimate float64
	Targets  []Sprint // planned sprints an unfinished issue can move to on completion
}

type uiSprintsView struct {
	Project     *Project
	Open        []uiSprintBlock
	Closed      []Sprint
	Backlog     []Issue
	BacklogMore bool
	Targets     []Sprint // sprints an issue can be added to
}

func (u *webUI) sprints(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	ctx := c.r.Context()
	st := u.a.store
	proj, err := st.GetProject(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	all, err := st.ListSprints(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	v := &uiSprintsView{Project: proj}
	var planned []Sprint
	for _, s := range all {
		if s.State == "planned" {
			planned = append(planned, s)
		}
	}
	shown := 0
	for i := len(all) - 1; i >= 0; i-- { // oldest first reads naturally for planning
		s := all[i]
		if s.State == "closed" {
			continue
		}
		v.Targets = append(v.Targets, s)
		if shown++; shown > 8 { // each open sprint costs one query: bounded
			continue
		}
		blk := uiSprintBlock{Sprint: s}
		items, next, err := st.ListIssues(ctx, c.p, key, "sprint:"+strconv.FormatInt(s.ID, 10), "rank", 100, 0)
		if err != nil {
			c.fail(err, "/ui/projects/"+key)
			return
		}
		blk.Issues, blk.More = items, next >= 0
		for _, is := range items {
			if is.Estimate != nil {
				blk.Estimate += *is.Estimate
			}
		}
		for _, p := range planned {
			if p.ID != s.ID {
				blk.Targets = append(blk.Targets, p)
			}
		}
		v.Open = append(v.Open, blk)
	}
	for _, s := range all {
		if s.State == "closed" && len(v.Closed) < 10 {
			v.Closed = append(v.Closed, s)
		}
	}
	items, next, err := st.ListIssues(ctx, c.p, key, "sprint:none is:open", "rank", 50, 0)
	if err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	v.Backlog, v.BacklogMore = items, next >= 0
	b := c.base(proj.Key+" "+msg("sprints.title"), "projects")
	b.Project, b.ProjectTab, b.D = key, "sprints", v
	c.render(http.StatusOK, "sprints", b)
}

func (u *webUI) createSprint(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	in := NewSprint{Name: c.formStr("name"), Goal: strings.ReplaceAll(c.r.PostFormValue("goal"), "\r\n", "\n")}
	if s := c.formStr("starts_on"); s != "" {
		in.StartsOn = &s
	}
	if s := c.formStr("ends_on"); s != "" {
		in.EndsOn = &s
	}
	back := "/ui/projects/" + key + "/sprints"
	if _, err := u.a.store.CreateSprint(c.r.Context(), c.p, key, in); err != nil {
		c.fail(err, back)
		return
	}
	c.redirect(back, "sprint_created")
}

func (c *uiCtx) sprintID() (int64, bool) {
	n, err := strconv.ParseInt(c.r.PathValue("id"), 10, 64)
	if err != nil || n < 1 {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return 0, false
	}
	return n, true
}

func (u *webUI) startSprint(c *uiCtx) {
	id, ok := c.sprintID()
	if !ok {
		return
	}
	sp, err := u.a.store.StartSprint(c.r.Context(), c.p, id)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	c.redirect("/ui/projects/"+sp.Project+"/sprints", "sprint_started")
}

func (u *webUI) completeSprint(c *uiCtx) {
	id, ok := c.sprintID()
	if !ok {
		return
	}
	var moveTo *int64
	if s := c.formStr("move_to"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			c.fail(invalid("move_to", "%s", msg("filter.number")), "/ui/")
			return
		}
		moveTo = &n
	}
	sp, err := u.a.store.CompleteSprint(c.r.Context(), c.p, id, moveTo)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	c.redirect("/ui/projects/"+sp.Project+"/sprints", "sprint_completed")
}

func asInvalid(err error, t **InvalidError) bool { return errors.As(err, t) }

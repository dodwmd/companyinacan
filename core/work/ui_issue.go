package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type uiComment struct {
	Comment
	HTML template.HTML
}

type uiActivity struct {
	Actor string
	At    string
	Text  string
}

type uiLink struct {
	Link
	Phrase string
}

type uiIssueView struct {
	Issue        *IssueDetail
	Description  template.HTML
	Comments     []uiComment
	CommentsMore string
	Activity     []uiActivity
	ActivityMore string
	Allowed      []string
	Sprints      []Sprint
	SprintName   string
	Parent       *Issue
	Children     []Issue
	Links        []uiLink
	Watching     bool
	Labels       string
	Priorities   []string
	LinkTypes    []string
	ClosedEdit   bool
	Attachments  []uiAttachment
}

type uiAttachment struct {
	Attachment
	Href string
}

func (c *uiCtx) cursorParam(name string) int64 {
	n, err := strconv.ParseInt(c.r.URL.Query().Get(name), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (u *webUI) issuePage(c *uiCtx) {
	key, ok := c.issueKey()
	if !ok {
		return
	}
	ctx := c.r.Context()
	s := u.a.store
	d, err := s.GetIssue(ctx, c.p, key)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	v := &uiIssueView{Issue: d, Description: renderText(d.Description), Labels: strings.Join(d.Labels, " "), Priorities: Priorities, LinkTypes: LinkTypes}
	comments, cn, err := s.ListComments(ctx, c.p, key, c.cursorParam("ca"), 30)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	for _, cm := range comments {
		v.Comments = append(v.Comments, uiComment{Comment: cm, HTML: renderText(cm.Body)})
	}
	if cn > 0 {
		v.CommentsMore = "/ui/issues/" + key + "?ca=" + strconv.FormatInt(cn, 10) + "#comments"
	}
	acts, an, err := s.ListActivity(ctx, c.p, key, c.cursorParam("aa"), 30)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	for _, a := range acts {
		v.Activity = append(v.Activity, uiActivity{Actor: a.Actor, At: a.At.UTC().Format("2006-01-02 15:04"), Text: activityText(a)})
	}
	if an > 0 {
		v.ActivityMore = "/ui/issues/" + key + "?aa=" + strconv.FormatInt(an, 10) + "#activity"
	}
	if v.Allowed, err = s.AllowedTransitions(ctx, c.p, key); err != nil {
		c.fail(err, "/ui/")
		return
	}
	sprints, err := s.ListSprints(ctx, c.p, d.Project)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	for _, sp := range sprints {
		if sp.State != "closed" || (d.SprintID != nil && *d.SprintID == sp.ID) {
			v.Sprints = append(v.Sprints, sp)
		}
		if d.SprintID != nil && *d.SprintID == sp.ID {
			v.SprintName = sp.Name
		}
	}
	if d.Parent != nil {
		if pd, err := s.GetIssue(ctx, c.p, *d.Parent); err == nil {
			v.Parent = &pd.Issue
		}
	}
	if len(d.Children) > 0 {
		kids, _, err := s.ListIssues(ctx, c.p, d.Project, "parent:"+d.Key, "rank", 100, 0)
		if err != nil {
			c.fail(err, "/ui/")
			return
		}
		v.Children = kids
	}
	for _, l := range d.Links {
		v.Links = append(v.Links, uiLink{Link: l, Phrase: msg("link." + l.Type + "." + l.Direction)})
	}
	v.Watching = slices.Contains(d.Watchers, c.p.Subject)
	for _, a := range d.Attachments {
		ua := uiAttachment{Attachment: a}
		if h, ok := safeURL(a.URL); ok {
			ua.Href = h
		}
		v.Attachments = append(v.Attachments, ua)
	}
	b := c.base(d.Key+" "+d.Title, "projects")
	b.Project, b.ProjectTab, b.D = d.Project, "", v
	c.render(http.StatusOK, "issue", b)
}

// activityText summarises one history entry as plain text (the template escapes it).
func activityText(a Activity) string {
	short := func(v any) string {
		if v == nil {
			return msg("activity.none")
		}
		s := fmt.Sprint(v)
		if utf8.RuneCountInString(s) > 60 {
			s = string([]rune(s)[:60]) + "..."
		}
		return s
	}
	switch a.Kind {
	case "created":
		var d struct{ Type, Priority string }
		_ = json.Unmarshal(a.Data, &d)
		return msg("activity.created", short(d.Type), short(d.Priority))
	case "transitioned":
		var d struct{ From, To string }
		_ = json.Unmarshal(a.Data, &d)
		return msg("activity.transitioned", short(d.From), short(d.To))
	case "updated":
		var d map[string]Change
		_ = json.Unmarshal(a.Data, &d)
		fields := make([]string, 0, len(d))
		for f := range d {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		parts := make([]string, 0, len(fields))
		for _, f := range fields {
			if f == "description" {
				parts = append(parts, msg("activity.description"))
				continue
			}
			parts = append(parts, msg("activity.change", short(f), short(d[f].From), short(d[f].To)))
		}
		return msg("activity.updated", strings.Join(parts, "; "))
	case "linked":
		var d struct{ Type, From, To string }
		_ = json.Unmarshal(a.Data, &d)
		return msg("activity.linked", short(d.From), short(d.Type), short(d.To))
	case "commented", "ranked", "unlinked", "attachment_added", "attachment_removed":
		return msg("activity." + a.Kind)
	}
	return short(a.Kind)
}

// ---- actions -------------------------------------------------------------------------------

func (c *uiCtx) version() (int64, bool) {
	n, err := strconv.ParseInt(c.r.PostFormValue("version"), 10, 64)
	if err != nil || n < 1 {
		c.errorPage(http.StatusBadRequest, "err.bad_request", "err.version_missing")
		return 0, false
	}
	return n, true
}

// issueAction runs a versioned change on an issue and redirects back. A stale version is reported
// on a conflict page and nothing is applied.
func (c *uiCtx) issueAction(flash string, fn func(key string, ver int64) error) {
	key, ok := c.issueKey()
	if !ok {
		return
	}
	ver, ok := c.version()
	if !ok {
		return
	}
	back := "/ui/issues/" + key
	if err := fn(key, ver); err != nil {
		if errors.Is(err, ErrVersionMismatch) {
			c.conflict(key)
			return
		}
		c.fail(err, back)
		return
	}
	c.redirect(back, flash)
}

// conflict explains a lost update: it shows who changed what, the form values the person had typed
// are kept (edits) and the way back is the current issue.
func (c *uiCtx) conflict(key string) {
	v := &uiErrorView{Message: msg("err.stale_msg"), Back: "/ui/issues/" + key, BackText: msg("err.reload")}
	if cur, err := c.u.a.store.GetIssue(c.r.Context(), c.p, key); err == nil {
		v.Extra = &uiConflictView{Key: cur.Key, Title: cur.Title, Version: cur.Version, Status: cur.Status, UpdatedAt: cur.UpdatedAt.UTC().Format("2006-01-02 15:04 MST")}
	}
	if c.r.URL.Path == "/ui/issues/"+key+"/edit" {
		if v.Extra == nil {
			v.Extra = &uiConflictView{}
		}
		x := v.Extra.(*uiConflictView)
		x.YourTitle, x.YourDescription, x.Edit = c.r.PostFormValue("title"), c.r.PostFormValue("description"), true
	}
	c.errorView(http.StatusConflict, msg("err.stale"), v)
}

type uiConflictView struct {
	Key, Title, Status, UpdatedAt string
	Version                       int64
	Edit                          bool
	YourTitle, YourDescription    string
}

func (u *webUI) issueTransition(c *uiCtx) {
	c.issueAction("moved", func(key string, ver int64) error {
		_, err := u.a.store.Transition(c.r.Context(), c.p, key, c.formStr("to"), &ver)
		return err
	})
}

func (u *webUI) patch(c *uiCtx, flash string, build func(*Patch) error) {
	c.issueAction(flash, func(key string, ver int64) error {
		var pt Patch
		if err := build(&pt); err != nil {
			return err
		}
		_, err := u.a.store.UpdateIssue(c.r.Context(), c.p, key, &pt, &ver)
		return err
	})
}

func (u *webUI) issuePriority(c *uiCtx) {
	u.patch(c, "updated", func(pt *Patch) error {
		pt.Priority = Opt[string]{Set: true, V: c.formStr("priority")}
		return nil
	})
}

func (u *webUI) issueAssignee(c *uiCtx) {
	u.patch(c, "updated", func(pt *Patch) error {
		if a := c.formStr("assignee"); a == "" {
			pt.Assignee = Opt[string]{Set: true, Null: true}
		} else {
			pt.Assignee = Opt[string]{Set: true, V: a}
		}
		return nil
	})
}

func (u *webUI) issueSprint(c *uiCtx) {
	u.patch(c, "updated", func(pt *Patch) error {
		s := c.formStr("sprint")
		if s == "" {
			pt.SprintID = Opt[int64]{Set: true, Null: true}
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			return invalid("sprint", "%s", msg("filter.number"))
		}
		pt.SprintID = Opt[int64]{Set: true, V: n}
		return nil
	})
}

func (u *webUI) issueEdit(c *uiCtx) {
	u.patch(c, "updated", func(pt *Patch) error {
		var p problems
		pt.Title = Opt[string]{Set: true, V: c.formStr("title")}
		pt.Description = Opt[string]{Set: true, V: strings.ReplaceAll(c.r.PostFormValue("description"), "\r\n", "\n")}
		pt.Labels = Opt[[]string]{Set: true, V: splitLabels(c.r.PostFormValue("labels"))}
		if e := c.formStr("estimate"); e == "" {
			pt.Estimate = Opt[float64]{Set: true, Null: true}
		} else if n, err := strconv.ParseFloat(e, 64); err != nil {
			p.add("estimate", "%s", msg("filter.number"))
		} else {
			pt.Estimate = Opt[float64]{Set: true, V: n}
		}
		if d := c.formStr("due_date"); d == "" {
			pt.DueDate = Opt[string]{Set: true, Null: true}
		} else {
			pt.DueDate = Opt[string]{Set: true, V: d}
		}
		if pa := strings.ToUpper(c.formStr("parent")); pa == "" {
			pt.Parent = Opt[string]{Set: true, Null: true}
		} else {
			pt.Parent = Opt[string]{Set: true, V: pa}
		}
		return p.err()
	})
}

func (u *webUI) issueRank(c *uiCtx) {
	c.issueAction("ranked", func(key string, ver int64) error {
		var req RankRequest
		switch {
		case c.formStr("before") != "":
			req.Before = strings.ToUpper(c.formStr("before"))
		case c.formStr("after") != "":
			req.After = strings.ToUpper(c.formStr("after"))
		default:
			req.Position = c.formStr("position")
		}
		// Reordering does not bump the issue's version, so a stale card is detected here: the
		// version the page showed must still be the current one.
		cur, err := u.a.store.GetIssue(c.r.Context(), c.p, key)
		if err != nil {
			return err
		}
		if cur.Version != ver {
			return ErrVersionMismatch
		}
		_, err = u.a.store.Rank(c.r.Context(), c.p, key, req)
		return err
	})
}

func (u *webUI) issueComment(c *uiCtx) {
	key, ok := c.issueKey()
	if !ok {
		return
	}
	if _, err := u.a.store.AddComment(c.r.Context(), c.p, key, strings.ReplaceAll(c.r.PostFormValue("body"), "\r\n", "\n")); err != nil {
		c.fail(err, "/ui/issues/"+key)
		return
	}
	c.redirect("/ui/issues/"+key+"#comments", "commented")
}

func (u *webUI) issueLinkAdd(c *uiCtx) {
	key, ok := c.issueKey()
	if !ok {
		return
	}
	if _, err := u.a.store.AddLink(c.r.Context(), c.p, key, c.formStr("link_type"), strings.ToUpper(c.formStr("target"))); err != nil {
		c.fail(err, "/ui/issues/"+key)
		return
	}
	c.redirect("/ui/issues/"+key, "linked")
}

func (u *webUI) issueLinkRemove(c *uiCtx) {
	key, ok := c.issueKey()
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.r.PathValue("link"), 10, 64)
	if err != nil || id < 1 {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	if err := u.a.store.RemoveLink(c.r.Context(), c.p, key, id); err != nil {
		c.fail(err, "/ui/issues/"+key)
		return
	}
	c.redirect("/ui/issues/"+key, "unlinked")
}

func (u *webUI) issueWatch(c *uiCtx) {
	key, ok := c.issueKey()
	if !ok {
		return
	}
	on := c.formStr("on") != "0"
	if err := u.a.store.SetWatcher(c.r.Context(), c.p, key, c.p.Subject, on); err != nil {
		c.fail(err, "/ui/issues/"+key)
		return
	}
	flash := "watching"
	if !on {
		flash = "unwatched"
	}
	c.redirect("/ui/issues/"+key, flash)
}

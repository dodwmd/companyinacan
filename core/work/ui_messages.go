package work

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

// messagesEN is the UI's message catalogue. Every user-visible string lives here, addressed by a
// dotted key, so another language is one more map. Values may contain fmt verbs; templates call
// them as {{t "key" args...}}. Keys built at run time (type.<t>, sort.<s>, sprint.<state>,
// linktype.<t>, link.<t>.<direction>, activity.<kind>, flash.<name>) are listed explicitly and
// checked by a test.
var messagesEN = map[string]string{
	"app.name":   "Work tracker",
	"app.footer": "Company in a Can work tracker. Apache-2.0.",
	"a11y.skip":  "Skip to main content",

	"nav.label":     "Main",
	"nav.project":   "Project",
	"nav.projects":  "Projects",
	"nav.dashboard": "Urgent by person",
	"nav.team":      "Team",
	"nav.workload":  "Workload",
	"nav.ageing":    "P0 and P1 ageing",
	"nav.stale":     "Stale work",
	"nav.filters":   "Saved filters",
	"nav.settings":  "Settings",
	"nav.logout":    "Log out",
	"nav.backlog":   "Backlog",
	"nav.board":     "Board",
	"nav.sprints":   "Sprints",
	"nav.epics":     "Epics",
	"nav.pages":     "Pages",

	"render.truncated": "Text shortened here to keep the page fast. The full text is in the edit form and the API.",

	"login.title":   "Sign in",
	"login.intro":   "Sign in with a personal access token. Create one in the admin UI (Tokens) with the work permissions you need.",
	"login.token":   "Personal access token",
	"login.hint":    "The token is checked and then kept only in the server's memory. It is not stored in your browser, and it is never shown again.",
	"login.submit":  "Sign in",
	"login.invalid": "That token was not accepted. Check that it is complete, not expired or revoked, and has a work permission.",
	"login.locked":  "Too many failed attempts from this address. Wait a minute and try again.",
	"login.csrf":    "The sign-in form expired. Reload the page and try again.",

	"login.sso_only":        "Sign in from the Apps page of the admin UI. Pasting an access token is switched off here.",
	"login.token_off":       "Pasting an access token is switched off. Open the tracker from the Apps page of the admin UI.",
	"login.ticket_invalid":  "That sign-in link was not accepted. A link works once, for about a minute, and only in the browser that opened it from the admin UI. Open the tracker again from the Apps page.",
	"login.sso_unavailable": "Single sign-on is not available right now. Try again in a moment.",
	"ticket.title":          "Continue with single sign-on",
	"ticket.intro":          "You came here from the admin UI. Check the account, then press Continue. Nothing happens until you do. If it is not your account, close this page.",
	"ticket.submit":         "Continue",
	"ticket.account":        "You are about to sign in as",
	"ticket.other":          "This browser is already signed in as another account:",
	"ticket.replace":        "Sign that account out here and sign in as the one above",

	"projects.title":    "Projects",
	"projects.caption":  "Projects you can see",
	"projects.empty":    "There are no projects you can see yet. Ask an administrator to create one or to widen your token.",
	"projects.archived": "archived",

	"project.new":           "New project",
	"project.new_intro":     "A project holds issues, boards and sprints. You can change its name and description later.",
	"project.key_hint":      "2 to 10 letters or digits, starting with a letter, for example WEB. It starts every issue number (WEB-1) and cannot be changed.",
	"project.create":        "Create project",
	"project.cancel":        "Cancel",
	"project.archived_note": "This project is archived: it can be read, but nothing in it can change until it is restored.",

	"first.title":  "Start by creating your first project",
	"first.intro":  "Issues, boards and sprints live in a project. Create one, then add the people who work on it and their first tasks.",
	"first.create": "Create your first project",

	"settings.title":   "%s settings",
	"settings.details": "Name and description",

	"members.title":  "Members",
	"members.intro":  "Who works on this project. Members are suggested as assignees and are listed in the workload view even when they have nothing to do. Membership does not change who can see or edit the project.",
	"members.none":   "No members yet.",
	"members.person": "Person",
	"members.hint":   "As it should appear as assignee. People who sign in from the admin UI are user:name.",
	"members.add":    "Add member",
	"members.remove": "Remove",

	"archive.title":         "Archive project",
	"archive.intro":         "An archived project stays readable but nothing in it can change, and it is left out of the team views. You can restore it later.",
	"archive.submit":        "Archive this project",
	"archive.restore_title": "Restore project",
	"archive.restore_intro": "Restoring makes the project, its members and its issues editable again.",
	"archive.restore":       "Restore this project",

	"workload.title":     "Who is on what",
	"workload.intro":     "Unresolved work by person, most urgent first. Epics and archived projects are left out.",
	"workload.caption":   "Unresolved issues per person",
	"workload.person":    "Person",
	"workload.open":      "Open",
	"workload.wip":       "In progress",
	"workload.blocked":   "Blocked",
	"workload.overdue":   "Overdue",
	"workload.current":   "Working on",
	"workload.nothing":   "Nothing in progress",
	"workload.more":      "Only the first few are listed.",
	"workload.truncated": "Only the first 500 people are shown. Choose one project to narrow the list.",
	"workload.empty":     "There is no unresolved work and no project has members.",

	"ageing.title":         "P0 and P1 ageing",
	"ageing.intro":         "Unresolved P0 and P1 issues, by how long they have had that priority. Past target means longer than the project's resolution hint.",
	"ageing.bands_caption": "How many, by time at this priority",
	"ageing.under2":        "Under 2 days",
	"ageing.under7":        "2 to 7 days",
	"ageing.under28":       "7 to 28 days",
	"ageing.over28":        "28 days or more",
	"ageing.past":          "Past target",
	"ageing.caption":       "Unresolved P0 and P1 issues, longest first within each priority",
	"ageing.at_priority":   "At this priority",
	"ageing.open_for":      "Open for",
	"ageing.past_mark":     "(past target)",
	"ageing.empty":         "No unresolved P0 or P1 issues.",

	"stale.title":     "Stale work",
	"stale.intro":     "Issues in progress with no activity for %d days or more. Reordering a card is not activity.",
	"stale.days":      "Quiet for at least (days)",
	"stale.caption":   "Issues in progress, quietest first",
	"stale.quiet_for": "Quiet for",
	"stale.empty":     "Nothing in progress has been quiet that long.",
	"stale.bad_days":  "Give a number of days between 1 and %d.",

	"age.days":         "%d d",
	"report.truncated": "Only the first 200 issues are listed. Choose one project to narrow the list.",

	"filters.title":         "Saved filters",
	"filters.intro":         "A saved filter is a search with a name. Running it shows what you are allowed to see; in a shared filter, \"me\" means whoever runs it.",
	"filters.caption":       "Your filters and the shared ones",
	"filters.empty":         "No saved filters yet. Save one here, or from a project's filter.",
	"filters.name":          "Filter name",
	"filters.project":       "Project",
	"filters.every_project": "Every project",
	"filters.query":         "Search",
	"filters.query_hint":    "For example: assignee:me priority:P0,P1 is:open. Empty matches every issue.",
	"filters.everything":    "every issue",
	"filters.who":           "Visible to",
	"filters.only_you":      "Only you",
	"filters.shared_by_you": "Everyone (yours)",
	"filters.shared_by":     "Everyone, saved by %s",
	"filters.share":         "Share with everyone who can see these issues",
	"filters.new":           "New saved filter",
	"filters.save":          "Save filter",
	"filters.save_this":     "Save this filter",
	"filters.delete":        "Delete filter",
	"filters.all":           "All saved filters",
	"filters.open_in":       "Open in %s",
	"filters.no_match":      "No issues match this filter now.",

	"col.key":      "Key",
	"col.type":     "Type",
	"col.priority": "Priority",
	"col.title":    "Title",
	"col.status":   "Status",
	"col.assignee": "Assignee",
	"col.labels":   "Labels",
	"col.estimate": "Estimate",
	"col.due":      "Due",
	"col.sprint":   "Sprint",
	"col.name":     "Name",
	"col.issues":   "Issues",
	"col.views":    "Views",
	"col.actions":  "Actions",
	"col.progress": "Progress",

	"status.todo": "To do",

	"type.epic":    "Epic",
	"type.story":   "Story",
	"type.task":    "Task",
	"type.bug":     "Bug",
	"type.subtask": "Subtask",

	"sort.rank":      "Backlog order",
	"sort.-priority": "Priority, lowest first",
	"sort.priority":  "Priority, highest first",
	"sort.due":       "Due date",
	"sort.-created":  "Newest first",
	"sort.created":   "Oldest first",
	"sort.-updated":  "Recently updated",
	"sort.updated":   "Least recently updated",

	"filter.title":         "Filter",
	"filter.q":             "Search",
	"filter.q_hint":        "Words search titles and descriptions. Terms such as status:in_progress, label:web, due<2026-12-01 or -status:done narrow the list. The address of this page keeps the filter, so you can bookmark or share it.",
	"filter.assignee_hint": "An email address, me, or none.",
	"filter.any":           "Any",
	"filter.label":         "Label",
	"filter.sort":          "Sort by",
	"filter.show":          "Show",
	"filter.open":          "Open issues",
	"filter.all":           "All issues, including done",
	"filter.apply":         "Apply filter",
	"filter.clear":         "Clear filter",
	"filter.invalid":       "Use letters, digits and . _ - : @ + only.",
	"filter.number":        "Must be a number.",

	"create.title":       "Create an issue",
	"create.submit":      "Create issue",
	"create.labels_hint": "Labels are lower case words separated by spaces or commas.",

	"backlog.title":   "Issues",
	"backlog.caption": "Issues matching the filter",
	"backlog.empty":   "No issues match. Change the filter, or create the first issue above.",

	"pager.prev": "Previous page",
	"pager.next": "Next page",
	"pager.page": "Page %d",
	"pager.more": "Show more",

	"issue.priority":       "Priority",
	"issue.unassigned":     "Unassigned",
	"issue.overdue":        "(overdue)",
	"issue.title":          "Title",
	"issue.description":    "Description",
	"issue.no_description": "No description.",
	"issue.edit":           "Edit title, description and details",
	"issue.save":           "Save changes",
	"issue.markdown_hint":  "Plain text with a little formatting: **bold**, `code`, - bullet lists, and [text](https://example.com) links. HTML is shown as text.",
	"issue.parent":         "Parent",
	"issue.children":       "Child issues",
	"issue.links":          "Linked issues",
	"issue.no_links":       "No links.",
	"issue.unlink":         "Remove link",
	"issue.link_type":      "Link type",
	"issue.link_target":    "Issue key",
	"issue.link_add":       "Add link",
	"issue.attachments":    "Attachments",
	"issue.no_attachments": "No attachments. Attachments are registered through the API; only their details are shown here.",
	"issue.comments":       "Comments",
	"issue.comment_by":     "Comment by %s",
	"issue.no_comments":    "No comments yet.",
	"issue.add_comment":    "Add a comment",
	"issue.comment":        "Comment",
	"issue.activity":       "Activity",
	"issue.details":        "Details",
	"issue.sla":            "(respond in %dh, resolve in %dh)",
	"issue.reporter":       "Reporter",
	"issue.no_sprint":      "No sprint (backlog)",
	"issue.created":        "Created",
	"issue.updated":        "Updated",
	"issue.move":           "Move to",
	"issue.terminal":       "No further moves from this status.",
	"issue.set":            "Set",
	"issue.assignee_hint":  "Leave empty to unassign.",
	"issue.assign_me":      "Assign to me",
	"issue.watchers":       "Watchers",
	"issue.watch":          "Watch this issue",
	"issue.unwatch":        "Stop watching",
	"issue.no_watchers":    "Nobody is watching.",

	"linktype.blocks":     "blocks",
	"linktype.relates":    "relates to",
	"linktype.duplicates": "duplicates",

	"link.blocks.outbound":     "Blocks",
	"link.blocks.inbound":      "Is blocked by",
	"link.relates.outbound":    "Relates to",
	"link.relates.inbound":     "Relates to",
	"link.duplicates.outbound": "Duplicates",
	"link.duplicates.inbound":  "Is duplicated by",

	"activity.none":               "none",
	"activity.created":            "created this %s (%s)",
	"activity.transitioned":       "moved it from %s to %s",
	"activity.updated":            "changed %s",
	"activity.description":        "the description",
	"activity.change":             "%s from %s to %s",
	"activity.linked":             "linked %s %s %s",
	"activity.linked_hidden":      "linked another issue",
	"activity.commented":          "commented",
	"activity.ranked":             "changed its order in the backlog",
	"activity.unlinked":           "removed a link",
	"activity.attachment_added":   "added an attachment",
	"activity.attachment_removed": "removed an attachment",

	"board.title":        "board",
	"board.views":        "Board views",
	"board.active":       "Active sprint",
	"board.backlog":      "Backlog",
	"board.all":          "All issues",
	"board.no_active":    "No active sprint.",
	"board.plan":         "Plan a sprint",
	"board.hint":         "Use the buttons on a card to move it along the workflow or to change its order. Each button only works while the card is unchanged since this page loaded.",
	"board.up":           "Up",
	"board.down":         "Down",
	"board.truncated":    "Showing %d of %d issues in this column. Use the backlog list to see the rest.",
	"board.empty_column": "Nothing here.",

	"sprint.planned": "planned",
	"sprint.active":  "active",
	"sprint.closed":  "closed",

	"sprints.title":           "sprints",
	"sprints.points":          "%g points",
	"sprints.start":           "Start sprint",
	"sprints.complete":        "Complete sprint",
	"sprints.move_to":         "Move unfinished issues to",
	"sprints.to_backlog":      "Backlog",
	"sprints.issues_caption":  "Issues in %s",
	"sprints.remove":          "Remove from sprint",
	"sprints.more":            "Only the first 100 issues are shown.",
	"sprints.empty":           "No issues in this sprint yet. Add issues from the backlog below.",
	"sprints.none":            "There is no planned or active sprint. Create one below.",
	"sprints.new":             "Create a sprint",
	"sprints.name":            "Name",
	"sprints.goal":            "Goal",
	"sprints.starts":          "Starts on",
	"sprints.ends":            "Ends on",
	"sprints.create":          "Create sprint",
	"sprints.backlog":         "Backlog",
	"sprints.backlog_caption": "Open issues that are not in a sprint",
	"sprints.add_to":          "Add to sprint",
	"sprints.add":             "Add to sprint",
	"sprints.more_backlog":    "Only the first 50 issues are shown. Use the filter on the %s page to find others.",
	"sprints.backlog_empty":   "Every open issue is in a sprint.",
	"sprints.closed":          "Closed sprints",

	"epics.title":       "epics",
	"epics.caption":     "Epics and the progress of their child issues",
	"epics.progress_of": "Progress of %s",
	"epics.done_of":     "%d of %d done",
	"epics.no_children": "No child issues",
	"epics.approx":      "This epic has many child issues; the counts cover the first 1000.",
	"epics.empty":       "This project has no epics.",

	"dashboard.title":      "Urgent work by person",
	"dashboard.intro":      "Open P0 and P1 issues across your team, in every project you can see.",
	"dashboard.people":     "People",
	"dashboard.hint":       "Enter the people on your team as email addresses or user names, separated by commas. The list is kept in the page address, so you can bookmark it.",
	"dashboard.show":       "Show issues",
	"dashboard.caption":    "Open P0 and P1 issues assigned to %s",
	"dashboard.none":       "No open P0 or P1 issues.",
	"dashboard.truncated":  "Only the first 100 issues are shown.",
	"dashboard.too_many":   "Give at most %d people.",
	"dashboard.bad_person": "A name contains characters that cannot be searched for. Use letters, digits and . _ - : @ + only.",

	"conflict.current":    "The issue now",
	"conflict.version":    "version %d, updated",
	"conflict.yours":      "Your changes",
	"conflict.yours_hint": "Copy anything you want to keep, then reload the issue and apply it again.",

	"err.home":            "Back to projects",
	"err.back":            "Go back",
	"err.reload":          "Reload the issue",
	"err.not_found_title": "Not found",
	"err.not_found":       "There is nothing here, or you cannot see it.",
	"err.forbidden":       "Request refused",
	"err.forbidden_msg":   "The form was not accepted (missing or invalid security token, or it came from another site). Reload the page and try again.",
	"err.admin_only":      "Only someone with work.admin can create or change projects.",
	"err.read_only":       "Read only",
	"err.read_only_msg":   "Your token can read but not change work items. Sign in with a token that has work.write.",
	"err.bad_request":     "Bad request",
	"err.bad_request_msg": "The form could not be read.",
	"err.version_missing": "The form is missing the version of the issue it was made for. Reload the page and try again.",
	"err.too_large":       "Too large",
	"err.too_large_msg":   "The form is too large.",
	"err.busy":            "Slow down",
	"err.busy_msg":        "Too many requests. Try again in a moment.",
	"err.method":          "Not allowed",
	"err.method_msg":      "That address does not accept this kind of request.",
	"err.invalid":         "Check the form",
	"err.invalid_msg":     "Some values were not accepted. Nothing was saved.",
	"err.search":          "The search was not understood",
	"err.search_msg":      "The filter could not be applied.",
	"err.stale":           "This issue changed",
	"err.stale_msg":       "This issue changed since you opened the page, so your change was not applied. Reload the issue and try again.",
	"err.conflict":        "Not possible",
	"err.not_allowed":     "Your token is not allowed to do that.",
	"err.slow":            "The search took too long",
	"err.slow_msg":        "It was stopped. Narrow the filter (add a status, label or date range) and try again.",
	"err.internal":        "Something went wrong",
	"err.internal_msg":    "The request failed and was logged. Request id: %s",

	"flash.created":          "Issue created.",
	"flash.updated":          "Issue updated.",
	"flash.moved":            "Issue moved.",
	"flash.ranked":           "Order changed.",
	"flash.commented":        "Comment added.",
	"flash.linked":           "Link added.",
	"flash.unlinked":         "Link removed.",
	"flash.watching":         "You are watching this issue.",
	"flash.unwatched":        "You stopped watching this issue.",
	"flash.sprint_created":   "Sprint created.",
	"flash.sprint_started":   "Sprint started.",
	"flash.sprint_completed": "Sprint completed.",
	"flash.logged_out":       "You have been signed out.",
	"flash.project_created":  "Project created. Add its members under Settings, or create the first issue below.",
	"flash.project_saved":    "Project saved.",
	"flash.project_archived": "Project archived.",
	"flash.project_restored": "Project restored.",
	"flash.member_added":     "Member added.",
	"flash.member_removed":   "Member removed.",
	"flash.filter_saved":     "Filter saved.",
	"flash.filter_deleted":   "Filter deleted.",
}

func msg(key string, args ...any) string {
	m, ok := messagesEN[key]
	if !ok {
		return "!!" + key + "!!"
	}
	if len(args) == 0 {
		return m
	}
	return fmt.Sprintf(m, args...)
}

func (u *webUI) funcs() template.FuncMap {
	now := func() time.Time {
		if u.now != nil {
			return u.now()
		}
		return time.Now()
	}
	return template.FuncMap{
		"t":  msg,
		"md": renderText,
		// stat shows a workflow status name ("in_progress") as words ("In progress").
		"stat": func(s string) string {
			if m, ok := messagesEN["status."+s]; ok {
				return m
			}
			s = strings.ReplaceAll(s, "_", " ")
			if s == "" {
				return s
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
		"overdue": func(due *string, category string) bool {
			return due != nil && category != CategoryDone && *due < now().UTC().Format(time.DateOnly)
		},
		"fmtTime": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 MST") },
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"sprintOf": func(m map[int64]string, id *int64) string {
			if id == nil {
				return ""
			}
			return m[*id]
		},
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
	}
}

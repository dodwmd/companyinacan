//go:build browser

package browser

import (
	"strings"

	"github.com/chromedp/chromedp"
)

// createProject makes a project in the work UI, as an owner of a new company does (no API call and
// no token), and arrives on its page.
func createProject(s *session, key, name string) {
	s.t.Helper()
	s.goTo(workURL() + "/")
	s.click(`a.button[href="/ui/projects/new"]`) // "Create your first project", or "New project"
	s.fill("#p-name", name)
	s.fill("#p-key", key)
	s.fill("#p-desc", "Made by the browser suite.")
	s.submit("/ui/projects")
	if s.path() != "/ui/projects/"+key {
		s.t.Fatalf("no project after the form: %s %.400s", s.url(), s.text())
	}
	s.has("Project created", name)
}

// workManage covers specs/065-work-manager in the browser: project settings and members, a saved
// filter, the manager views, and archiving.
func workManage(s *session, r *run) {
	s.t.Helper()
	project := "/ui/projects/" + r.key
	member := "member-" + r.u + "@acme.example"

	s.goTo(workURL() + "/projects/" + r.key + "/settings")
	s.fill("#p-name", "Renamed "+r.u)
	s.submit(project + "/settings")
	s.has("Project saved")
	s.fill("#m-subject", member)
	s.submit(project + "/members")
	s.has("Member added", member)

	// An urgent issue, in progress, so that every view has something to show.
	s.goTo(workURL() + "/projects/" + r.key)
	urgent := "Urgent browser bug " + r.u
	s.fill("#c-title", urgent)
	s.fill("#c-priority", "P0")
	s.fill("#c-assignee", member)
	s.submit(project + "/issues")
	s.has("Issue created")

	// Save the filter shown, from the project page.
	s.goTo(workURL() + "/projects/" + r.key + "?priority=P0")
	s.run(chromedp.Click(`details summary`, chromedp.ByQuery))
	filter := "P0 of " + r.u
	s.fill("#sf-name", filter)
	s.run(chromedp.Click(`#sf-shared`, chromedp.ByQuery))
	s.submit("/ui/filters")
	s.has("Filter saved", filter, urgent)
	s.goTo(workURL() + "/filters")
	s.has(filter, "priority:P0 is:open")

	for page, want := range map[string]string{"workload": member, "ageing": urgent, "stale": "Stale work"} {
		s.goTo(workURL() + "/team/" + page + "?project=" + r.key)
		if !strings.Contains(s.text(), want) {
			s.t.Fatalf("%s does not show %q: %.600s", page, want, s.text())
		}
	}

	// Archive a project of its own; the run's project stays usable for the later scenarios.
	spare := r.key[:len(r.key)-1] + "Z"
	if spare == r.key {
		spare = r.key[:len(r.key)-1] + "Y"
	}
	createProject(s, spare, "Spare "+r.u)
	s.goTo(workURL() + "/projects/" + spare + "/settings")
	s.submit("/ui/projects/" + spare + "/archive")
	s.has("Project archived", "This project is archived")
	s.goTo(workURL() + "/")
	s.has("archived")
}

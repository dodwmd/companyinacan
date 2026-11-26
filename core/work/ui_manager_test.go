package work

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// renderManagerUI drives the pages of specs/065-work-manager; renderAllUI includes them, so they are
// covered by the golden files and by the static accessibility checklist.
func renderManagerUI(t *testing.T, out map[string]string) {
	t.Helper()
	h := newHarness(t, nil)
	boss := h.newToken("personal", "boss@example.com", []string{PermAdmin})
	b := newUIBrowser(t, h)
	b.mustLogin(boss)
	get := func(name, path string, want int) {
		t.Helper()
		r := b.do("GET", path, nil)
		if r.Code != want {
			t.Fatalf("%s: %d, want %d\n%.600s", name, r.Code, want, r.Body)
		}
		out[name] = normaliseUIPage(r.Body)
	}
	post := func(page, action string, form url.Values, want int) uiResp {
		t.Helper()
		r := b.post(page, action, form)
		if r.Code != want {
			t.Fatalf("POST %s: %d, want %d\n%.900s", action, r.Code, want, r.Body)
		}
		return r
	}
	get("projects-first-run", "/ui/", 200)
	get("project-new", "/ui/projects/new", 200)
	r := post("/ui/projects/new", "/ui/projects", url.Values{"key": {"1"}, "name": {""}, "description": {"kept"}}, 422)
	out["project-new-invalid"] = normaliseUIPage(r.Body)
	post("/ui/projects/new", "/ui/projects", url.Values{"key": {"web"}, "name": {"Website"}}, 303)
	for _, name := range []string{"ann@example.com", "idle@example.com"} {
		post("/ui/projects/WEB/settings", "/ui/projects/WEB/members", url.Values{"subject": {name}}, 303)
	}
	get("settings", "/ui/projects/WEB/settings", 200)
	get("workload-empty-work", "/ui/team/workload", 200)
	get("ageing-empty", "/ui/team/ageing", 200)
	get("stale-empty", "/ui/team/stale", 200)
	get("filters-empty", "/ui/filters", 200)

	h.mustDo(201, "POST", "/v1/projects/WEB/issues", boss, map[string]any{"type": "bug", "title": "Checkout is down", "priority": "P0", "assignee": "ann@example.com"})
	h.mustDo(201, "POST", "/v1/projects/WEB/issues", boss, map[string]any{"type": "task", "title": "Renew the certificate", "priority": "P1", "due_date": "2026-11-20"})
	h.mustDo(200, "POST", "/v1/issues/WEB-1/transitions", boss, map[string]any{"to": "in_progress"})
	h.clk.Advance(9 * 24 * time.Hour)
	b = newUIBrowser(t, h)
	b.mustLogin(boss)
	get("workload", "/ui/team/workload", 200)
	get("ageing", "/ui/team/ageing?project=WEB", 200)
	get("stale", "/ui/team/stale?days=3", 200)
	get("stale-bad-days", "/ui/team/stale?days=0", 400)
	post("/ui/filters", "/ui/filters", url.Values{"name": {"Urgent"}, "project": {"WEB"}, "query": {"priority:P0,P1 is:open"}, "sort": {"priority"}, "shared": {"1"}}, 303)
	get("filters", "/ui/filters", 200)
	get("filter", "/ui/filters/1", 200)
	post("/ui/projects/WEB/settings", "/ui/projects/WEB/archive", url.Values{"archived": {"1"}}, 303)
	get("settings-archived", "/ui/projects/WEB/settings", 200)
}

func TestUIFirstRunLeadsToAProject(t *testing.T) {
	h := newHarness(t, nil)
	boss := h.newToken("personal", "boss@example.com", []string{PermAdmin})
	writer := h.newToken("personal", "dev@example.com", []string{PermWrite})
	b := newUIBrowser(t, h)
	b.mustLogin(boss)
	if r := b.do("GET", "/ui/", nil); !strings.Contains(r.Body, `href="/ui/projects/new"`) || !strings.Contains(r.Body, "Create your first project") {
		t.Fatalf("an administrator is not led to a project: %s", r.Body)
	}
	w := newUIBrowser(t, h)
	w.mustLogin(writer)
	if r := w.do("GET", "/ui/", nil); strings.Contains(r.Body, "/ui/projects/new") || !strings.Contains(r.Body, "Ask an administrator") {
		t.Fatalf("a writer is offered what they cannot do: %s", r.Body)
	}
	r := b.post("/ui/projects/new", "/ui/projects", url.Values{"key": {"web"}, "name": {"Website"}, "description": {"The site"}})
	if r.Code != 303 || r.Header.Get("Location") != "/ui/projects/WEB?ok=project_created" {
		t.Fatalf("create: %d %q", r.Code, r.Header.Get("Location"))
	}
	r = b.post("/ui/projects/new", "/ui/projects", url.Values{"key": {"WEB"}, "name": {"Again"}})
	if r.Code != 409 || !strings.Contains(r.Body, "already exists") || !strings.Contains(r.Body, `value="Again"`) {
		t.Fatalf("duplicate: %d %s", r.Code, r.Body)
	}
	if r := b.post("/ui/projects/WEB/settings", "/ui/projects/WEB/settings", url.Values{"name": {"Web shop"}, "description": {"New"}}); r.Code != 303 {
		t.Fatalf("edit: %d", r.Code)
	}
	p := h.mustDo(200, "GET", "/v1/projects/WEB", bootstrap, nil)
	if p["name"] != "Web shop" || p["description"] != "New" {
		t.Fatalf("%v", p)
	}
	for _, v := range []string{"1", "0"} {
		if r := b.post("/ui/projects/WEB/settings", "/ui/projects/WEB/archive", url.Values{"archived": {v}}); r.Code != 303 {
			t.Fatalf("archive %s: %d", v, r.Code)
		}
		if got := h.mustDo(200, "GET", "/v1/projects/WEB", bootstrap, nil)["archived"]; got != (v == "1") {
			t.Fatalf("archived = %v after %s", got, v)
		}
	}
	if r := b.post("/ui/projects/WEB/settings", "/ui/projects/WEB/archive", url.Values{}); r.Code != 400 {
		t.Fatalf("archive without a value: %d", r.Code)
	}
	// Without the CSRF token nothing happens.
	if r := b.do("POST", "/ui/projects", url.Values{"key": {"EVIL"}, "name": {"x"}}); r.Code != 403 {
		t.Fatalf("no csrf: %d", r.Code)
	}
	if r := b.do("GET", "/v1/projects", nil); r.Code != 401 { // the session cookie is not an API credential
		t.Fatalf("cookie on the API: %d", r.Code)
	}
}

func TestProjectScopedAdminCannotCreateOrReachOtherProjects(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	scoped := h.newToken("personal", "lead@example.com", []string{PermAdmin}, "ACME")
	b := newUIBrowser(t, h)
	b.mustLogin(scoped)
	if r := b.do("GET", "/ui/", nil); strings.Contains(r.Body, "/ui/projects/new") {
		t.Fatal("a scoped administrator is offered a new project")
	}
	if r := b.do("GET", "/ui/projects/new", nil); r.Code != 403 {
		t.Fatalf("new: %d", r.Code)
	}
	if r := b.post("/ui/", "/ui/projects", url.Values{"key": {"NEW"}, "name": {"x"}}); r.Code != 403 {
		t.Fatalf("create: %d", r.Code)
	}
	for _, p := range []string{"/ui/projects/SEC/settings", "/ui/team/workload?project=SEC", "/ui/team/ageing?project=SEC", "/ui/team/stale?project=SEC"} {
		if r := b.do("GET", p, nil); r.Code != 404 {
			t.Errorf("%s: %d, want 404", p, r.Code)
		}
	}
	for _, a := range []string{"/ui/projects/SEC/settings", "/ui/projects/SEC/archive", "/ui/projects/SEC/members"} {
		if r := b.post("/ui/", a, url.Values{"name": {"x"}, "archived": {"1"}, "subject": {"x"}}); r.Code != 404 {
			t.Errorf("POST %s: %d, want 404", a, r.Code)
		}
	}
	h.mustDo(404, "PUT", "/v1/projects/SEC/members/ann", scoped, nil)
	h.mustDo(404, "GET", "/v1/projects/SEC/members", scoped, nil)
	h.mustDo(404, "GET", "/v1/reports/workload?project=SEC", scoped, nil)
}

func items(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, v := range m["items"].([]any) {
		out = append(out, v.(map[string]any))
	}
	return out
}

func TestReportsCountScopeAndAge(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	mk := func(project, priority, assignee string, extra map[string]any) string {
		body := map[string]any{"type": "task", "title": project + " " + priority, "priority": priority}
		if assignee != "" {
			body["assignee"] = assignee
		}
		for k, v := range extra {
			body[k] = v
		}
		return h.mustDo(201, "POST", "/v1/projects/"+project+"/issues", bootstrap, body)["key"].(string)
	}
	old := mk("ACME", "P3", "ann", nil)                            // ACME-1, raised later
	wip := mk("ACME", "P1", "ann", nil)                            // ACME-2
	mk("ACME", "P2", "", map[string]any{"due_date": "2026-11-01"}) // overdue, unassigned
	blocker := mk("SEC", "P0", "eve", nil)                         // SEC-1
	h.mustDo(201, "POST", "/v1/issues/"+blocker+"/links", bootstrap, map[string]any{"type": "blocks", "target": wip})
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "epic", "title": "container", "assignee": "ann"})
	h.mustDo(200, "POST", "/v1/issues/"+wip+"/transitions", bootstrap, map[string]any{"to": "in_progress"})
	h.mustDo(204, "PUT", "/v1/projects/ACME/members/idle", bootstrap, nil)
	h.mustDo(204, "PUT", "/v1/projects/ACME/members/idle", bootstrap, nil) // repeating is fine

	h.clk.Advance(20 * 24 * time.Hour)
	h.mustDo(200, "PATCH", "/v1/issues/"+old, bootstrap, map[string]any{"priority": "P0"})
	h.clk.Advance(24 * time.Hour)

	scoped := h.newToken("personal", "lead@example.com", []string{PermRead}, "ACME")
	rows := map[string]map[string]any{}
	for _, r := range items(t, h.mustDo(200, "GET", "/v1/reports/workload", scoped, nil)) {
		rows[r["person"].(string)] = r
	}
	if rows["eve"] != nil {
		t.Fatal("work of a project outside the scope is counted")
	}
	ann := rows["ann"]
	if ann["open"] != 2.0 || ann["p0"] != 1.0 || ann["p1"] != 1.0 || ann["in_progress"] != 1.0 || ann["blocked"] != 0.0 {
		t.Fatalf("ann (epic not counted, hidden blocker not counted): %v", ann)
	}
	if rows["idle"] == nil || rows["idle"]["open"] != 0.0 {
		t.Fatalf("an idle member is listed: %v", rows)
	}
	if rows[""]["overdue"] != 1.0 {
		t.Fatalf("unassigned: %v", rows[""])
	}
	for _, r := range items(t, h.mustDo(200, "GET", "/v1/reports/workload", bootstrap, nil)) {
		if r["person"] == "ann" && r["blocked"] != 1.0 {
			t.Fatalf("an unscoped caller sees the blocker: %v", r)
		}
	}

	ag := h.mustDo(200, "GET", "/v1/reports/ageing?project=ACME", scoped, nil)
	var got []string
	for _, it := range items(t, ag) {
		got = append(got, it["key"].(string)+" "+strconv.Itoa(int(it["priority_days"].(float64)))+"/"+strconv.Itoa(int(it["open_days"].(float64))))
	}
	// The epic is P2, so it is not here. ACME-1 has been P0 for a day although it is 21 days old.
	if strings.Join(got, ", ") != "ACME-1 1/21, ACME-2 21/21" {
		t.Fatalf("ageing: %v", got)
	}
	b, _ := json.Marshal(ag["bands"])
	if !strings.Contains(string(b), `"priority":"P1"`) || items(t, ag)[1]["past_target"] != true {
		t.Fatalf("bands %s", b)
	}

	st := h.mustDo(200, "GET", "/v1/reports/stale", scoped, nil)
	if l := items(t, st); len(l) != 1 || l[0]["key"] != wip || l[0]["quiet_days"] != 21.0 {
		t.Fatalf("stale: %v", st)
	}
	h.mustDo(201, "POST", "/v1/issues/"+wip+"/comments", bootstrap, map[string]any{"body": "still on it"})
	if st = h.mustDo(200, "GET", "/v1/reports/stale", scoped, nil); len(items(t, st)) != 0 {
		t.Fatalf("a comment is activity: %v", st)
	}
	h.mustDo(422, "GET", "/v1/reports/stale?days=-1", scoped, nil)

	// Archived projects leave the reports.
	h.mustDo(200, "PATCH", "/v1/projects/ACME", bootstrap, map[string]any{"archived": true})
	if l := items(t, h.mustDo(200, "GET", "/v1/reports/workload", scoped, nil)); len(l) != 0 {
		t.Fatalf("archived work listed: %v", l)
	}
}

func TestSavedFiltersVisibilityAndOwnership(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	h.mustDo(201, "POST", "/v1/projects/SEC/issues", bootstrap, map[string]any{"type": "bug", "title": "secret", "priority": "P0"})
	ann := h.newToken("personal", "ann", []string{PermWrite})
	ben := h.newToken("personal", "ben", []string{PermWrite})
	acme := h.newToken("personal", "cid", []string{PermWrite}, "ACME")
	reader := h.newToken("personal", "dee", []string{PermRead})
	id := func(m map[string]any) string { return strconv.Itoa(int(m["id"].(float64))) }
	private := id(h.mustDo(201, "POST", "/v1/filters", ann, map[string]any{"name": "mine", "query": "assignee:me"}))
	all := id(h.mustDo(201, "POST", "/v1/filters", ann, map[string]any{"name": "urgent", "query": "priority:P0", "shared": true}))
	sec := id(h.mustDo(201, "POST", "/v1/filters", ann, map[string]any{"name": "sec", "project": "SEC", "shared": true}))
	h.mustDo(409, "POST", "/v1/filters", ann, map[string]any{"name": "mine"})
	h.mustDo(422, "POST", "/v1/filters", ann, map[string]any{"name": "bad", "query": "nosuch:thing"})
	h.mustDo(422, "POST", "/v1/filters", ann, map[string]any{"name": " "})
	h.mustDo(404, "POST", "/v1/filters", acme, map[string]any{"name": "x", "project": "SEC"})
	h.mustDo(403, "POST", "/v1/filters", reader, map[string]any{"name": "x"})

	h.mustDo(404, "GET", "/v1/filters/"+private, ben, nil)
	h.mustDo(200, "GET", "/v1/filters/"+all, ben, nil)
	h.mustDo(404, "GET", "/v1/filters/"+all, acme, nil) // a shared filter over every project: not for a scoped token
	h.mustDo(404, "GET", "/v1/filters/"+sec, acme, nil)
	if l := items(t, h.mustDo(200, "GET", "/v1/filters", acme, nil)); len(l) != 0 {
		t.Fatalf("scoped: %v", l)
	}
	h.mustDo(403, "DELETE", "/v1/filters/"+all, ben, nil)
	h.mustDo(404, "DELETE", "/v1/filters/"+private, ben, nil)
	h.mustDo(204, "DELETE", "/v1/filters/"+all, bootstrap, nil) // an administrator may remove a shared one
	h.mustDo(204, "DELETE", "/v1/filters/"+private, ann, nil)

	b := newUIBrowser(t, h)
	b.mustLogin(ben)
	if r := b.do("GET", "/ui/filters/"+sec, nil); r.Code != 200 || !strings.Contains(r.Body, "secret") {
		t.Fatalf("run: %d", r.Code)
	}
	// "Save this filter" on a project page keeps the search that is shown.
	page := b.do("GET", "/ui/projects/ACME?priority=P0&q=login", nil)
	if !strings.Contains(page.Body, `name="query" value="login priority:P0 is:open"`) {
		t.Fatalf("save form: %s", page.Body)
	}
	r := b.post("/ui/projects/ACME", "/ui/filters", url.Values{"name": {"p0 login"}, "project": {"ACME"}, "query": {"login priority:P0 is:open"}, "sort": {"rank"}})
	if r.Code != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/ui/filters/") {
		t.Fatalf("save: %d %q", r.Code, r.Header.Get("Location"))
	}
}

func TestPriorityChangedEvent(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Webhook.URL = "http://127.0.0.1:9/never" })
	writer := h.setup()
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "bug", "title": "secret title", "priority": "P2", "assignee": "ann"})
	events := func() []string {
		rows, err := h.app.store.db.Query(`SELECT payload FROM outbox WHERE type = ? ORDER BY id`, EventPriorityChanged)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	h.mustDo(200, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"title": "other"})
	h.mustDo(200, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"priority": "P2"}) // no change
	if n := len(events()); n != 0 {
		t.Fatalf("%d events without a priority change", n)
	}
	h.mustDo(200, "POST", "/v1/issues/bulk-update", writer, map[string]any{"keys": []string{"ACME-1"}, "set": map[string]any{"priority": "P0"}})
	got := events()
	if len(got) != 1 {
		t.Fatalf("%d events", len(got))
	}
	var ev struct {
		Data priorityChangedData `json:"data"`
	}
	if err := json.Unmarshal([]byte(got[0]), &ev); err != nil {
		t.Fatal(err)
	}
	d := ev.Data
	if d.IssueKey != "ACME-1" || d.From != "P2" || d.To != "P0" || d.Version != 3 || d.Assignee == nil || *d.Assignee != "ann" || strings.Contains(got[0], "other") {
		t.Fatalf("%s", got[0])
	}
}

func TestErasureCoversMembersAndFilters(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	const who = "gone@example.com"
	theirs := h.newToken("personal", who, []string{PermWrite})
	boss := h.newToken("personal", "boss", []string{PermWrite})
	h.mustDo(204, "PUT", "/v1/projects/ACME/members/"+who, bootstrap, nil)
	h.mustDo(201, "POST", "/v1/filters", theirs, map[string]any{"name": "private"})
	h.mustDo(201, "POST", "/v1/filters", theirs, map[string]any{"name": "team", "query": "is:open", "shared": true})
	h.mustDo(201, "POST", "/v1/filters", boss, map[string]any{"name": "their work", "query": "is:open assignee:ann," + who, "shared": true})
	h.mustDo(200, "POST", "/v1/people/erase", bootstrap, map[string]any{"subject": who})
	var left int
	if err := h.app.store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM project_members WHERE subject = ?1 OR added_by = ?1)
		+ (SELECT COUNT(*) FROM saved_filters WHERE owner = ?1 OR instr(query, ?1) > 0)`, who).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d rows still name the person", left)
	}
	var names string
	if err := h.app.store.db.QueryRow(`SELECT group_concat(name) FROM saved_filters`).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if names != "team" {
		t.Fatalf("filters left: %s", names)
	}
}

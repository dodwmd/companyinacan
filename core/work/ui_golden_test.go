package work

import (
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden HTML files")

var uiIdemRe = regexp.MustCompile(`name="idem" value="[^"]+"`)

func normaliseUIPage(s string) string {
	s = uiCSRFRe.ReplaceAllString(s, `name="csrf" value="CSRF"`)
	s = uiIdemRe.ReplaceAllString(s, `name="idem" value="IDEM"`)
	s = regexp.MustCompile(`Request id: [A-Za-z0-9_-]+`).ReplaceAllString(s, "Request id: REQID")
	return s
}

// renderAllUI drives every page of the UI through the real handlers over fixed fixtures and returns
// the normalised HTML by page name.
func renderAllUI(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}

	// An empty installation.
	e := newHarness(t, nil)
	eTok := e.newToken("personal", "dev@example.com", []string{PermWrite})
	eb := newUIBrowser(t, e)
	out["login"] = normaliseUIPage(eb.do("GET", "/ui/login", nil).Body)
	if r := eb.login("cwt_0123456789abcdef_" + strings.Repeat("z", 43)); r.Code != 401 {
		t.Fatalf("login failure: %d", r.Code)
	} else {
		out["login-failed"] = normaliseUIPage(r.Body)
	}
	eb.mustLogin(eTok)
	out["projects-empty"] = normaliseUIPage(eb.do("GET", "/ui/", nil).Body)
	out["dashboard-empty"] = normaliseUIPage(eb.do("GET", "/ui/dashboard", nil).Body)
	e.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": "EMPTY", "name": "Nothing yet"})
	for name, p := range map[string]string{"project-empty": "/ui/projects/EMPTY", "board-empty": "/ui/projects/EMPTY/board", "sprints-empty": "/ui/projects/EMPTY/sprints", "epics-empty": "/ui/projects/EMPTY/epics"} {
		r := eb.do("GET", p, nil)
		if r.Code != 200 {
			t.Fatalf("%s: %d", name, r.Code)
		}
		out[name] = normaliseUIPage(r.Body)
	}

	// A populated one.
	f := newUIFixture(t)
	h := f.h
	epic := h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "epic", "title": "Onboarding", "priority": "P1"})["key"].(string)
	task := h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "task", "title": "Write the welcome email", "priority": "P2", "parent": epic, "assignee": "ann@example.com", "labels": []string{"copy", "email"}, "estimate": 3, "due_date": "2026-11-20", "description": "Draft the **welcome** email.\n\n- subject line\n- body with a [style guide](https://example.com/style)"})["key"].(string)
	done := h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "bug", "title": "Fix the signup button", "priority": "P0", "parent": epic, "assignee": "dev@example.com"})["key"].(string)
	h.mustDo(200, "POST", "/v1/issues/"+done+"/transitions", bootstrap, map[string]any{"to": "in_progress"})
	h.mustDo(200, "POST", "/v1/issues/"+done+"/transitions", bootstrap, map[string]any{"to": "done"})
	h.mustDo(201, "POST", "/v1/issues/"+task+"/comments", bootstrap, map[string]any{"body": "First draft is in **review**. See [notes](https://example.com/n).\n\n<b>not bold</b>"})
	h.mustDo(201, "POST", "/v1/issues/"+task+"/links", bootstrap, map[string]any{"type": "blocks", "target": f.acmeKey})
	h.mustDo(201, "POST", "/v1/issues/"+task+"/attachments", bootstrap, map[string]any{"filename": "draft.docx", "content_type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "size_bytes": 20480, "sha256": strings.Repeat("ab", 32), "url": "https://files.example/draft.docx"})
	h.mustDo(204, "PUT", "/v1/issues/"+task+"/watchers/ann@example.com", bootstrap, nil)
	sp := h.mustDo(201, "POST", "/v1/projects/ACME/sprints", bootstrap, map[string]any{"name": "Sprint 1", "goal": "Ship onboarding", "starts_on": "2026-11-23", "ends_on": "2026-12-04"})
	sid := strconv.Itoa(int(sp["id"].(float64)))
	h.mustDo(201, "POST", "/v1/projects/ACME/sprints", bootstrap, map[string]any{"name": "Sprint 2"})
	h.mustDo(200, "PATCH", "/v1/issues/"+task, bootstrap, map[string]any{"sprint_id": int(sp["id"].(float64))})
	rw := f.browser(t, f.rw)
	ro := f.browser(t, f.ro)
	get := func(name string, b *uiBrowser, path string, want int) {
		t.Helper()
		r := b.do("GET", path, nil)
		if r.Code != want {
			t.Fatalf("%s: %d, want %d\n%.600s", name, r.Code, want, r.Body)
		}
		out[name] = normaliseUIPage(r.Body)
	}
	get("projects", rw, "/ui/", 200)
	get("project", rw, "/ui/projects/ACME", 200)
	get("project-filtered", rw, "/ui/projects/ACME?priority=P0&scope=all&sort=-priority", 200)
	get("project-readonly", ro, "/ui/projects/ACME", 200)
	get("project-bad-query", rw, "/ui/projects/ACME?q=nosuchfield%3Ax", 400)
	get("sprints", rw, "/ui/projects/ACME/sprints", 200)
	get("epics", rw, "/ui/projects/ACME/epics", 200)
	get("board-all", rw, "/ui/projects/ACME/board?sprint=all", 200)
	get("board-no-active", rw, "/ui/projects/ACME/board", 200)
	get("board-readonly", ro, "/ui/projects/ACME/board?sprint=all", 200)
	get("issue", rw, "/ui/issues/"+task, 200)
	get("issue-readonly", ro, "/ui/issues/"+task, 200)
	get("dashboard", rw, "/ui/dashboard?assignees=ann%40example.com%2Cdev%40example.com%2Cnobody%40example.com", 200)
	get("error-not-found", rw, "/ui/issues/SEC-1", 404)
	get("error-page-not-found", rw, "/ui/nope", 404)
	r := rw.post("/ui/", "/ui/sprints/"+sid+"/start", nil)
	if r.Code != 303 {
		t.Fatalf("start sprint: %d", r.Code)
	}
	get("board-active", rw, "/ui/projects/ACME/board", 200)
	// errors from posts
	stale := rw.post("/ui/issues/"+task, "/ui/issues/"+task+"/edit", url.Values{"title": {"My edit"}, "description": {"My long description"}, "version": {"1"}})
	if stale.Code != 409 {
		t.Fatalf("stale: %d", stale.Code)
	}
	out["error-conflict-edit"] = normaliseUIPage(stale.Body)
	bad := rw.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {""}, "type": {"task"}})
	if bad.Code != 422 {
		t.Fatalf("invalid: %d", bad.Code)
	}
	out["error-invalid"] = normaliseUIPage(bad.Body)
	ill := rw.post("/ui/issues/"+f.acmeKey, "/ui/issues/"+f.acmeKey+"/transition", url.Values{"to": {"done"}, "version": {"1"}})
	if ill.Code != 409 {
		t.Fatalf("illegal: %d", ill.Code)
	}
	out["error-illegal-move"] = normaliseUIPage(ill.Body)
	ro403 := ro.post("/ui/", "/ui/issues/"+f.acmeKey+"/priority", url.Values{"priority": {"P0"}, "version": {"1"}})
	if ro403.Code != 403 {
		t.Fatalf("read-only: %d", ro403.Code)
	}
	out["error-read-only"] = normaliseUIPage(ro403.Body)
	renderManagerUI(t, out)
	return out
}

func TestUIGoldenPages(t *testing.T) {
	pages := renderAllUI(t)
	dir := filepath.Join("testdata", "ui-golden")
	if *updateGolden {
		_ = os.MkdirAll(dir, 0o755)
	}
	for name, got := range pages {
		file := filepath.Join(dir, name+".html")
		if *updateGolden {
			if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: no golden file (run go test ./core/work -run TestUIGolden -update): %v", name, err)
			continue
		}
		if strings.TrimSpace(string(want)) != strings.TrimSpace(got) {
			t.Errorf("%s differs from %s; run `go test ./core/work -run TestUIGolden -update` and review the diff", name, file)
		}
	}
	if !*updateGolden {
		files, _ := filepath.Glob(filepath.Join(dir, "*.html"))
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f), ".html")
			if _, ok := pages[name]; !ok {
				t.Errorf("stale golden file %s", f)
			}
		}
	}
}

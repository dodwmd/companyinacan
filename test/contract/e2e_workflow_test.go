package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// specs/032-e2e-docs: the end-to-end workflow is advisory. It must never become a required check,
// must not collide with a required job name, and must stay read-only (the shared hygiene tests in
// workflows_test.go already enforce SHA pins, persist-credentials: false and no script injection).
func TestE2EWorkflowIsAdvisoryAndReadOnly(t *testing.T) {
	w := workflowByFile(t, "e2e.yml")
	ci := workflowByFile(t, "ci.yml")
	if len(w.Jobs) == 0 {
		t.Fatal("e2e.yml has no jobs")
	}
	for jn, j := range w.Jobs {
		for _, cj := range ci.Jobs {
			if j.Name == cj.Name {
				t.Errorf("e2e.yml job %q collides with a required ci.yml check", j.Name)
			}
		}
		for k, v := range j.Permissions {
			if v == "write" {
				t.Errorf("e2e.yml job %s has %s: write", jn, k)
			}
		}
	}
	perms, _ := w.Permissions.(map[string]any)
	if len(perms) != 1 || perms["contents"] != "read" {
		t.Errorf("e2e.yml top-level permissions = %v, want exactly contents: read", w.Permissions)
	}
	on, _ := w.On.(map[string]any)
	for _, trig := range []string{"pull_request", "schedule", "workflow_dispatch"} {
		if _, ok := on[trig]; !ok {
			t.Errorf("e2e.yml has no %s trigger", trig)
		}
	}
	if _, bad := on["push"]; bad {
		t.Errorf("e2e.yml runs on push; it is a pull_request, nightly and manual workflow")
	}
	// Advisory: a failing e2e run must not turn the PR red, and the logs must be kept when it fails.
	if !strings.Contains(w.raw, "continue-on-error: true") {
		t.Errorf("e2e.yml jobs must be continue-on-error: true while the suite is advisory")
	}
	if !strings.Contains(w.raw, "actions/upload-artifact@") || !strings.Contains(w.raw, "if: failure()") {
		t.Errorf("e2e.yml must upload the service logs on failure")
	}
	b, err := os.ReadFile(filepath.Join(root, "scripts", "gh-protect-main.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "e2e") {
		t.Errorf("scripts/gh-protect-main.sh mentions e2e: it must not be a required check")
	}
}

// The e2e files are only compiled with the e2e build tag, so a plain `go test ./...` never needs a
// running stack. A file without the tag would make `make unit` try to reach one.
func TestE2EScenarioFilesAreBuildTagged(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(root, "test", "e2e", "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no e2e scenario files: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "//go:build e2e") {
			t.Errorf("%s lacks the //go:build e2e constraint on its first line", filepath.Base(f))
		}
	}
}

// specs/054-browser-e2e: the browser job is advisory as well, and its files only build with the
// browser tag, so no other build or test needs chromedp or a browser.
func TestBrowserJobIsAdvisoryAndSuiteIsBuildTagged(t *testing.T) {
	w := workflowByFile(t, "e2e.yml")
	j, ok := w.Jobs["browser"]
	if !ok {
		t.Fatal("e2e.yml has no browser job")
	}
	if j.ContinueOnError != true {
		t.Errorf("the browser job must be continue-on-error: true while it is advisory")
	}
	files, err := filepath.Glob(filepath.Join(root, "test", "browser", "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no browser scenario files: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "//go:build browser") {
			t.Errorf("%s lacks the //go:build browser constraint on its first line", filepath.Base(f))
		}
	}
}

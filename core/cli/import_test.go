package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

func runImport(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(append([]string{"import"}, args...), strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestImportIssuesCLI(t *testing.T) {
	srv := workServer(t)
	t.Setenv("CAN_WORK_URL", srv.URL)
	t.Setenv("CAN_WORK_TOKEN", workCLIToken)
	file := filepath.Join(t.TempDir(), "issues.csv")
	if err := os.WriteFile(file, []byte("Ref,title,colour\nJ-1,First,red\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"issues", file, "--project", "ACME", "--map", "external_id=Ref"}

	code, out, errs := runImport(t, "", args...)
	if code != 0 || !strings.Contains(out, "preview: 1 rows: 1 to create") || !strings.Contains(out, "--apply") || !strings.Contains(out, `"colour"`) {
		t.Fatalf("preview: %d %q %q", code, out, errs)
	}
	code, out, errs = runImport(t, "", append(args, "--apply")...)
	if code != 0 || !strings.Contains(out, "applied: 1 rows: 1 created") {
		t.Fatalf("apply: %d %q %q", code, out, errs)
	}
	code, out, _ = runImport(t, "", append(args, "--apply", "--output", "json")...)
	var rep csvimport.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 || rep.Unchanged != 1 {
		t.Fatalf("re-run: %d %v %q", code, err, out)
	}
	code, out, errs = runImport(t, "external_id,title\nJ-2,\n", "issues", "-", "--project", "ACME", "--apply")
	if code != ExitFailure || !strings.Contains(out, "line 2: title: required") || !strings.Contains(out, "nothing was written") || !strings.Contains(errs, "1 error") {
		t.Fatalf("bad row: %d %q %q", code, out, errs)
	}
	if code, _, errs := runImport(t, "nope\n1\n", "issues", "-", "--project", "ACME"); code != ExitFailure || !strings.Contains(errs, "400") {
		t.Fatalf("bad file: %d %q", code, errs)
	}
}

func TestImportCLIUsageAndTargets(t *testing.T) {
	var path, query, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query, auth = r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization")
		if r.URL.Path == "/v1/import/parties" {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			return
		}
		_ = json.NewEncoder(w).Encode(csvimport.Report{DryRun: true, Rows: 1, Notices: []string{"a notice"}})
	}))
	defer srv.Close()
	for _, m := range []string{"PEOPLE", "FINANCE", "WORK"} {
		t.Setenv("CAN_"+m+"_URL", srv.URL)
		t.Setenv("CAN_"+m+"_TOKEN", "token-of-"+m)
	}
	if code, _, errs := runImport(t, "a\n", "people", "-"); code != 0 || path != "/v1/import/people" || query != "" || auth != "Bearer token-of-PEOPLE" {
		t.Fatalf("people: %d %q %q %q %q", code, errs, path, query, auth)
	}
	if code, out, _ := runImport(t, "a\n", "people", "-", "--joiners"); code != 0 || query != "joiners=true" || !strings.Contains(out, "note: a notice") {
		t.Fatalf("joiners: %d %q %q", code, query, out)
	}
	// A redirect would carry the token elsewhere: it is an error, not followed.
	if code, _, _ := runImport(t, "a\n", "parties", "-", "--apply"); code != ExitFailure || path != "/v1/import/parties" || query != "apply=true" {
		t.Fatalf("parties: %d %q %q", code, path, query)
	}
	for name, args := range map[string][]string{
		"issues without project": {"issues", "-"},
		"bad map":                {"people", "-", "--map", "name"},
		"no file":                {"people"},
	} {
		if code, _, _ := runImport(t, "a\n", args...); code != ExitUsage {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _, errs := runImport(t, strings.Repeat("a", csvimport.MaxBytes+1), "people", "-"); code != ExitFailure || !strings.Contains(errs, "larger than") {
		t.Errorf("oversize: %d %q", code, errs)
	}
	t.Setenv("CAN_PEOPLE_TOKEN", "")
	if code, _, errs := runImport(t, "a\n", "people", "-"); code != ExitUsage || !strings.Contains(errs, "CAN_PEOPLE_TOKEN") {
		t.Errorf("no token: %d %q", code, errs)
	}
}

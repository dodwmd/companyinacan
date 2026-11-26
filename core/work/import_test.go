package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

// Over the ~840 appends at which ranks are respaced; MaxRows would be slow under -race.
const fullFile = 1000

// Children come before parents on purpose.
const issuesCSV = `external_id,title,type,status,priority,labels,parent_external_id,description
J-3,Write the form,subtask,,,,J-2,
J-2,Sign-up form,story,done,P1,web;ux,J-1,"line one
line two"
J-1,Onboarding,epic,,,,,
`

func importIssues(s *Store, a *Principal, project string, apply bool, in string) (*csvimport.Report, error) {
	return s.ImportIssues(context.Background(), a, project, strings.NewReader(in), csvimport.Options{Apply: apply})
}

func count(t *testing.T, s *Store, query string) (n int) {
	t.Helper()
	if err := s.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const written = `SELECT (SELECT COUNT(*) FROM issues) + (SELECT COUNT(*) FROM outbox) + (SELECT COUNT(*) FROM activity) + (SELECT COUNT(*) FROM audit_log WHERE action = 'issue.import')`

func TestImportIssuesPreviewApplyAndRerun(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	rep, err := importIssues(s, admin, "ACME", false, issuesCSV)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Create != 3 || rep.Applied || rep.Items[0].ID != "" || count(t, s, written) != 0 || count(t, s, `SELECT next_number FROM projects`) != 1 {
		t.Fatalf("preview = %+v %v", rep, rep.Errors)
	}
	rep, err = importIssues(s, admin, "ACME", true, issuesCSV)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || rep.Create != 3 {
		t.Fatalf("apply = %+v %v", rep, rep.Errors)
	}
	story, err := s.GetIssue(context.Background(), admin, rep.Items[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if story.Status != "done" || story.ResolvedAt == nil || story.Priority != "P1" || fmt.Sprint(story.Labels) != "[ux web]" || *story.Parent != rep.Items[2].ID ||
		story.Description != "line one\nline two" || fmt.Sprint(story.Children) != "["+rep.Items[0].ID+"]" {
		t.Fatalf("story = %+v", story)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM audit_log WHERE action = 'issue.import'`); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	before := count(t, s, written)
	rep, err = importIssues(s, admin, "ACME", true, issuesCSV)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unchanged != 3 || count(t, s, written) != before+1 { // only the audit entry of the run
		t.Fatalf("re-run = %+v %v", rep, rep.Errors)
	}
	// Status and type are set once: a later run never moves an issue back.
	rep, err = importIssues(s, admin, "ACME", true, "external_id,title,type,status\nJ-2,Sign-up page,bug,todo\n")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Update != 1 || fmt.Sprint(rep.Items[0].Changed) != "[title]" || len(rep.Warnings) != 2 || count(t, s, `SELECT COUNT(*) FROM issues WHERE status = 'done' AND type = 'story'`) != 1 {
		t.Fatalf("update = %+v %v", rep.Items[0], rep.Warnings)
	}
}

func TestImportIssuesAllOrNothing(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	rep, err := importIssues(s, admin, "ACME", true, `external_id,title,type,status,priority,labels,parent_external_id,estimate,due_date
A,Fine,task,,,,,,
B,,task,,,,,,
C,x,wish,,,,,,
D,x,,secret-status,,,,,
E,x,,,,Secret Label!,,,
F,x,subtask,,,,,,
G,x,task,,,,NOPE,,
H,x,,,,,,lots,
I,x,,,P9,,,,soon
`)
	if err != nil {
		t.Fatal(err)
	}
	lines := map[int]bool{}
	for _, m := range rep.Errors {
		lines[m.Line] = true
		if strings.Contains(strings.ToLower(m.Message), "secret") {
			t.Errorf("error echoes a cell: %q", m.Message)
		}
	}
	for l := 3; l <= 10; l++ {
		if !lines[l] {
			t.Errorf("line %d has no error: %v", l, rep.Errors)
		}
	}
	if rep.Applied || lines[2] || count(t, s, written) != 0 {
		t.Fatalf("report = %+v", rep.Errors)
	}
}

func TestImportIssuesNeedsAnUnscopedAdmin(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	for name, a := range map[string]*Principal{
		"reader":       {Subject: "r", Permissions: []string{PermRead}},
		"writer":       {Subject: "w", Permissions: []string{PermWrite}},
		"scoped admin": {Subject: "s", Permissions: []string{PermAdmin}, Projects: []string{"ACME"}},
		"scoped write": acmeRW,
	} {
		for _, apply := range []bool{false, true} {
			if _, err := importIssues(s, a, "ACME", apply, issuesCSV); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s apply=%v: %v", name, apply, err)
			}
		}
	}
	if _, err := importIssues(s, admin, "NONE", false, issuesCSV); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
}

func TestImportIssuesAPIAndFullFile(t *testing.T) {
	h := newHarness(t, nil)
	if r := h.do("POST", "/v1/projects", bootstrap, map[string]any{"key": "ACME", "name": "Acme"}); r.Code != 201 {
		t.Fatalf("project: %d %s", r.Code, r.Body)
	}
	post := func(tok, query, body string) (int, csvimport.Report) {
		t.Helper()
		r := h.do("POST", "/v1/projects/ACME/issues/import"+query, tok, body, "Content-Type", "text/csv")
		var rep csvimport.Report
		_ = json.Unmarshal(r.Body, &rep)
		return r.Code, rep
	}
	if code, rep := post(bootstrap, "", issuesCSV); code != 200 || !rep.DryRun || rep.Create != 3 {
		t.Fatalf("preview: %d %+v", code, rep)
	}
	if code, rep := post(bootstrap, "?apply=true", "external_id,title\nA,\n"); code != 422 || rep.Applied {
		t.Fatalf("bad row: %d", code)
	}
	if code, _ := post(bootstrap, "?apply=1", issuesCSV); code != 400 {
		t.Fatalf("bad parameter: %d", code)
	}
	if code, _ := post(bootstrap, "", strings.Repeat("a", csvimport.MaxBytes+1)); code != 413 {
		t.Fatalf("oversize: %d", code)
	}
	var big strings.Builder
	big.WriteString("external_id,title,type,parent_external_id\n")
	for i := range fullFile {
		if i%10 == 0 {
			fmt.Fprintf(&big, "P%d,Epic,epic,\n", i)
		} else {
			fmt.Fprintf(&big, "P%d,Task %d,task,P%d\n", i, i, i-i%10)
		}
	}
	if code, rep := post(bootstrap, "?apply=true", big.String()); code != 200 || rep.Create != fullFile {
		t.Fatalf("full file: %d %+v", code, rep.Errors)
	}
}

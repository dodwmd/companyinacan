package people

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

func (e *env) importCSV(a Actor, apply bool, in string) (*csvimport.Report, error) {
	e.t.Helper()
	return e.s.ImportPeople(e.ctx, a, strings.NewReader(in), csvimport.Options{Apply: apply})
}

func (e *env) count(query string) (n int) {
	e.t.Helper()
	e.must(e.s.db.QueryRowContext(e.ctx, query).Scan(&n))
	return n
}

// The boss comes last on purpose: managers are linked after every row exists.
const peopleCSV = `external_id,given_name,family_name,work_email,start_date,manager_external_id
E2,Ann,Example,ann@example.com,2025-01-06,E1
E3,Ben,Example,ben@example.com,,E1
E1,Boss,Example,boss@example.com,,
`

func TestImportPreviewWritesNothingAndApplyIsIdempotent(t *testing.T) {
	e := newEnv(t)
	before := len(e.aud.evs)
	rep, err := e.importCSV(e.hr, false, peopleCSV)
	e.must(err)
	if !rep.DryRun || rep.Applied || rep.Create != 3 || rep.HasErrors() || rep.Items[0].ID != "" {
		t.Fatalf("preview = %+v %v", rep, rep.Errors)
	}
	if n := e.count(`SELECT COUNT(*) FROM people`) + e.count(`SELECT COUNT(*) FROM outbox`) + e.count(`SELECT COUNT(*) FROM status_history`); n != 0 {
		t.Fatalf("a preview wrote %d rows", n)
	}
	if len(e.aud.evs) != before {
		t.Fatalf("a preview recorded audit entries: %+v", e.aud.evs[before:])
	}

	rep, err = e.importCSV(e.hr, true, peopleCSV)
	e.must(err)
	if !rep.Applied || rep.Create != 3 || rep.Items[0].ID == "" {
		t.Fatalf("apply = %+v %v", rep, rep.Errors)
	}
	ann, err := e.s.GetPerson(e.ctx, e.hr, rep.Items[0].ID)
	e.must(err)
	if ann.ManagerID != rep.Items[2].ID || ann.Status != StActive || ann.Kind != "employee" {
		t.Fatalf("ann = %+v", ann)
	}
	events := e.count(`SELECT COUNT(*) FROM outbox`)
	if events != 3 {
		t.Fatalf("events = %d", events)
	}
	var payloads string
	e.must(e.s.db.QueryRowContext(e.ctx, `SELECT group_concat(payload) FROM outbox`).Scan(&payloads))
	for _, pii := range []string{"Ann", "example.com", "E2"} {
		if strings.Contains(payloads, pii) {
			t.Errorf("event payload carries %q", pii)
		}
	}
	for _, ev := range e.aud.evs[before:] {
		if s := fmt.Sprintf("%+v", ev); strings.Contains(s, "Ann") || strings.Contains(s, "example.com") {
			t.Errorf("audit entry carries personal data: %s", s)
		}
	}

	rep, err = e.importCSV(e.hr, true, peopleCSV)
	e.must(err)
	if rep.Create != 0 || rep.Update != 0 || rep.Unchanged != 3 {
		t.Fatalf("re-run = %+v %v", rep, rep.Errors)
	}
	if e.count(`SELECT COUNT(*) FROM people`) != 3 || e.count(`SELECT COUNT(*) FROM outbox`) != events {
		t.Fatal("a re-run duplicated people or events")
	}

	// A change updates only what the file carries; empty cells and absent columns keep values.
	rep, err = e.importCSV(e.hr, true, "external_id,given_name,family_name,location,work_email\nE2,Ann,Sample,Perth,\n")
	e.must(err)
	if rep.Update != 1 || fmt.Sprint(rep.Items[0].Changed) != "[family_name location]" {
		t.Fatalf("update = %+v", rep.Items[0])
	}
	ann, err = e.s.GetPerson(e.ctx, e.hr, ann.ID)
	e.must(err)
	if ann.FamilyName != "Sample" || ann.Location != "Perth" || ann.WorkEmail != "ann@example.com" || ann.StartDate != "2025-01-06" || ann.ManagerID == "" {
		t.Fatalf("after update = %+v", ann)
	}
}

func TestImportIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	e.person("Hand", "") // entered by hand, no external id, hand@example.com
	before, events := e.count(`SELECT COUNT(*) FROM people`), e.count(`SELECT COUNT(*) FROM outbox`)
	in := `external_id,given_name,family_name,work_email,kind,department,start_date,manager_external_id
A,Good,One,good@example.com,,,,
B,,Two,,,,,
C,Three,Three,HAND@example.com,,,,
D,Four,Four,not-an-address,,,,
E,Five,Five,,robot,,,
F,Six,Six,,,Nowhere,,
G,Seven,Seven,,,,secret-date,
H,Eight,Eight,,,,,NOBODY
I,Nine,Nine,good@example.com,,,,
J,Ten,Ten,,,,,K
K,Eleven,Eleven,,,,,J
`
	rep, err := e.importCSV(e.hr, true, in)
	e.must(err)
	if rep.Applied || rep.Failed < 9 {
		t.Fatalf("report = %+v %v", rep, rep.Errors)
	}
	lines := map[int]bool{}
	for _, m := range rep.Errors {
		lines[m.Line] = true
		if strings.Contains(m.Message, "example.com") || strings.Contains(m.Message, "secret-date") || strings.Contains(m.Message, "Nowhere") {
			t.Errorf("error echoes a cell: %q", m.Message)
		}
	}
	for l := 3; l <= 10; l++ {
		if !lines[l] {
			t.Errorf("line %d has no error: %v", l, rep.Errors)
		}
	}
	if lines[2] {
		t.Errorf("the good row failed: %v", rep.Errors)
	}
	if !lines[12] && !lines[11] {
		t.Errorf("the reporting cycle was not reported: %v", rep.Errors)
	}
	if e.count(`SELECT COUNT(*) FROM people`) != before || e.count(`SELECT COUNT(*) FROM outbox`) != events {
		t.Fatal("a failed import wrote something")
	}
}

func TestImportReversesReportingLinesInOneFile(t *testing.T) {
	e := newEnv(t)
	_, err := e.importCSV(e.hr, true, "external_id,given_name,family_name,manager_external_id\nA,A,A,\nB,B,B,A\n")
	e.must(err)
	// A now reports to B, and B no longer to A: the final lines are acyclic.
	rep, err := e.importCSV(e.hr, true, "external_id,given_name,family_name,manager_external_id\nA,A,A,B\n")
	e.must(err)
	if !rep.HasErrors() {
		t.Fatal("B still reports to A, so A cannot report to B")
	}
	_, err = e.s.db.ExecContext(e.ctx, `UPDATE people SET manager_id = NULL`)
	e.must(err)
	rep, err = e.importCSV(e.hr, true, "external_id,given_name,family_name,manager_external_id\nA,A,A,B\nB,B,B,\n")
	e.must(err)
	if rep.HasErrors() || rep.Update != 1 {
		t.Fatalf("report = %v", rep.Errors)
	}
}

func TestImportUsesOrgNamesAndWarnsOnCreateOnlyFields(t *testing.T) {
	e := newEnv(t)
	dep, err := e.s.CreateOrgUnit(e.ctx, e.hr, "departments", OrgInput{Name: "Operations"})
	e.must(err)
	rep, err := e.s.ImportPeople(e.ctx, e.hr, strings.NewReader("Employee No,First,Last,department,kind,status,salary\n7,Ann,Example,Operations,contractor,onboarding,1\n"),
		csvimport.Options{Apply: true, Mapping: map[string]string{"external_id": "Employee No", "given_name": "First", "family_name": "Last"}})
	e.must(err)
	if rep.HasErrors() || fmt.Sprint(rep.Ignored) != "[salary]" {
		t.Fatalf("report = %+v %v", rep, rep.Errors)
	}
	p, err := e.s.GetPerson(e.ctx, e.hr, rep.Items[0].ID)
	e.must(err)
	if p.DepartmentID != dep.ID || p.Kind != "contractor" || p.Status != StOnboarding {
		t.Fatalf("person = %+v", p)
	}
	rep, err = e.importCSV(e.hr, true, "external_id,given_name,family_name,kind,status\n7,Ann,Example,employee,active\n")
	e.must(err)
	if rep.Unchanged != 1 || len(rep.Warnings) != 2 {
		t.Fatalf("report = %+v %v", rep, rep.Warnings)
	}
}

func TestImportNeverResurrectsErasedOrChangesFormerRecords(t *testing.T) {
	e := newEnv(t)
	rep, err := e.importCSV(e.hr, true, "external_id,given_name,family_name\nA,Ann,Example\nB,Ben,Example\n")
	e.must(err)
	_, err = e.s.db.ExecContext(e.ctx, `UPDATE people SET erased_at = 1, given_name = '[erased]', family_name = '' WHERE id = ?`, rep.Items[0].ID)
	e.must(err)
	_, err = e.s.db.ExecContext(e.ctx, `UPDATE people SET status = 'former' WHERE id = ?`, rep.Items[1].ID)
	e.must(err)
	rep, err = e.importCSV(e.hr, true, "external_id,given_name,family_name\nA,Ann,Example\nB,Bea,Example\n")
	e.must(err)
	if rep.Failed != 2 || e.count(`SELECT COUNT(*) FROM people`) != 2 || e.count(`SELECT COUNT(*) FROM people WHERE given_name IN ('Ann','Bea')`) != 0 {
		t.Fatalf("report = %+v %v", rep, rep.Errors)
	}
}

func TestImportNeedsHRWrite(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	_, annA := e.person("Ann", boss.ID, RoleEmployee)
	readOnly := Actor{Subject: "svc-ro", Roles: []string{RoleHR, RoleAdmin}, Restricted: true, Perms: []string{"people.read"}}
	for name, a := range map[string]Actor{"employee": annA, "manager": bossA, "read-only token": readOnly, "nobody": {}} {
		for _, apply := range []bool{false, true} {
			if _, err := e.importCSV(a, apply, peopleCSV); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s apply=%v: %v", name, apply, err)
			}
		}
	}
	writer := Actor{Subject: "svc-w", Roles: []string{RoleHR}, Restricted: true, Perms: []string{"people.read", "people.write"}}
	if _, err := e.importCSV(writer, false, peopleCSV); err != nil {
		t.Fatalf("hr writer: %v", err)
	}
}

// With the real auditor the entries commit with the change or not at all.
func TestImportAuditIsInTheTransaction(t *testing.T) {
	kr, err := ParseKeyring("k1 " + hexKeyA + "\n")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(t.TempDir(), "p.db"), Options{Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	e := &env{t: t, s: s, ctx: context.Background(), hr: Actor{Subject: "svc-hr", Roles: []string{RoleHR}}}
	_, err = e.importCSV(e.hr, false, peopleCSV)
	e.must(err)
	if n := e.count(`SELECT COUNT(*) FROM audit_log`); n != 0 {
		t.Fatalf("preview audit entries = %d", n)
	}
	_, err = e.importCSV(e.hr, true, peopleCSV)
	e.must(err)
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'create'`); n != 3 {
		t.Fatalf("create entries = %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'import' AND resource LIKE 'import:people:%'`); n != 1 {
		t.Fatalf("import entries = %d", n)
	}
	// If the audit cannot be written, nothing is.
	_, err = s.db.ExecContext(e.ctx, `CREATE TRIGGER no_import BEFORE INSERT ON audit_log WHEN NEW.action = 'import' BEGIN SELECT RAISE(ABORT, 'no'); END`)
	e.must(err)
	if _, err := e.importCSV(e.hr, true, "external_id,given_name,family_name\nZ,Zed,Example\n"); err == nil {
		t.Fatal("applied without an audit entry")
	}
	if n := e.count(`SELECT COUNT(*) FROM people`); n != 3 {
		t.Fatalf("people = %d", n)
	}
}

func TestImportRunsOneAtATimeAndTakesAFullFile(t *testing.T) {
	e := newEnv(t)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := e.s.ImportPeople(e.ctx, e.hr, pr, csvimport.Options{})
		done <- err
	}()
	_, err := pw.Write([]byte("external_id,given_name,family_name\n")) // returns once the first import is reading
	e.must(err)
	if _, err := e.importCSV(e.hr, false, peopleCSV); !errors.Is(err, csvimport.ErrBusy) {
		t.Fatalf("second import: %v", err)
	}
	_, err = pw.Write([]byte("A,Ann,Example\n"))
	e.must(err)
	e.must(pw.Close())
	e.must(<-done)

	var big strings.Builder
	big.WriteString("external_id,given_name,family_name,work_email,manager_external_id\n")
	for i := range csvimport.MaxRows {
		mgr := ""
		if i > 0 {
			mgr = fmt.Sprintf("P%d", i/8)
		}
		fmt.Fprintf(&big, "P%d,Given,Family,p%d@example.com,%s\n", i, i, mgr)
	}
	rep, err := e.importCSV(e.hr, true, big.String())
	e.must(err)
	if !rep.Applied || rep.Create != csvimport.MaxRows {
		t.Fatalf("report = %d %v", rep.Create, rep.Errors)
	}
}

func TestImportAPI(t *testing.T) {
	e := newAPIEnv(t)
	post := func(tok, query, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/import/people"+query, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "text/csv")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if code, m := post(hrTok, "", peopleCSV); code != 200 || m["dry_run"] != true || m["create"] != 3.0 {
		t.Fatalf("preview: %d %v", code, m)
	}
	if code, m := post(hrTok, "?apply=true", "external_id,given_name,family_name\nA,,x\n"); code != 422 || m["applied"] != false {
		t.Fatalf("bad row: %d %v", code, m)
	}
	if code, m := post(hrTok, "?apply=true", peopleCSV); code != 200 || m["applied"] != true {
		t.Fatalf("apply: %d %v", code, m)
	}
	if code, _ := post(hrTok, "?aply=true", peopleCSV); code != 400 {
		t.Fatalf("unknown parameter: %d", code)
	}
	if code, _ := post(hrTok, "", "nothing\n"); code != 400 {
		t.Fatalf("bad file: %d", code)
	}
	if code, _ := post(hrTok, "", strings.Repeat("a", csvimport.MaxBytes+1)); code != 413 {
		t.Fatalf("oversize: %d", code)
	}
	if code, _ := post("nope", "", peopleCSV); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	_, ann := e.person("Zoe", "", RoleEmployee)
	if code, _ := post(e.token("zoe", ann), "?apply=true", peopleCSV); code != 403 {
		t.Fatalf("employee: %d", code)
	}
}

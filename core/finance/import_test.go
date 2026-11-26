package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/csvimport"
)

const partiesCSV = `external_id,name,kind,email,currency,notes
C-1,Acme Pty,customer,ap@acme.example,AUD,"=HYPERLINK(""http://x"")"
V-1,Bolt Supplies,vendor,,,
`

var importer = &Principal{Subject: "importer", Permissions: []string{PermWrite}}

const fullFile = 500 // under -race the pure-Go SQLite is about forty times slower

func (e *env) importParties(p *Principal, apply bool, in string) (*csvimport.Report, error) {
	e.t.Helper()
	return e.app.store.ImportParties(context.Background(), p, strings.NewReader(in), csvimport.Options{Apply: apply})
}

func (e *env) count(query string) (n int) {
	e.t.Helper()
	if err := e.app.store.db.QueryRow(query).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestImportPartiesPreviewApplyAndRerun(t *testing.T) {
	e := newEnv(t)
	rep, err := e.importParties(importer, false, partiesCSV)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Create != 2 || rep.Applied || rep.Items[0].ID != "" || e.count(`SELECT COUNT(*) FROM parties`)+e.count(`SELECT COUNT(*) FROM audit_log`) != 0 {
		t.Fatalf("preview = %+v", rep)
	}
	rep, err = e.importParties(importer, true, partiesCSV)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || rep.Create != 2 || rep.Items[0].ID == "" {
		t.Fatalf("apply = %+v %v", rep, rep.Errors)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'party.import'`); n != 3 { // two records and the file
		t.Fatalf("audit = %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE detail LIKE '%Acme%' OR detail LIKE '%acme.example%'`) + e.count(`SELECT COUNT(*) FROM outbox`); n != 0 {
		t.Fatalf("names in the audit log, or events published: %d", n)
	}
	if e.count(`SELECT COUNT(*) FROM parties WHERE notes LIKE '=HYPERLINK%'`) != 1 {
		t.Fatal("values are stored as written")
	}
	rep, err = e.importParties(importer, true, partiesCSV)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unchanged != 2 || e.count(`SELECT COUNT(*) FROM parties`) != 2 || e.count(`SELECT SUM(version) FROM parties`) != 2 {
		t.Fatalf("re-run = %+v", rep)
	}
	rep, err = e.importParties(importer, true, "external_id,name,phone,email\nC-1,Acme Pty,555 0100,\n")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Update != 1 || fmt.Sprint(rep.Items[0].Changed) != "[phone]" || e.count(`SELECT COUNT(*) FROM parties WHERE phone = '555 0100' AND email = 'ap@acme.example' AND version = 2`) != 1 {
		t.Fatalf("update = %+v", rep.Items[0])
	}
}

func TestImportPartiesAllOrNothingAndErased(t *testing.T) {
	e := newEnv(t)
	_, err := e.importParties(importer, true, partiesCSV)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := e.importParties(importer, true, "external_id,name,kind,email,currency\nN-1,Fine,,,\nN-2,,,,\nN-3,x,robot,secret-not-an-address,ZZZ\n")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || rep.Failed != 2 || rep.ErrorCount < 4 || e.count(`SELECT COUNT(*) FROM parties`) != 2 {
		t.Fatalf("report = %+v %v", rep, rep.Errors)
	}
	for _, m := range rep.Errors {
		if strings.Contains(m.Message, "secret") {
			t.Errorf("error echoes a cell: %q", m.Message)
		}
	}
	if _, err := e.app.store.db.Exec(`UPDATE parties SET erased_at = 1, name = 'Erased party' WHERE external_id = 'V-1'`); err != nil {
		t.Fatal(err)
	}
	rep, err = e.importParties(importer, true, partiesCSV)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 || e.count(`SELECT COUNT(*) FROM parties`) != 2 {
		t.Fatalf("erased: %+v %v", rep, rep.Errors)
	}
	rep, err = e.importParties(importer, false, "external_id,name\nOTHER,acme pty\n")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Create != 1 || len(rep.Warnings) != 1 {
		t.Fatalf("same name: %+v", rep.Warnings)
	}
}

func TestImportPartiesNeedsAnUnscopedWriter(t *testing.T) {
	e := newEnv(t)
	w := []string{PermWrite}
	for name, p := range map[string]*Principal{
		"reader":         {Subject: "r", Permissions: []string{PermRead}},
		"approver":       {Subject: "a", Permissions: []string{PermApprove}},
		"party scoped":   {Subject: "p", Permissions: w, Parties: []int64{1}},
		"project scoped": {Subject: "j", Permissions: w, Projects: []string{"OPS"}},
		"own only":       {Subject: "o", Permissions: w, OwnOnly: true},
		"scoped admin":   {Subject: "s", Permissions: []string{PermAdmin}, Parties: []int64{1}},
	} {
		for _, apply := range []bool{false, true} {
			if _, err := e.importParties(p, apply, partiesCSV); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s apply=%v: %v", name, apply, err)
			}
		}
	}
	if _, err := e.importParties(&Principal{Subject: "admin", Permissions: []string{PermAdmin}}, false, partiesCSV); err != nil {
		t.Fatal(err)
	}
}

func TestImportPartiesAPIAndFullFile(t *testing.T) {
	e := newEnv(t)
	post := func(tok, query, body string) (int, csvimport.Report) {
		t.Helper()
		code, _, raw := e.req(tok, "POST", "/v1/import/parties"+query, body, "Content-Type", "text/csv")
		var rep csvimport.Report
		_ = json.Unmarshal(raw, &rep)
		return code, rep
	}
	if code, rep := post(adminTok, "", partiesCSV); code != 200 || !rep.DryRun || rep.Create != 2 {
		t.Fatalf("preview: %d %+v", code, rep)
	}
	if code, rep := post(adminTok, "?apply=true", "external_id,name\nA,\n"); code != 422 || rep.Applied {
		t.Fatalf("bad row: %d %+v", code, rep)
	}
	if code, _ := post(adminTok, "?nope=1", partiesCSV); code != 400 {
		t.Fatalf("unknown parameter: %d", code)
	}
	if code, _ := post(adminTok, "", strings.Repeat("a", csvimport.MaxBytes+1)); code != 413 {
		t.Fatalf("oversize: %d", code)
	}
	if code, _ := post("nope", "", partiesCSV); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	var big strings.Builder
	big.WriteString("external_id,name,address\n")
	for i := range fullFile {
		fmt.Fprintf(&big, "C%d,Company %d,1 Example Street\n", i, i)
	}
	if code, rep := post(adminTok, "?apply=true", big.String()); code != 200 || rep.Create != fullFile {
		t.Fatalf("full file: %d %+v", code, rep.Errors)
	}
}

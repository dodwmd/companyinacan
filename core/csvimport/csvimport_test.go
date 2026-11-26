package csvimport

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

var fields = []Field{{Name: ExternalID, Required: true}, {Name: "name", Required: true}, {Name: "work_email"}}

func read(t *testing.T, in string, mapping map[string]string) *File {
	t.Helper()
	f, err := Read(strings.NewReader(in), fields, mapping)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReadMapsHeaders(t *testing.T) {
	f := read(t, "\ufeffExternal ID,Full Name,Work-Email,salary\r\n E1 ,\"Ann, A\",,100\r\n\r\nE2,Bo,bo@example.com,2\n", map[string]string{"name": "Full Name"})
	if len(f.Rows) != 2 {
		t.Fatalf("rows = %d", len(f.Rows))
	}
	r := f.Rows[0]
	if r.Line != 2 || r.Get(ExternalID) != "E1" || r.Get("name") != "Ann, A" || r.Get("work_email") != "" || r.Err != "" {
		t.Fatalf("row = %+v", r)
	}
	if f.Rows[1].Line != 4 {
		t.Fatalf("line = %d, want 4 (blank lines still count)", f.Rows[1].Line)
	}
	if fmt.Sprint(f.Ignored) != "[salary]" || f.Columns["name"] != "Full Name" || f.Columns["work_email"] != "Work-Email" {
		t.Fatalf("ignored %v columns %v", f.Ignored, f.Columns)
	}
	if len(f.SHA256) != 64 || !f.Has("work_email") {
		t.Fatalf("sha %q", f.SHA256)
	}
}

func TestReadRefusesBadFiles(t *testing.T) {
	many := "external_id,name\n" + strings.Repeat("a,b\n", MaxRows+1)
	for name, c := range map[string]struct {
		in      string
		mapping map[string]string
		tooBig  bool
	}{
		"empty":             {in: ""},
		"header only":       {in: "external_id,name\n"},
		"missing column":    {in: "external_id\nE1\n"},
		"duplicate header":  {in: "external_id,name,Name\nE1,a,b\n"},
		"unknown field":     {in: "external_id,name\nE1,a\n", mapping: map[string]string{"salary": "name"}},
		"unknown header":    {in: "external_id,name\nE1,a\n", mapping: map[string]string{"name": "nope"}},
		"header used twice": {in: "external_id,name\nE1,a\n", mapping: map[string]string{"work_email": "name"}},
		"bad quote":         {in: "external_id,name\nE1,\"a\n"},
		"not utf-8":         {in: "external_id,name\nE1,\xff\n"},
		"nul":               {in: "external_id,name\nE1,a\x00b\n"},
		"columns":           {in: "external_id,name" + strings.Repeat(",x", MaxColumns) + "\n"},
		"rows":              {in: many, tooBig: true},
		"bytes":             {in: "external_id,name\nE1," + strings.Repeat("a", MaxBytes) + "\n", tooBig: true},
	} {
		_, err := Read(strings.NewReader(c.in), fields, c.mapping)
		var fe *FileError
		if !errors.As(err, &fe) {
			t.Errorf("%s: err = %v, want a FileError", name, err)
			continue
		}
		if fe.TooLarge != c.tooBig {
			t.Errorf("%s: too large = %v (%v)", name, fe.TooLarge, err)
		}
	}
}

func TestExternalIDsAndCellCounts(t *testing.T) {
	f := read(t, "external_id,name\n=1+1,a\nann@example.com,b\n,c\nE1,d\nE1,e\nE2\nHR/001#a.b:c-d_e,f\n"+strings.Repeat("a", 65)+",g\n", nil)
	var bad []int
	for _, r := range f.Rows {
		if r.Err != "" {
			bad = append(bad, r.Line)
		}
	}
	if fmt.Sprint(bad) != "[2 3 4 6 7 9]" {
		t.Fatalf("bad rows = %v", bad)
	}
	for _, r := range f.Rows {
		if strings.Contains(r.Err, "example.com") || strings.Contains(r.Err, "1+1") {
			t.Errorf("error echoes the cell: %q", r.Err)
		}
	}
}

func TestParseQuery(t *testing.T) {
	o, err := ParseQuery(url.Values{})
	if err != nil || o.Apply || len(o.Mapping) != 0 {
		t.Fatalf("default = %+v %v", o, err)
	}
	o, err = ParseQuery(url.Values{"apply": {"true"}, "map": {"name=Full = Name", "work_email=E"}})
	if err != nil || !o.Apply || o.Mapping["name"] != "Full = Name" || o.Mapping["work_email"] != "E" {
		t.Fatalf("got %+v %v", o, err)
	}
	for _, v := range []url.Values{{"aply": {"true"}}, {"apply": {"yes"}}, {"map": {"name"}}, {"map": {"name=a", "name=b"}}, {"apply": {"true", "false"}}} {
		if _, err := ParseQuery(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
	if got := (Options{Apply: true, Mapping: map[string]string{"name": "A&B"}}).Query(); got != "apply=true&map=name%3DA%26B" {
		t.Errorf("query = %q", got)
	}
}

func TestReportCapsListsNotCounts(t *testing.T) {
	var in strings.Builder
	in.WriteString("external_id,name\n")
	for i := range MaxListed + 10 {
		fmt.Fprintf(&in, "E%d,\n", i)
	}
	f := read(t, in.String(), nil)
	rep := NewReport(f, Options{})
	for _, r := range f.Rows {
		it := rep.Row(r)
		it.Error("name", "required")
		it.Error("name", "again")
		it.Warn("name", "w")
	}
	rep.Finish()
	if rep.Failed != MaxListed+10 || len(rep.Errors) != MaxListed || len(rep.Warnings) != MaxListed || rep.ErrorCount != 2*(MaxListed+10) || !rep.HasErrors() {
		t.Fatalf("report = failed %d listed %d count %d", rep.Failed, len(rep.Errors), rep.ErrorCount)
	}
	if rep.Applied || rep.Items[0].Action != ActionError {
		t.Fatalf("a preview is never applied: %+v", rep.Items[0])
	}
}

func TestReportCounts(t *testing.T) {
	f := read(t, "external_id,name\nA,a\nB,b\nC,c\n", nil)
	rep := NewReport(f, Options{Apply: true})
	rep.Row(f.Rows[0]).Create("1")
	rep.Row(f.Rows[1]).Update("2", []string{"name"})
	rep.Row(f.Rows[2]).Update("3", nil)
	rep.Finish()
	if !rep.Applied || rep.Create != 1 || rep.Update != 1 || rep.Unchanged != 1 || rep.Rows != 3 || rep.Items[0].ID != "1" {
		t.Fatalf("report = %+v", rep)
	}
}

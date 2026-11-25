package forms

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestCSVInjectionNeutralised(t *testing.T) {
	for _, in := range []string{"=1+1", "+1", "-1", "@SUM(A1)", "\tfoo", "\rfoo", "=cmd|' /C calc'!A0"} {
		out := SanitizeCSVCell(in)
		if !strings.HasPrefix(out, "'") {
			t.Errorf("%q not neutralised: %q", in, out)
		}
	}
	for _, in := range []string{"", "hello", "a=b", "1 + 1", "o'brien"} {
		if SanitizeCSVCell(in) != in {
			t.Errorf("%q must be unchanged", in)
		}
	}
}

func TestWriteCSV(t *testing.T) {
	ts := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	leads := []Lead{
		{ID: "ld_1", FormID: "contact", CreatedAt: ts, Status: "new", Email: "a@example.com", Name: "=HYPERLINK(\"x\")", Fields: map[string]string{"message": "hi, \"there\"\nline2", "topic": "sales"}},
		{ID: "ld_2", FormID: "contact", CreatedAt: ts, Status: "new", Fields: map[string]string{"company": "@evil"}},
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, leads); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	head := strings.Join(rows[0], ",")
	if head != "id,form_id,created_at,status,email,name,note,company,message,topic" {
		t.Fatalf("header: %s", head)
	}
	if rows[1][5] != "'=HYPERLINK(\"x\")" {
		t.Fatalf("name not neutralised: %q", rows[1][5])
	}
	if rows[1][8] != "hi, \"there\"\nline2" || rows[1][2] != "2026-11-25T01:02:03Z" {
		t.Fatalf("row: %q", rows[1])
	}
	if rows[2][7] != "'@evil" {
		t.Fatalf("field not neutralised: %q", rows[2][7])
	}
}

func TestWriteCSVHeaderInjection(t *testing.T) {
	// A field name comes from operator config, but defend the header anyway.
	var buf bytes.Buffer
	if err := WriteCSV(&buf, []Lead{{ID: "x", Fields: map[string]string{"=x": "1"}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "'=x") {
		t.Fatalf("header not neutralised: %s", buf.String())
	}
}

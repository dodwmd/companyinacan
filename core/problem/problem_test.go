package problem

import (
	"encoding/json"
	"testing"
)

func TestString(t *testing.T) {
	tests := []struct {
		name string
		p    Problem
		want string
	}{
		{"with position", Problem{File: "a.yaml", Line: 3, Column: 5, Path: "/x", Rule: "schema.required", Severity: Error, Message: "missing"}, "a.yaml:3:5: /x: missing [schema.required]"},
		{"without position", Problem{File: "a.yaml", Path: "/x", Rule: "r", Severity: Error, Message: "m"}, "a.yaml: /x: m [r]"},
		{"without path", Problem{File: "a.yaml", Line: 1, Column: 1, Rule: "parse.yaml", Severity: Error, Message: "bad"}, "a.yaml:1:1: bad [parse.yaml]"},
		{"warning prefix", Problem{File: "a.yaml", Path: "/x", Rule: "r", Severity: Warning, Message: "m"}, "a.yaml: /x: warning: m [r]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.String(); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestJSONShape(t *testing.T) {
	b, err := json.Marshal(Problem{File: "f", Line: 2, Column: 1, Path: "/p", Rule: "r", Severity: Error, Message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"file":"f","line":2,"column":1,"path":"/p","rule":"r","severity":"error","message":"m"}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}

func TestHasErrorsAndSort(t *testing.T) {
	ps := []Problem{
		{File: "b", Path: "/a", Rule: "r", Severity: Warning},
		{File: "a", Line: 2, Path: "/z", Rule: "r", Severity: Error},
		{File: "a", Line: 1, Path: "/y", Rule: "r", Severity: Error},
	}
	if !HasErrors(ps) {
		t.Fatal("expected errors")
	}
	if HasErrors(ps[:1]) {
		t.Fatal("warnings must not count as errors")
	}
	Sort(ps)
	if ps[0].File != "a" || ps[0].Line != 1 || ps[2].File != "b" {
		t.Fatalf("unexpected order: %+v", ps)
	}
}

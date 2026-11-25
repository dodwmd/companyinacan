package config

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/problem"
)

func TestLoadBytesPositions(t *testing.T) {
	src := "schema: 1\nname: Acme\nlist:\n  - a\n  - b: 2\nnested:\n  key: value\n"
	doc, ps := LoadBytes("f.yaml", []byte(src))
	if len(ps) != 0 {
		t.Fatalf("unexpected problems: %v", ps)
	}
	cases := map[string][2]int{
		"/name":        {2, 7},
		"/nested/key":  {7, 8},
		"/list/1/b":    {5, 8},
		"/list/1":      {5, 5},
		"/list/0":      {4, 5},
		"":             {1, 1},
		"/missing/key": {1, 1}, // falls back to the nearest existing ancestor
	}
	for path, want := range cases {
		l, c := doc.Pos(path)
		if l != want[0] || c != want[1] {
			t.Errorf("Pos(%q) = %d:%d, want %d:%d", path, l, c, want[0], want[1])
		}
	}
}

func TestLoadBytesErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"empty", ""},
		{"only comment", "# nothing\n"},
		{"invalid yaml", "a: [1, 2\nb: : :\n"},
		{"tab indent", "a:\n\tb: 1\n"},
		{"non-string key", "1: a\n"},
		{"multiple documents", "a: 1\n---\nb: 2\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, ps := LoadBytes("f.yaml", []byte(tt.src))
			if doc != nil {
				t.Fatal("expected nil doc on error")
			}
			if len(ps) != 1 || ps[0].Rule != "parse.yaml" || ps[0].Severity != problem.Error || ps[0].File != "f.yaml" {
				t.Fatalf("unexpected problems: %+v", ps)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, ps := Load("/nonexistent/company.yaml")
	if len(ps) != 1 || ps[0].Rule != "parse.read" {
		t.Fatalf("unexpected problems: %+v", ps)
	}
}

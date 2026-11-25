package runtime

import "testing"

func TestParseModuleUIURLs(t *testing.T) {
	got := parseModuleUIURLs("work=https://work.example.org/ui, people = https://people.example.org ,bad,=x,Evil=https://x,work2=")
	if len(got) != 2 || got["work"] != "https://work.example.org/ui" || got["people"] != "https://people.example.org" {
		t.Fatalf("%v", got)
	}
	if len(parseModuleUIURLs("")) != 0 {
		t.Fatal("empty input")
	}
}

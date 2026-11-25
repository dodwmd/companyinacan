package runtime

import (
	"strings"
	"testing"
)

func TestParseModuleUIURLs(t *testing.T) {
	got := parseModuleUIURLs("work=https://work.example.org/ui, people = https://people.example.org ,bad,=x,Evil=https://x,work2=")
	if len(got) != 2 || got["work"] != "https://work.example.org/ui" || got["people"] != "https://people.example.org" {
		t.Fatalf("%v", got)
	}
	if len(parseModuleUIURLs("")) != 0 {
		t.Fatal("empty input")
	}
}

func TestModuleUIWarnings(t *testing.T) {
	if w := moduleUIWarnings("work=https://work.example.org/ui,people=https://people.example.org/", "example.org", false); len(w) != 0 {
		t.Fatalf("a good configuration warns: %v", w)
	}
	all := func(raw, domain string, allowHTTP bool) string {
		return strings.Join(moduleUIWarnings(raw, domain, allowHTTP), "\n")
	}
	if w := all("", "", false); !strings.Contains(w, "CAN_COMPANY_DOMAIN") {
		t.Fatalf("no warning for a missing domain: %q", w)
	}
	w := all("work=http://work.example.org,jobs=https://user:hunter2@jobs.example.org,finance=https://evil.example,nonsense", "example.org", false)
	for _, want := range []string{"work", "jobs", "finance", "entry 4"} {
		if !strings.Contains(w, want) {
			t.Errorf("no warning mentioning %q in %q", want, w)
		}
	}
	if strings.Contains(w, "hunter2") || strings.Contains(w, "evil.example") {
		t.Fatalf("the warning repeats the configured value: %q", w)
	}
	if w := all("", "localhost", true); !strings.Contains(w, "cookie") {
		t.Fatalf("no warning for a single-label domain: %q", w)
	}
}

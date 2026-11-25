package runtime

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
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

func TestUnknownModuleIDsInUIURLsAreWarnedAbout(t *testing.T) {
	overrides := parseModuleUIURLs("work=https://work.example.org,wrok=https://work.example.org,poeple=https://user:secret@people.example.org")
	w := unknownModuleUIWarnings(overrides, []store.Module{{ID: "work"}})
	if len(w) != 2 || !strings.Contains(w[0], `"poeple"`) || !strings.Contains(w[1], `"wrok"`) {
		t.Fatalf("%q", w)
	}
	if strings.Contains(strings.Join(w, " "), "secret") || strings.Contains(strings.Join(w, " "), "https") {
		t.Fatalf("a configured address was repeated: %q", w)
	}
}

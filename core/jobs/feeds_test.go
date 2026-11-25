package jobs

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func at(s string) *time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return &t
}

func testSite() SiteInfo {
	return SiteInfo{Company: "Example Co", BaseURL: "https://example.test", CareersPath: "/careers", LogoURL: "https://example.test/logo.png"}
}

// fixtureJobs are fake data covering hybrid and remote, with and without published pay.
func fixtureJobs() []*Job {
	opened := at("2026-11-01")
	return []*Job{
		{
			ID: "job_0000000000000001", Slug: "senior-engineer-0001", Title: "Senior Engineer", Department: "Engineering",
			Location: Location{City: "Austin", Region: "TX", Country: "US", PostalCode: "78701"}, Remote: "hybrid",
			EmploymentType: "full_time", Salary: &Salary{Min: 150000, Max: 190000, Currency: "USD", Period: "year"}, PublishSalary: true,
			Sections: []Section{{Heading: "About the role", Body: "Build things.\n\nShip them <b>safely</b> & often."}, {Heading: "Requirements", Body: "Go."}},
			Status:   StatusOpen, ValidThrough: at("2027-01-15"), OpenedAt: opened, UpdatedAt: *opened, Headcount: 1,
		},
		{
			ID: "job_0000000000000002", Slug: "support-agent-0002", Title: "Support Agent", Department: "Support",
			Location: Location{Country: "DE"}, Remote: "remote", ApplicantCountries: []string{"DE", "AT"},
			EmploymentType: "part_time", Salary: &Salary{Min: 20, Max: 25, Currency: "EUR", Period: "hour"}, PublishSalary: false,
			Sections: []Section{{Heading: "Role", Body: "Help customers."}},
			Status:   StatusOpen, ValidThrough: at("2027-02-01"), OpenedAt: at("2026-11-10"), UpdatedAt: *at("2026-11-12"), Headcount: 2,
		},
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test -update to create golden files)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

var feedNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func TestGoldenGenericFeed(t *testing.T) {
	b, err := GenericFeed(testSite(), fixtureJobs(), feedNow)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "generic_feed.golden.xml", b)
	if err := xml.Unmarshal(b, new(struct{})); err != nil {
		t.Errorf("not well-formed XML: %v", err)
	}
}

func TestGoldenIndeedFeed(t *testing.T) {
	b, err := IndeedFeed(testSite(), fixtureJobs(), feedNow)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "indeed_feed.golden.xml", b)
	if !strings.Contains(string(b), "150000") {
		t.Error("published salary missing")
	}
	if strings.Contains(string(b), "EUR") {
		t.Error("an unpublished salary leaked into the feed")
	}
}

func TestGoldenSitemap(t *testing.T) {
	b, err := Sitemap(testSite(), fixtureJobs())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "sitemap.golden.xml", b)
}

func TestGoldenJobPostingJSONLD(t *testing.T) {
	js := fixtureJobs()
	b1, err := JobPostingJSONLD(testSite(), js[0])
	if err != nil {
		t.Fatal(err)
	}
	b2, err := JobPostingJSONLD(testSite(), js[1])
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "jobposting_hybrid.golden.json", b1)
	golden(t, "jobposting_remote.golden.json", b2)
}

func TestJSONLDRequiredProperties(t *testing.T) {
	b, err := JobPostingJSONLD(testSite(), fixtureJobs()[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"@context", "@type", "title", "description", "datePosted", "validThrough", "hiringOrganization", "jobLocation", "baseSalary", "employmentType", "identifier"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JobPosting lacks %s", k)
		}
	}
	if m["@type"] != "JobPosting" || m["validThrough"] != "2027-01-15" || m["datePosted"] != "2026-11-01" {
		t.Errorf("dates/type: %v %v %v", m["@type"], m["validThrough"], m["datePosted"])
	}
	if m["jobLocationType"] != nil {
		t.Error("a hybrid job is not TELECOMMUTE")
	}
	b, _ = JobPostingJSONLD(testSite(), fixtureJobs()[1])
	m = map[string]any{}
	_ = json.Unmarshal(b, &m)
	if m["jobLocationType"] != "TELECOMMUTE" {
		t.Errorf("remote job lacks TELECOMMUTE: %v", m["jobLocationType"])
	}
	if reqs, _ := m["applicantLocationRequirements"].([]any); len(reqs) != 2 {
		t.Errorf("applicantLocationRequirements = %v", m["applicantLocationRequirements"])
	}
	if _, ok := m["baseSalary"]; ok {
		t.Error("baseSalary emitted although publish_salary is false")
	}
}

func TestJSONLDCannotBreakOutOfScript(t *testing.T) {
	j := fixtureJobs()[0]
	j.Title = `</script><script>alert(1)</script>`
	j.Sections = []Section{{Heading: "x", Body: `</script><img src=x onerror=alert(1)>`}}
	b, err := JobPostingJSONLD(testSite(), j)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(b), []byte("</script")) || bytes.Contains(b, []byte("<img")) {
		t.Errorf("JSON-LD is not safe to inline in a script element: %s", b)
	}
}

func TestFeedsOmitNonOpenAndEscape(t *testing.T) {
	js := fixtureJobs()
	js[1].Status = StatusClosed
	js[0].Title = "A & B <c>\x01"
	for name, fn := range map[string]func(SiteInfo, []*Job, time.Time) ([]byte, error){"generic": GenericFeed, "indeed": IndeedFeed} {
		b, err := fn(testSite(), js, feedNow)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "Support Agent") {
			t.Errorf("%s feed lists a closed job", name)
		}
		if err := xml.Unmarshal(b, new(struct{})); err != nil {
			t.Errorf("%s feed is not well-formed with hostile text: %v", name, err)
		}
		if strings.Contains(string(b), "<c>") {
			t.Errorf("%s feed did not escape markup in the title", name)
		}
	}
}

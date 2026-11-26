package provider

// Records for module hostnames (specs/045-public-access), against the fake Cloudflare API: a name
// someone else uses is never changed, adopted or added to, and only what can created is deleted.

import (
	"context"
	"strings"
	"testing"
)

func moduleRec(name, ip string) Record {
	return Record{Type: "A", Name: name, Value: ip, TTL: DefaultTTL, Purpose: "work ui", NoAdopt: true}
}

func TestModuleRecordsNeverTouchSomeoneElsesName(t *testing.T) {
	f := newFakeCF(t)
	f.records = []*cfRecord{
		{ID: "f1", Type: "A", Name: "work.acme.example", Content: "198.51.100.9", TTL: 300},
		{ID: "f2", Type: "CNAME", Name: "careers.acme.example", Content: "jobs.vendor.test", TTL: 300},
		{ID: "f3", Type: "AAAA", Name: "people.acme.example", Content: "2001:db8::9", TTL: 300, Comment: "mine"},
	}
	// even adopt_existing, which is for the company's own names, does not reach module names
	x := cfFixture(t, f, DNSConfig{AdoptExisting: true})
	var out strings.Builder
	recs := []Record{moduleRec("work.acme.example", "203.0.113.7"), moduleRec("careers.acme.example", "203.0.113.7"),
		moduleRec("people.acme.example", "203.0.113.7"), moduleRec("jobs.acme.example", "203.0.113.7")}
	if err := x.p.Apply(context.Background(), "acme.example", recs, &out); err != nil {
		t.Fatal(err)
	}
	if f.count("PUT ") != 0 || f.count("DELETE ") != 0 || f.count("POST ") != 1 {
		t.Fatalf("only the free name may be written: %v", f.calls)
	}
	for _, want := range []string{"skipped A work.acme.example", "skipped A careers.acme.example", "skipped A people.acme.example", "created A jobs.acme.example"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := x.p.Remove(context.Background(), "acme.example", &out); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 3 {
		t.Fatalf("can down must delete exactly what can created: %d records left", len(f.records))
	}
}

func TestModuleRecordsFollowTheServerButNotOverAnEdit(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{})
	var out strings.Builder
	apply := func(ip string) {
		t.Helper()
		out.Reset()
		if err := x.p.Apply(context.Background(), "acme.example", []Record{moduleRec("work.acme.example", ip)}, &out); err != nil {
			t.Fatal(err)
		}
	}
	apply("203.0.113.7")
	apply("203.0.113.8") // the server moved
	if f.records[0].Content != "203.0.113.8" {
		t.Fatalf("a record can created follows the server: %+v", f.records[0])
	}
	f.records[0].Content = "198.51.100.1" // the operator points it elsewhere, keeping the comment
	apply("203.0.113.8")
	if f.records[0].Content != "198.51.100.1" || !strings.Contains(out.String(), "skipped") {
		t.Fatalf("an operator's edit was overwritten: %+v %s", f.records[0], out.String())
	}
}

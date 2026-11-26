package routing

import (
	"reflect"
	"strings"
	"testing"
)

func TestMayClaim(t *testing.T) {
	for _, c := range []struct {
		id, label string
		want      bool
	}{
		{"work", "work", true}, {"jobs", "jobs", true}, {"jobs", "careers", true},
		{"work", "admin", false}, {"admin", "admin", false}, {"www", "www", false},
		{"work", "finance", false}, {"work", "careers", false}, {"careers", "careers", false}, {"work", "", false},
	} {
		if got := MayClaim(c.id, c.label); got != c.want {
			t.Errorf("MayClaim(%q, %q) = %v", c.id, c.label, got)
		}
	}
}

func TestCheckUIPaths(t *testing.T) {
	if err := CheckUIPaths([]string{"/", "/manifest.webmanifest"}, []string{"/ui", "/people"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/v1", "/healthz", "/public", "/metrics", "/a/b", "/UI", "/x`) || PathPrefix(`/"} {
		if CheckUIPaths(nil, []string{p}) == nil {
			t.Errorf("prefix %q accepted", p)
		}
	}
	for _, p := range []string{"/v1", "/readyz", "ui", "/../v1"} {
		if CheckUIPaths([]string{p}, nil) == nil {
			t.Errorf("exact path %q accepted", p)
		}
	}
	if CheckUIPaths(nil, nil) == nil {
		t.Error("empty list accepted")
	}
}

func TestConfigRoundTripAndRefusals(t *testing.T) {
	c := Config{Domain: "acme.example", TLS: true, SiteHosts: []string{"acme.example", "1-2-3-4.sslip.io"}}
	got, err := Parse(c.Data())
	if err != nil || got.Domain != c.Domain || !got.TLS || got.CatchAll || len(got.SiteHosts) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	m, err := c.Manifest()
	if err != nil || !strings.Contains(string(m), "name: can-routing") {
		t.Fatal(string(m), err)
	}
	for _, bad := range []string{"", "a b", "acme.example`)", "UPPER.example"} {
		if _, err := Default(bad).Manifest(); err == nil {
			t.Errorf("domain %q accepted", bad)
		}
	}
}

func TestFromSiteRoute(t *testing.T) {
	for _, c := range []struct {
		name, in string
		want     Config
		matches  int
	}{
		{"domain and sslip", `{"spec":{"entryPoints":["websecure"],"routes":[{"match":"Host(` + "`" + `acme.example` + "`" + `)"},{"match":"Host(` + "`" + `1-2-3-4.sslip.io` + "`" + `)"}]}}`,
			Config{Domain: "acme.example", TLS: true, SiteHosts: []string{"acme.example", "1-2-3-4.sslip.io"}}, 2},
		{"plain HTTP catch-all", `{"spec":{"entryPoints":["web"],"routes":[{"match":"PathPrefix(` + "`" + `/` + "`" + `)"}]}}`,
			Config{Domain: "acme.example", CatchAll: true}, 1},
	} {
		got, err := FromSiteRoute("acme.example", []byte(c.in))
		if err != nil || !reflect.DeepEqual(got, c.want) || len(got.SiteMatches("/_forms")) != c.matches {
			t.Errorf("%s: %+v %v", c.name, got, err)
		}
	}
	for _, bad := range []string{"", "{}", `{"spec":{"routes":[{"match":"HostRegexp(` + "`" + `.*` + "`" + `)"}]}}`} {
		if _, err := FromSiteRoute("acme.example", []byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

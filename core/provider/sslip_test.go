package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if v, ok := f[h]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func TestDashedAndHosts(t *testing.T) {
	d, err := Dashed("203.0.113.7")
	if err != nil || d != "203-0-113-7" {
		t.Fatalf("%q %v", d, err)
	}
	hosts, err := InstantHosts("203.0.113.7", []string{"admin", "help"})
	want := []string{"203-0-113-7.sslip.io", "admin.203-0-113-7.sslip.io", "help.203-0-113-7.sslip.io"}
	if err != nil || fmt.Sprint(hosts) != fmt.Sprint(want) {
		t.Fatalf("%v %v", hosts, err)
	}
	if d, err := Dashed("2001:db8::1"); err != nil || d != "2001-db8--1" {
		t.Fatalf("v6: %q %v", d, err)
	}
	for _, bad := range []string{"", "host.example", "10.0.0.5", "127.0.0.1", "192.168.1.1", "0.0.0.0", "169.254.1.1"} {
		if _, err := Dashed(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestPlanInstantHTTPS(t *testing.T) {
	res := fakeResolver{"203-0-113-7.sslip.io": {"203.0.113.7"}}
	p := PlanInstant(context.Background(), "203.0.113.7", []string{"admin", "help"}, t0, nil, res)
	if p.Mode != ModeHTTPS || p.AdminViaTunnelOnly || len(p.Hosts) != 3 || p.URLs[1] != "https://admin.203-0-113-7.sslip.io" {
		t.Fatalf("%+v", p)
	}
}

func TestPlanInstantFallbacks(t *testing.T) {
	hosts, _ := InstantHosts("203.0.113.7", []string{"admin", "help"})
	good := fakeResolver{"203-0-113-7.sslip.io": {"203.0.113.7"}}
	hist := func(n int, ok bool, ago time.Duration) []Issuance {
		var h []Issuance
		for i := 0; i < n; i++ {
			h = append(h, Issuance{At: t0.Add(-ago), Hosts: hosts, OK: ok})
		}
		return h
	}
	cases := []struct {
		name   string
		ip     string
		res    Resolver
		hist   []Issuance
		reason string
	}{
		{"sslip unreachable", "203.0.113.7", fakeResolver{}, nil, "did not resolve"},
		{"resolves elsewhere", "203.0.113.7", fakeResolver{"203-0-113-7.sslip.io": {"198.51.100.1"}}, nil, "not 203.0.113.7"},
		{"weekly duplicate limit", "203.0.113.7", good, hist(4, true, 24*time.Hour), "per week"},
		{"failed validations", "203.0.113.7", good, hist(3, false, 10*time.Minute), "failed"},
		{"private ip", "10.1.2.3", good, nil, "not a public address"},
	}
	for _, c := range cases {
		p := PlanInstant(context.Background(), c.ip, []string{"admin", "help"}, t0, c.hist, c.res)
		if p.Mode != ModeHTTPFallback || !p.AdminViaTunnelOnly || !strings.Contains(p.Reason, c.reason) {
			t.Errorf("%s: %+v", c.name, p)
		}
		if len(p.URLs) != 1 || p.URLs[0] != "http://"+c.ip {
			t.Errorf("%s: fallback must serve only the public site over http: %v", c.name, p.URLs)
		}
	}
}

func TestPlanInstantIgnoresOldOrUnrelatedHistory(t *testing.T) {
	hosts, _ := InstantHosts("203.0.113.7", []string{"admin", "help"})
	res := fakeResolver{"203-0-113-7.sslip.io": {"203.0.113.7"}}
	old := []Issuance{}
	for i := 0; i < 10; i++ {
		old = append(old, Issuance{At: t0.Add(-8 * 24 * time.Hour), Hosts: hosts, OK: true})
		old = append(old, Issuance{At: t0.Add(-2 * time.Hour), Hosts: hosts, OK: false})
		old = append(old, Issuance{At: t0.Add(-time.Hour), Hosts: []string{"other.sslip.io"}, OK: true})
	}
	if p := PlanInstant(context.Background(), "203.0.113.7", []string{"admin", "help"}, t0, old, res); p.Mode != ModeHTTPS {
		t.Fatalf("%+v", p)
	}
}

func TestPlanInstantHostOrderDoesNotMatter(t *testing.T) {
	res := fakeResolver{"203-0-113-7.sslip.io": {"203.0.113.7"}}
	h := []Issuance{}
	for i := 0; i < 4; i++ {
		h = append(h, Issuance{At: t0.Add(-time.Hour), Hosts: []string{"help.203-0-113-7.sslip.io", "203-0-113-7.sslip.io", "admin.203-0-113-7.sslip.io"}, OK: true})
	}
	if p := PlanInstant(context.Background(), "203.0.113.7", []string{"admin", "help"}, t0, h, res); p.Mode != ModeHTTPFallback {
		t.Fatalf("%+v", p)
	}
}

func TestRecordIssuancePrunes(t *testing.T) {
	st := &State{Issued: []Issuance{{At: t0.Add(-15 * 24 * time.Hour)}, {At: t0.Add(-time.Hour)}}}
	RecordIssuance(st, []string{"a"}, true, t0)
	if len(st.Issued) != 2 || st.Issued[1].Hosts[0] != "a" || !st.Issued[1].OK {
		t.Fatalf("%+v", st.Issued)
	}
}

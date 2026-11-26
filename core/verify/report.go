// Package verify runs the checks of `can verify --server` against an installed server
// (specs/052-real-server-verification). Every check is read-only unless its probe says otherwise,
// nothing is destructive, and the report is meant to be pasted into a public issue: it carries no
// secret, address, domain or user name.
package verify

import (
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Schema names the report format.
const Schema = "can-verify/v1"

// Status of one check.
type Status string

// Statuses. Skip means "no verdict": not applicable, not asked for, or could not be decided.
const (
	Pass Status = "pass"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one result.
type Check struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Summary counts the results.
type Summary struct {
	Pass int `json:"pass"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// Report is the machine-readable result.
type Report struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	CanVersion  string    `json:"can_version"`
	Provider    string    `json:"provider"`
	Probes      []string  `json:"probes"`
	Checks      []Check   `json:"checks"`
	Summary     Summary   `json:"summary"`
}

func pass(id, detail string) Check { return Check{id, Pass, detail} }
func fail(id, detail string) Check { return Check{id, Fail, detail} }
func skip(id, detail string) Check { return Check{id, Skip, detail} }

func verdict(id string, ok bool, good, bad string) Check {
	if ok {
		return pass(id, good)
	}
	return fail(id, bad)
}

var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reIPish = regexp.MustCompile(`[0-9A-Fa-f:.]*[0-9A-Fa-f]*[:.][0-9A-Fa-f:.]+`)
	reURL   = regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^\s"']+`)
)

// Scrubber removes what identifies the installation. Details are built from numbers and fixed
// words in the first place; this is the second line for text that came from elsewhere.
type Scrubber struct{ pairs []string }

// NewScrubber replaces the domain, the server address and its sslip.io label, and any other
// identifying strings (company name, relay and issuer hosts) with placeholders.
func NewScrubber(domain, addr string, other ...string) *Scrubber {
	s := &Scrubber{}
	add := func(v, with string) {
		if len(v) >= 3 {
			s.pairs = append(s.pairs, v, with)
		}
	}
	add(domain, "<domain>")
	add(addr, "<server>")
	add(strings.NewReplacer(".", "-", ":", "-").Replace(addr), "<server-dashed>")
	for _, o := range other {
		add(o, "<redacted>")
	}
	return s
}

// Text scrubs one string.
func (s *Scrubber) Text(v string) string {
	v = reURL.ReplaceAllString(v, "<url>")
	v = reEmail.ReplaceAllString(v, "<email>")
	if len(s.pairs) > 0 {
		v = strings.NewReplacer(s.pairs...).Replace(v)
	}
	return reIPish.ReplaceAllStringFunc(v, func(m string) string {
		for _, t := range []string{strings.Trim(m, "."), strings.Trim(m, ".:")} {
			if _, err := netip.ParseAddr(t); err == nil {
				return strings.Replace(m, t, "<address>", 1)
			}
		}
		return m
	})
}

// Finish scrubs every detail and counts the results.
func (r *Report) Finish(s *Scrubber) {
	r.Summary = Summary{}
	for i := range r.Checks {
		r.Checks[i].Detail = s.Text(r.Checks[i].Detail)
		switch r.Checks[i].Status {
		case Pass:
			r.Summary.Pass++
		case Fail:
			r.Summary.Fail++
		default:
			r.Summary.Skip++
		}
	}
}

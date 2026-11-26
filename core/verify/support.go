package verify

import (
	"encoding/json"
	"strings"
	"unicode"
)

// The state of the support inbox's mail, as the script in the support module's container reports it
// (`can-support.php status`, specs/059-support-mailbox).

// MailPart is one of configuration, fetch and send.
type MailPart struct {
	State  string `json:"state"` // ok, failing, pending, not_configured
	Detail string `json:"detail,omitempty"`
}

// MailStatus is the whole report.
type MailStatus struct {
	Config MailPart `json:"config"`
	Fetch  MailPart `json:"fetch"`
	Send   MailPart `json:"send"`
}

// Failing reports whether any part is failing.
func (s MailStatus) Failing() bool {
	return s.Config.State == "failing" || s.Fetch.State == "failing" || s.Send.State == "failing"
}

// ParseMailStatus finds the report in what the command printed. The text comes from a third party's
// container: unknown states are refused and details are cut to one bounded line.
func ParseMailStatus(out string) (MailStatus, bool) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, `{"config"`) || len(l) > 4096 {
			continue
		}
		var s MailStatus
		if json.Unmarshal([]byte(l), &s) != nil {
			return MailStatus{}, false
		}
		for _, p := range []*MailPart{&s.Config, &s.Fetch, &s.Send} {
			switch p.State {
			case "ok", "failing", "pending", "not_configured":
			default:
				return MailStatus{}, false
			}
			p.Detail = strings.Map(func(r rune) rune {
				if unicode.IsPrint(r) {
					return r
				}
				return -1
			}, p.Detail)
			if len(p.Detail) > 240 {
				p.Detail = p.Detail[:240]
			}
		}
		return s, true
	}
	return MailStatus{}, false
}

// evalSupportMail judges the fact `support_mail`. Details are not repeated: they can hold addresses.
func evalSupportMail(f Facts, o ServerOptions) []Check {
	ids := []string{"support.mail-config", "support.mail-fetch", "support.mail-send"}
	if !o.SupportMail {
		return nil // a company without a support inbox: nothing to say
	}
	all := func(c func(id string) Check) []Check {
		return []Check{c(ids[0]), c(ids[1]), c(ids[2])}
	}
	s, ok := ParseMailStatus(strings.Join(f["support_mail"], "\n"))
	if !ok {
		return all(func(id string) Check {
			return fail(id, "the support module did not report its mail state (is it running? `can support status`)")
		})
	}
	var out []Check
	for i, p := range []MailPart{s.Config, s.Fetch, s.Send} {
		switch p.State {
		case "ok":
			out = append(out, pass(ids[i], "ok"))
		case "failing":
			out = append(out, fail(ids[i], "failing: see `can support status`"))
		default:
			out = append(out, skip(ids[i], strings.ReplaceAll(p.State, "_", " ")))
		}
	}
	return out
}

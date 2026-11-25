package installer

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	literalRE = regexp.MustCompile("`[^`]*`")
	termRE    = regexp.MustCompile(`^([A-Za-z]+)\(\x00(?:\s*,\s*\x00)*\)$`)
	// matchers other than Host that a module route may use to narrow a request further.
	allowedMatchers = map[string]bool{"pathprefix": true, "path": true, "method": true, "headers": true}
)

// checkMatch accepts one Traefik rule shape only: an `&&` chain of matcher calls with backtick string
// arguments that contains exactly one Host(`host`) (case-insensitive names). `||`, `!`, parentheses
// for grouping and every matcher outside the allowlist (HostRegexp, HeaderRegexp, ClientIP, Query...)
// are refused, because a disjunction or a loose matcher lets the route answer for other hosts.
func checkMatch(match, host string) error {
	if strings.Contains(match, "\x00") {
		return fmt.Errorf("rule contains a NUL byte")
	}
	var lits []string
	stripped := literalRE.ReplaceAllStringFunc(match, func(l string) string {
		lits = append(lits, l[1:len(l)-1])
		return "\x00"
	})
	if strings.Contains(stripped, "`") {
		return fmt.Errorf("rule %q has an unterminated string", match)
	}
	if strings.ContainsAny(stripped, "|!") {
		return fmt.Errorf("rule %q: '||' and '!' are not allowed; use one '&&' chain with a single Host", match)
	}
	hosts, next := 0, 0
	for _, term := range strings.Split(stripped, "&&") {
		term = strings.TrimSpace(term)
		m := termRE.FindStringSubmatch(term)
		if m == nil {
			return fmt.Errorf("rule %q: %q is not a plain matcher call (no grouping or operators besides &&)", match, strings.ReplaceAll(term, "\x00", "`..`"))
		}
		args := lits[next : next+strings.Count(term, "\x00")]
		next += len(args)
		switch name := strings.ToLower(m[1]); {
		case name == "host":
			if len(args) != 1 || args[0] != host {
				return fmt.Errorf("rule %q: host %v is not the module's host %q", match, args, host)
			}
			hosts++
		case allowedMatchers[name]:
		default:
			return fmt.Errorf("rule %q: matcher %s is not allowed (Host, PathPrefix, Path, Method, Headers)", match, m[1])
		}
	}
	if hosts != 1 {
		return fmt.Errorf("rule %q must contain exactly one Host(`%s`)", match, host)
	}
	return nil
}

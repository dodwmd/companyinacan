package config

import (
	"regexp"
	"strings"
)

// Outbound addresses are deliberately narrower than RFC 5321: bare ASCII addr-specs only, no quoted
// local parts, no display names, no source routes. '%' and '!' are refused because some relays read
// them as routing instructions ("user%elsewhere@relay"), which would defeat a recipient allow-list.
var (
	mailLocalRe  = regexp.MustCompile("^[A-Za-z0-9#$&'*+/=?^_`{|}~.-]{1,64}$")
	mailDomainRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)
)

// ValidAddress reports whether s is an address can-core is willing to put into a message header and
// an SMTP envelope. In particular it contains no CR, LF, space, comma or angle bracket.
func ValidAddress(s string) bool {
	if len(s) > 254 {
		return false
	}
	local, domain, ok := strings.Cut(s, "@")
	return ok && mailLocalRe.MatchString(local) && !strings.HasPrefix(local, ".") && !strings.HasSuffix(local, ".") &&
		!strings.Contains(local, "..") && len(domain) <= 253 && mailDomainRe.MatchString(domain)
}

// ValidRecipientRule reports whether s is an allow-list entry: an address or "@domain".
func ValidRecipientRule(s string) bool {
	if d, ok := strings.CutPrefix(s, "@"); ok {
		return mailDomainRe.MatchString(d)
	}
	return ValidAddress(s)
}

// RecipientAllowed reports whether addr matches one of the rules. A domain rule matches that domain
// exactly, not its subdomains; comparison ignores case.
func RecipientAllowed(addr string, rules []string) bool {
	if !ValidAddress(addr) {
		return false
	}
	_, domain, _ := strings.Cut(addr, "@")
	for _, r := range rules {
		if d, ok := strings.CutPrefix(r, "@"); ok {
			if strings.EqualFold(d, domain) {
				return true
			}
		} else if strings.EqualFold(r, addr) {
			return true
		}
	}
	return false
}

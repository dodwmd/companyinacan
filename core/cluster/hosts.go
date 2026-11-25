package cluster

import (
	"fmt"
	"regexp"
	"strings"
)

// Domain is the wildcard DNS zone used for local hostnames; it resolves to 127.0.0.1.
const Domain = "localtest.me"

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Hostname returns sub + ".localtest.me" ("" gives the bare zone). sub may contain
// several dot-separated labels.
func Hostname(sub string) (string, error) {
	sub = strings.ToLower(sub)
	if sub == "" {
		return Domain, nil
	}
	for _, l := range strings.Split(sub, ".") {
		if len(l) > 63 || !labelRE.MatchString(l) {
			return "", fmt.Errorf("invalid hostname label in %q", sub)
		}
	}
	return sub + "." + Domain, nil
}

// URL returns the local URL for a subdomain.
func URL(sub string, tls bool) (string, error) {
	h, err := Hostname(sub)
	if err != nil {
		return "", err
	}
	if tls {
		return "https://" + h, nil
	}
	return "http://" + h, nil
}

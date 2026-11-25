package contract

import (
	"regexp"
	"strings"
)

// chartDigestOK accepts an empty digest, the release placeholder (which `make release-check`
// refuses) or a real sha256: never a made-up value.
func chartDigestOK(d string) bool {
	return d == "" || strings.Contains(d, "REPLACE_WITH_") || regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(d)
}

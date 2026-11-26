package site

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"mime"
	"path"
	"regexp"
	"strings"
)

// ErrNoPage means the output has no such page.
var ErrNoPage = errors.New("site: no such page")

var (
	jsonLDRe   = regexp.MustCompile(`(?s)\n?<script type="application/ld\+json">.*?</script>`)
	styleRe    = regexp.MustCompile(`<link rel="stylesheet" href="/(assets/site\.[0-9a-f]+\.css)">`)
	internalRe = regexp.MustCompile(`href="/([^"/][^"]*)?"`)
)

// Preview returns one page ("" is the home page, otherwise a page slug) as a single self-contained
// document, to be shown in a sandboxed frame: the stylesheet is inline, the logo is a data URI,
// there is no script of any kind and links within the site do nothing. styleHash is the CSP source
// ('sha256-...') of the inline stylesheet.
func (o *Output) Preview(page string) (doc []byte, styleHash string, err error) {
	name := "index.html"
	if page != "" {
		name = page + ".html"
	}
	b, ok := o.Files[name]
	if !ok || strings.ContainsAny(page, "/.") {
		return nil, "", ErrNoPage
	}
	s := jsonLDRe.ReplaceAllString(string(b), "")
	var css []byte
	if m := styleRe.FindStringSubmatch(s); m != nil {
		css = o.Files[m[1]]
		s = strings.Replace(s, m[0], "<style>"+string(css)+"</style>", 1)
	}
	for p, data := range o.Files {
		if !IsLogoPath(p) {
			continue
		}
		uri := "data:" + mime.TypeByExtension(path.Ext(p)) + ";base64," + base64.StdEncoding.EncodeToString(data)
		s = strings.ReplaceAll(s, `src="/`+p+`"`, `src="`+uri+`"`)
		s = strings.ReplaceAll(s, `href="/`+p+`"`, `href="data:,"`)
	}
	// "/#section" is a section of the page shown; every other address of the site itself would leave
	// the preview (and the frame may not show the admin UI), so it goes nowhere.
	s = internalRe.ReplaceAllStringFunc(s, func(m string) string {
		rest := m[len(`href="/`) : len(m)-1]
		if strings.HasPrefix(rest, "#") {
			return `href="` + rest + `"`
		}
		return `href="#"`
	})
	sum := sha256.Sum256(css)
	return []byte(s), "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'", nil
}

package work

import (
	"html"
	"html/template"
	"net/url"
	"strings"
)

// renderText turns user text into HTML with a deliberately tiny, safe subset of Markdown:
// paragraphs, "- " bullet lists, fenced code blocks, **bold**, `code` and [text](http(s) url).
// Everything else is text. Raw HTML is never passed through: every character of the input is
// escaped, and the only tags that appear are the ones emitted here. Links are limited to http and
// https and carry rel="noopener noreferrer nofollow".
func renderText(src string) template.HTML {
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	var b strings.Builder
	lines := strings.Split(src, "\n")
	var para []string
	inList := false
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>")
			for i, l := range para {
				if i > 0 {
					b.WriteString("<br>")
				}
				b.WriteString(inline(l))
			}
			b.WriteString("</p>\n")
			para = para[:0]
		}
	}
	closeList := func() {
		if inList {
			b.WriteString("</ul>\n")
			inList = false
		}
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "```"):
			flush()
			closeList()
			var code []string
			i++
			for ; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
		case strings.TrimSpace(l) == "":
			flush()
			closeList()
		case strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* "):
			flush()
			if !inList {
				b.WriteString("<ul>\n")
				inList = true
			}
			b.WriteString("<li>" + inline(l[2:]) + "</li>\n")
		default:
			closeList()
			para = append(para, l)
		}
	}
	flush()
	closeList()
	return template.HTML(b.String()) //nolint:gosec // built only from escaped text and fixed tags
}

// inline renders **bold**, `code` and [text](url) inside one line.
func inline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				b.WriteString("<code>" + html.EscapeString(s[i+1:i+1+j]) + "</code>")
				i += j + 2
				continue
			}
		case strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				b.WriteString("<strong>" + html.EscapeString(s[i+2:i+2+j]) + "</strong>")
				i += j + 4
				continue
			}
		case s[i] == '[':
			if end := strings.Index(s[i:], "]("); end > 0 {
				if cl := strings.IndexByte(s[i+end+2:], ')'); cl >= 0 {
					text, href := s[i+1:i+end], s[i+end+2:i+end+2+cl]
					if safe, ok := safeURL(href); ok && strings.TrimSpace(text) != "" {
						b.WriteString(`<a href="` + html.EscapeString(safe) + `" rel="noopener noreferrer nofollow">` + html.EscapeString(text) + `</a>`)
						i += end + 2 + cl + 1
						continue
					}
				}
			}
		}
		// One rune at a time keeps multi-byte characters whole.
		r := []rune(s[i:])[0]
		b.WriteString(html.EscapeString(string(r)))
		i += len(string(r))
	}
	return b.String()
}

// safeURL accepts only absolute http and https URLs without credentials, whitespace, quotes or
// control characters. It is the single gate for every href the UI renders from user data.
func safeURL(raw string) (string, bool) {
	if raw == "" || len(raw) > 2000 || strings.ContainsAny(raw, " \t\r\n\x00\\\"'<>`") {
		return "", false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", false
	}
	return u.String(), true
}

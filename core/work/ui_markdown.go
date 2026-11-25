package work

import (
	"html"
	"html/template"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	// uiMaxRenderBytes is how much of one field is rendered; the rest is cut with a notice. The full
	// text stays available in the edit form and the API.
	uiMaxRenderBytes = 20000
	// uiPageRenderBudget bounds all markdown rendered for one page; fields past it are shown as
	// short plain text.
	uiPageRenderBudget = 150000
	// uiInlineWindow is how far apart the two ends of a link, bold or code span may be.
	uiInlineWindow = 2000
)

// renderBudget limits the total markdown work of one page.
type renderBudget struct{ left int }

func newRenderBudget() *renderBudget { return &renderBudget{left: uiPageRenderBudget} }

func (rb *renderBudget) render(src string) template.HTML {
	if rb.left <= 0 {
		cut := truncateUTF8(src, 400)
		return template.HTML("<p>" + html.EscapeString(cut) + "</p>\n" + truncatedNotice()) //nolint:gosec // escaped
	}
	rb.left -= min(len(src), uiMaxRenderBytes)
	return renderText(src)
}

func truncatedNotice() string {
	return `<p class="hint">` + html.EscapeString(msg("render.truncated")) + "</p>\n"
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// renderText turns user text into HTML with a deliberately tiny, safe subset of Markdown:
// paragraphs, "- " bullet lists, fenced code blocks, **bold**, `code` and [text](http(s) url).
// Everything else is text. Raw HTML is never passed through: every character of the input is
// escaped, and the only tags that appear are the ones emitted here. Links are limited to http and
// https and carry rel="noopener noreferrer nofollow". Work is linear in the input, which is cut at
// uiMaxRenderBytes.
func renderText(src string) template.HTML {
	truncated := len(src) > uiMaxRenderBytes
	src = truncateUTF8(src, uiMaxRenderBytes)
	src = strings.ToValidUTF8(src, "\uFFFD")
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
	if truncated {
		b.WriteString(truncatedNotice())
	}
	return template.HTML(b.String()) //nolint:gosec // built only from escaped text and fixed tags
}

// finder locates the next occurrence of a delimiter at or after a position, remembering the last
// answer so that scanning for the same delimiter never rereads text: all scans of one line add up
// to one pass.
type finder struct {
	s, pat string
	at     int
	done   bool
}

func (f *finder) next(from int) int {
	if f.done || from > len(f.s) {
		return -1
	}
	if f.at >= from {
		return f.at
	}
	i := strings.Index(f.s[from:], f.pat)
	if i < 0 {
		f.done = true
		return -1
	}
	f.at = from + i
	return f.at
}

// inline renders **bold**, `code` and [text](url) inside one line, in one left-to-right pass.
func inline(s string) string {
	var b strings.Builder
	code := finder{s: s, pat: "`", at: -1}
	bold := finder{s: s, pat: "**", at: -1}
	mid := finder{s: s, pat: "](", at: -1}
	paren := finder{s: s, pat: ")", at: -1}
	start := 0 // start of the pending literal run
	lit := func(to int) {
		if to > start {
			b.WriteString(html.EscapeString(s[start:to]))
		}
	}
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if j := code.next(i + 1); j > i+1 && j-i < uiInlineWindow {
				lit(i)
				b.WriteString("<code>" + html.EscapeString(s[i+1:j]) + "</code>")
				i, start = j+1, j+1
				continue
			}
		case s[i] == '*' && i+1 < len(s) && s[i+1] == '*':
			if j := bold.next(i + 2); j > i+2 && j-i < uiInlineWindow {
				lit(i)
				b.WriteString("<strong>" + html.EscapeString(s[i+2:j]) + "</strong>")
				i, start = j+2, j+2
				continue
			}
		case s[i] == '[':
			if end := mid.next(i + 1); end > 0 && end-i < uiInlineWindow {
				if cl := paren.next(end + 2); cl >= 0 && cl-end < uiInlineWindow {
					text, href := s[i+1:end], s[end+2:cl]
					if safe, ok := safeURL(href); ok && strings.TrimSpace(text) != "" {
						lit(i)
						b.WriteString(`<a href="` + html.EscapeString(safe) + `" rel="noopener noreferrer nofollow">` + html.EscapeString(text) + `</a>`)
						i, start = cl+1, cl+1
						continue
					}
				}
			}
		}
		i++
	}
	lit(len(s))
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

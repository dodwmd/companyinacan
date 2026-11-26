package site

import "strings"

type textPage struct {
	Title  string
	Blocks []textBlock
}

// textBlock is a heading, a list or a paragraph.
type textBlock struct {
	Heading string
	Items   []string
	Text    string
}

// textBlocks reads the plain-text body of a page: blank lines separate blocks, "## " starts a heading
// and "- " a list item. Everything else is text; the template escapes all of it.
func textBlocks(body string) []textBlock {
	var out []textBlock
	var para, items []string
	flush := func() {
		if len(para) > 0 {
			out = append(out, textBlock{Text: strings.Join(para, " ")})
		}
		if len(items) > 0 {
			out = append(out, textBlock{Items: items})
		}
		para, items = nil, nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "## "):
			flush()
			out = append(out, textBlock{Heading: strings.TrimSpace(line[3:])})
		case strings.HasPrefix(line, "- "):
			if len(para) > 0 {
				flush()
			}
			items = append(items, strings.TrimSpace(line[2:]))
		default:
			if len(items) > 0 {
				flush()
			}
			para = append(para, line)
		}
	}
	flush()
	return out
}

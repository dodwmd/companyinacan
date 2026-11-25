package notify

import (
	"bytes"
	htmltemplate "html/template"
	"strings"
	"sync"
	texttemplate "text/template"
	"unicode"
)

// Templates are written by the project, never by modules or users. Data values are untrusted and
// are escaped by html/template in the HTML body; the plain-text and subject renderings strip
// control characters (so a value can never add a header line).
type tmpl struct{ Subject, Body string }

var builtinTemplates = map[string]tmpl{
	"approval.requested": {"Approval needed: {{.approval_type}}", "A {{.approval_type}} request ({{.request_id}}) is waiting for your decision."},
	"approval.escalated": {"Approval escalated: {{.approval_type}}", "A {{.approval_type}} request ({{.request_id}}) has waited too long and is now also assigned to you."},
	"approval.decided":   {"Approval {{.outcome}}: {{.approval_type}}", "Your {{.approval_type}} request ({{.request_id}}) was {{.outcome}}."},
	"lead.created":       {"New lead", "A new lead ({{.lead_id}}) arrived."},
}

const genericSubject = `{{if .title}}{{.title}}{{else}}Notification: {{.type}}{{end}}`
const genericBody = `{{if .body}}{{.body}}{{else}}You have a new {{.type}} notification.{{end}}`

type compiled struct {
	subject *texttemplate.Template
	body    *texttemplate.Template
	hbody   *htmltemplate.Template
}

var (
	compileOnce sync.Once
	compiledSet map[string]compiled
	generic     compiled
)

func compileAll() {
	build := func(name string, t tmpl) compiled {
		return compiled{
			subject: texttemplate.Must(texttemplate.New(name + ".subject").Option("missingkey=zero").Parse(t.Subject)),
			body:    texttemplate.Must(texttemplate.New(name + ".body").Option("missingkey=zero").Parse(t.Body)),
			hbody:   htmltemplate.Must(htmltemplate.New(name + ".html").Option("missingkey=zero").Parse("<h3>" + t.Subject + "</h3><p>" + t.Body + "</p>")),
		}
	}
	compiledSet = map[string]compiled{}
	for k, v := range builtinTemplates {
		compiledSet[k] = build(k, v)
	}
	generic = build("generic", tmpl{genericSubject, genericBody})
}

// Rendered is a notification ready to show or send.
type Rendered struct{ Subject, Text, HTML string }

// Render renders type with data. Unknown types use the generic template, which shows the optional
// `title` and `body` values supplied by the publishing module.
func Render(typ string, data map[string]string) Rendered {
	compileOnce.Do(compileAll)
	c, ok := compiledSet[typ]
	if !ok {
		c = generic
	}
	d := make(map[string]string, len(data)+1)
	for k, v := range data {
		d[k] = v
	}
	d["type"] = typ
	var subj, text, html bytes.Buffer
	_ = c.subject.Execute(&subj, d)
	_ = c.body.Execute(&text, d)
	_ = c.hbody.Execute(&html, d)
	return Rendered{Subject: oneLine(subj.String(), 120), Text: cleanText(text.String(), 1000), HTML: html.String()}
}

// oneLine removes every control character and shortens to max runes.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), max)
}

func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	return truncate(s, max)
}

func truncate(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

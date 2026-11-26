package forms

// The hosted form page: GET <prefix>/<id>/form. The timing token a submission needs is in the page
// itself, so it works without JavaScript, which a static site served by can-static cannot run (its
// Content-Security-Policy allows no script). A site links to this page, or a page of its own with
// scripts keeps fetching <prefix>/<id>/token as before.

import (
	"bytes"
	"html/template"
	"io"
	"net/http"
	"strings"
)

const (
	pageSegment = "form"
	cssSegment  = "form.css"
)

// hostedCSP lets the page load its own stylesheet and post to this origin, nothing else.
const hostedCSP = "default-src 'none'; style-src 'self'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// htmlHead opens every HTML page the gateway serves. The empty icon stops the browser asking for
// a /favicon.ico (img-src data: allows it).
const htmlHead = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light dark"><link rel="icon" href="data:,">`

var hostedTmpl = template.Must(template.New("form").Parse(htmlHead + `<title>{{.Title}}</title>
<link rel="stylesheet" href="form.css">
</head><body><main><h1>{{.Title}}</h1>
<form method="post" action="../{{.ID}}">
<input type="hidden" name="_t" value="{{.Token}}">
<p class="hp" aria-hidden="true"><label for="f-{{.Honeypot}}">Leave this empty</label><input id="f-{{.Honeypot}}" name="{{.Honeypot}}" tabindex="-1" autocomplete="off"></p>
{{range .Fields}}<p><label for="f-{{.Name}}">{{.Label}}{{if not .Required}} (optional){{end}}</label>
{{if eq .Type "select"}}<select id="f-{{.Name}}" name="{{.Name}}"{{if .Required}} required{{end}}><option value="">Choose</option>{{range .Options}}<option value="{{.}}">{{.}}</option>{{end}}</select>
{{- else if .Multiline}}<textarea id="f-{{.Name}}" name="{{.Name}}" rows="6" maxlength="{{.MaxLength}}"{{if .Required}} required{{end}}></textarea>
{{- else}}<input id="f-{{.Name}}" name="{{.Name}}" type="{{.Type}}" maxlength="{{.MaxLength}}"{{if .Required}} required{{end}}{{if eq .Type "email"}} autocomplete="email"{{end}}>{{end}}</p>
{{end}}<p><button type="submit">Send</button></p>
</form></main></body></html>
`))

const hostedCSS = `:root{color-scheme:light dark}
body{font:1rem/1.5 system-ui,sans-serif;max-width:32rem;margin:0 auto;padding:1rem}
label{display:block;font-weight:600;margin-bottom:.25rem}
input,select,textarea{box-sizing:border-box;width:100%;min-height:2.75rem;padding:.5rem;font:inherit}
button{min-height:2.75rem;padding:.5rem 1.5rem;font:inherit}
.hp{position:absolute;left:-9999px}
`

type hostedField struct {
	Field
	Label string
}

func fieldLabel(name string) string {
	l := strings.ReplaceAll(name, "_", " ")
	return strings.ToUpper(l[:1]) + l[1:]
}

// serveHosted answers the page and its stylesheet. A form with a CAPTCHA needs the provider's
// script, which this page does not load, so it has no hosted page.
func (g *publicHandler) serveHosted(w http.ResponseWriter, r *http.Request, form *Form, segment string) {
	if form.Captcha {
		writeError(w, http.StatusNotFound, "not_found", "this form has no hosted page")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if segment == cssSegment {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = io.WriteString(w, hostedCSS)
		return
	}
	data := struct {
		ID, Title, Token, Honeypot string
		Fields                     []hostedField
	}{ID: form.ID, Title: form.Name, Token: g.signer.Issue(form.ID), Honeypot: form.Honeypot}
	if data.Title == "" {
		data.Title = fieldLabel(form.ID)
	}
	for _, f := range form.Fields {
		data.Fields = append(data.Fields, hostedField{Field: f, Label: fieldLabel(f.Name)})
	}
	var buf bytes.Buffer
	if err := hostedTmpl.Execute(&buf, data); err != nil {
		g.log.Error("hosted form render", "form", form.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.Header().Set("Content-Security-Policy", hostedCSP)
	// no-referrer would make the browser send "Origin: null" with the post, which the same-origin
	// check refuses.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

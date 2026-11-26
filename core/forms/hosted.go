package forms

// The hosted form page: GET <prefix>/<id>/form. The timing token a submission needs is in the page
// itself, so it works without JavaScript, which a static site served by can-static cannot run (its
// Content-Security-Policy allows no script). A site links to this page, or a page of its own with
// scripts keeps fetching <prefix>/<id>/token as before.

// It is written with html.EscapeString, not html/template: the template engine alone adds more than
// can-forms' binary size budget allows.

import (
	"html"
	"io"
	"net/http"
	"strconv"
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

const hostedCSS = `:root{color-scheme:light dark}
body{font:1rem/1.5 system-ui,sans-serif;max-width:32rem;margin:0 auto;padding:1rem}
label{display:block;font-weight:600;margin-bottom:.25rem}
input,select,textarea{box-sizing:border-box;width:100%;min-height:2.75rem;padding:.5rem;font:inherit}
button{min-height:2.75rem;padding:.5rem 1.5rem;font:inherit}
.hp{position:absolute;left:-9999px}
`

func fieldLabel(name string) string {
	l := strings.ReplaceAll(name, "_", " ")
	return strings.ToUpper(l[:1]) + l[1:]
}

// hostedPage renders the form. Every value from the configuration is escaped, in text and in the
// double-quoted attributes alike.
func hostedPage(form *Form, token string) string {
	e := html.EscapeString
	title := form.Name
	if title == "" {
		title = fieldLabel(form.ID)
	}
	var b strings.Builder
	b.WriteString(htmlHead + `<title>` + e(title) + `</title><link rel="stylesheet" href="` + cssSegment + `"></head><body><main><h1>` + e(title) + `</h1>
<form method="post" action="../` + e(form.ID) + `">
<input type="hidden" name="` + tokenField + `" value="` + e(token) + `">
<p class="hp" aria-hidden="true"><label for="f-` + e(form.Honeypot) + `">Leave this empty</label><input id="f-` + e(form.Honeypot) + `" name="` + e(form.Honeypot) + `" tabindex="-1" autocomplete="off"></p>
`)
	for _, f := range form.Fields {
		id, required, optional := e(f.Name), "", " (optional)"
		if f.Required {
			required, optional = " required", ""
		}
		b.WriteString(`<p><label for="f-` + id + `">` + e(fieldLabel(f.Name)) + optional + `</label>` + "\n")
		attrs := ` id="f-` + id + `" name="` + id + `"` + required
		switch {
		case f.Type == TypeSelect:
			b.WriteString(`<select` + attrs + `><option value="">Choose</option>`)
			for _, o := range f.Options {
				b.WriteString(`<option value="` + e(o) + `">` + e(o) + `</option>`)
			}
			b.WriteString(`</select>`)
		case f.Multiline:
			b.WriteString(`<textarea` + attrs + ` rows="6" maxlength="` + strconv.Itoa(f.MaxLength) + `"></textarea>`)
		default:
			if f.Type == TypeEmail {
				attrs += ` autocomplete="email"`
			}
			b.WriteString(`<input` + attrs + ` type="` + e(f.Type) + `" maxlength="` + strconv.Itoa(f.MaxLength) + `">`)
		}
		b.WriteString("</p>\n")
	}
	b.WriteString("<p><button type=\"submit\">Send</button></p>\n</form></main></body></html>\n")
	return b.String()
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
	w.Header().Set("Content-Security-Policy", hostedCSP)
	// no-referrer would make the browser send "Origin: null" with the post, which the same-origin
	// check refuses.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, hostedPage(form, g.signer.Issue(form.ID)))
}

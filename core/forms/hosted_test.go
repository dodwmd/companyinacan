package forms

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

var hostedToken = regexp.MustCompile(`name="_t" value="([^"]+)"`)

// A browser on a site that runs no script (can-static allows none) can only submit a form whose
// timing token is in the page.
func TestHostedFormPageSubmitsWithoutScript(t *testing.T) {
	h := newHarness(t, harnessCfg)
	rec := h.do(h.pub, http.MethodGet, "/_forms/contact/form", nil, nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("hosted page: %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`<html lang="en">`, "<main>", `action="../contact"`, `href="form.css"`, `<label for="f-email">`, `name="website"`} {
		if !strings.Contains(body, want) {
			t.Errorf("hosted page lacks %q", want)
		}
	}
	if strings.Contains(body, "<script") || strings.Contains(body, "style=") {
		t.Errorf("the page needs script or inline style:\n%s", body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP %q", csp)
	}
	// With no-referrer a browser posts "Origin: null", which checkOrigin refuses.
	if rp := rec.Header().Get("Referrer-Policy"); rp != "same-origin" {
		t.Errorf("Referrer-Policy %q", rp)
	}
	m := hostedToken.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no timing token in the page")
	}
	h.clock.advance(5 * time.Second)
	v := goodValues(m[1])
	if got := h.postForm("/_forms/contact", v, nil); got.Code != http.StatusSeeOther {
		t.Fatalf("submit with the page's token: %d %s", got.Code, got.Body)
	}
	if len(h.leads()) != 1 {
		t.Fatal("the lead is not stored")
	}

	css := h.do(h.pub, http.MethodGet, "/contact/form.css", nil, nil)
	if css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet: %d", css.Code)
	}
	if got := h.do(h.pub, http.MethodPost, "/_forms/contact/form", nil, nil); got.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to the page: %d", got.Code)
	}
	if got := h.do(h.pub, http.MethodGet, "/_forms/nope/form", nil, nil); got.Code != http.StatusNotFound {
		t.Errorf("unknown form: %d", got.Code)
	}
}

func TestHostedFormPageNotOfferedWithCaptcha(t *testing.T) {
	h := newHarness(t, harnessCfg)
	h.app.cfg.Forms[0].Captcha = true
	if got := h.do(h.pub, http.MethodGet, "/_forms/contact/form", nil, nil); got.Code != http.StatusNotFound {
		t.Fatalf("a CAPTCHA form has no hosted page: %d", got.Code)
	}
}

// The error a visitor sees after a refused post is a whole page: language, landmark, no favicon request.
func TestHTMLErrorPageIsAccessible(t *testing.T) {
	h := newHarness(t, harnessCfg)
	rec := h.postForm("/_forms/contact", goodValues(""), nil)
	body := rec.Body.String()
	for _, want := range []string{`<html lang="en">`, "<main>", `<link rel="icon" href="data:,">`, "<h1>"} {
		if !strings.Contains(body, want) {
			t.Errorf("error page lacks %q: %s", want, body)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "img-src data:") {
		t.Errorf("CSP blocks the empty icon: %q", rec.Header().Get("Content-Security-Policy"))
	}
}

// The page is assembled by hand: text from the configuration must not become markup.
func TestHostedFormPageEscapesConfiguration(t *testing.T) {
	hostile := `"><script>alert(1)</script>`
	page := hostedPage(&Form{ID: "contact", Name: hostile, Honeypot: "website", Fields: []Field{
		{Name: "topic", Type: TypeSelect, Options: []string{hostile}},
		{Name: "message", Type: TypeText, Multiline: true, Required: true, MaxLength: 10},
	}}, "tok")
	if strings.Contains(page, "<script") || !strings.Contains(page, "&#34;&gt;&lt;script&gt;") {
		t.Fatalf("not escaped:\n%s", page)
	}
	for _, want := range []string{`<select id="f-topic" name="topic">`, `Topic (optional)`, `<textarea id="f-message" name="message" required rows="6" maxlength="10">`} {
		if !strings.Contains(page, want) {
			t.Errorf("lacks %q:\n%s", want, page)
		}
	}
}

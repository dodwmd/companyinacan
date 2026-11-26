package forms

import (
	"net/http"
	"net/url"
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

// The page's links are relative: with a trailing slash they would point one level too deep.
func TestHostedFormPageTrailingSlash(t *testing.T) {
	h := newHarness(t, harnessCfg)
	for _, p := range []string{"/contact/form/", "/_forms/contact/form/"} {
		rec := h.do(h.pub, http.MethodGet, p, nil, nil)
		if rec.Code != http.StatusPermanentRedirect {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		// Resolved by the browser against its own path, which still has the prefix the edge strips.
		base, _ := url.Parse("https://acme.example/_forms/contact/form/")
		ref, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || base.ResolveReference(ref).Path != "/_forms/contact/form" {
			t.Fatalf("%s: Location %q", p, rec.Header().Get("Location"))
		}
	}
	for _, p := range []string{"/contact/form//", "/contact/form.css/", "/nope/form/"} {
		if rec := h.do(h.pub, http.MethodGet, p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestHostedPagesLeadBackToTheSite(t *testing.T) {
	h := newHarness(t, harnessCfg)
	for name, body := range map[string]string{
		"form":  h.do(h.pub, http.MethodGet, "/contact/form", nil, nil).Body.String(),
		"error": h.postForm("/_forms/contact", goodValues(""), nil).Body.String(),
	} {
		if !strings.Contains(body, `<a href="/">Back to the website</a>`) {
			t.Errorf("%s page has no way back: %s", name, body)
		}
	}
}

// A browser counts a line break as one character against maxlength and posts it as CR LF.
func TestLineBreakCountsAsOneCharacter(t *testing.T) {
	f := &Form{ID: "contact", Fields: []Field{{Name: "message", Type: TypeText, Multiline: true, MaxLength: 5}}}
	clean, errs := f.Validate(map[string]string{"message": "ab\r\ncd"})
	if len(errs) != 0 || clean["message"] != "ab\ncd" {
		t.Fatalf("%q %v", clean["message"], errs)
	}
	if _, errs = f.Validate(map[string]string{"message": "ab\r\ncde"}); len(errs) != 1 || errs[0].Code != "too_long" {
		t.Fatalf("over the limit: %v", errs)
	}
}

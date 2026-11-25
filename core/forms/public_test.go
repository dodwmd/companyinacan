package forms

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func jsonField(t *testing.T, body, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not json: %q", body)
	}
	s, _ := m[key].(string)
	return s
}

func (h *harness) leads() []Lead {
	h.t.Helper()
	l, err := h.app.store.List(context.Background(), ListFilter{Limit: 100})
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}

func TestSubmitUrlencodedRedirects(t *testing.T) {
	h := newHarness(t, harnessCfg)
	rec := h.postForm("/_forms/contact", goodValues(h.token("contact")), nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/thanks/" {
		t.Fatalf("want 303 /thanks/, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	ls := h.leads()
	if len(ls) != 1 || ls[0].FormID != "contact" || ls[0].Email != "ada@example.com" || ls[0].Name != "Ada" {
		t.Fatalf("lead not stored correctly: %+v", ls)
	}
	if _, ok := ls[0].Fields["_t"]; ok {
		t.Fatal("token must not be stored")
	}
	if want := h.clock.now().AddDate(0, 0, 730); ls[0].RetentionUntil.Unix() < want.Unix()-1 || ls[0].RetentionUntil.Unix() > want.Unix()+1 {
		t.Fatalf("retention not set from config: %v", ls[0].RetentionUntil)
	}
}

func TestSubmitWithoutPrefixAfterStripping(t *testing.T) {
	h := newHarness(t, harnessCfg)
	rec := h.postForm("/contact", goodValues(h.token("contact")), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("prefix-stripped path must work: %d %s", rec.Code, rec.Body)
	}
}

func TestSubmitJSON(t *testing.T) {
	h := newHarness(t, harnessCfg)
	tok := h.token("contact")
	body := `{"name":"Ada","email":"ada@example.com","message":"hi","_t":"` + tok + `"}`
	rec := h.do(h.pub, http.MethodPost, "/_forms/contact", strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || strings.Contains(rec.Body.String(), "ld_") {
		t.Fatalf("json ack expected without internal ids: %s", rec.Body)
	}
	if len(h.leads()) != 1 {
		t.Fatal("not stored")
	}
}

func TestSubmitHTMLThanksWithoutRedirect(t *testing.T) {
	h := newHarness(t, harnessCfg)
	h.app.cfg.Forms[0].Redirect = ""
	rec := h.postForm("/_forms/contact", goodValues(h.token("contact")), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("want html thanks page, got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestTokenRequiredAndTooFast(t *testing.T) {
	h := newHarness(t, harnessCfg)
	v := goodValues("")
	v.Del("_t")
	rec := h.postForm("/_forms/contact", v, map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusBadRequest || jsonField(t, rec.Body.String(), "code") != "form_token_invalid" {
		t.Fatalf("missing token: %d %s", rec.Code, rec.Body)
	}
	tokRec := h.do(h.pub, http.MethodGet, "/_forms/contact/token", nil, nil)
	tok := jsonField(t, tokRec.Body.String(), "token")
	rec = h.postForm("/_forms/contact", goodValues(tok), map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusBadRequest || jsonField(t, rec.Body.String(), "code") != "submitted_too_fast" {
		t.Fatalf("too fast: %d %s", rec.Code, rec.Body)
	}
	h.clock.advance(5 * time.Second)
	if rec = h.postForm("/_forms/contact", goodValues(tok), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("after waiting must pass: %d %s", rec.Code, rec.Body)
	}
	if len(h.leads()) != 1 {
		t.Fatal("only the final attempt must be stored")
	}
}

func TestTokenExpiredAndWrongForm(t *testing.T) {
	h := newHarness(t, harnessCfg)
	tok := h.token("contact")
	h.clock.advance(25 * time.Hour)
	rec := h.postForm("/_forms/contact", goodValues(tok), map[string]string{"Accept": "application/json"})
	if jsonField(t, rec.Body.String(), "code") != "form_token_expired" {
		t.Fatalf("expired: %d %s", rec.Code, rec.Body)
	}
	other := h.token("quote")
	rec = h.postForm("/_forms/contact", goodValues(other), map[string]string{"Accept": "application/json"})
	if jsonField(t, rec.Body.String(), "code") != "form_token_invalid" {
		t.Fatalf("token for another form must be invalid: %s", rec.Body)
	}
}

func TestHoneypotSilentlyDropped(t *testing.T) {
	h := newHarness(t, harnessCfg)
	v := goodValues(h.token("contact"))
	v.Set("website", "http://spam.example")
	rec := h.postForm("/_forms/contact", v, map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("bots should see success, got %d", rec.Code)
	}
	if len(h.leads()) != 0 {
		t.Fatal("honeypot submission must not be stored")
	}
	h.app.dispatcher.Flush()
	if len(h.mailer.all()) != 0 {
		t.Fatal("honeypot submission must not notify")
	}
}

func TestValidationErrors(t *testing.T) {
	h := newHarness(t, harnessCfg)
	v := goodValues(h.token("contact"))
	v.Del("name")
	v.Set("evil", "x")
	rec := h.postForm("/_forms/contact", v, map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", rec.Code)
	}
	var out struct {
		Errors []FieldError `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	got := codes(out.Errors)
	if got["name"] != "required" || got["evil"] != "unknown_field" {
		t.Fatalf("errors: %+v", out.Errors)
	}
	if len(h.leads()) != 0 {
		t.Fatal("invalid submission stored")
	}
	// Browser (non-JSON) clients get an HTML page and the input is escaped.
	v.Set("<script>", "x")
	rec = h.postForm("/_forms/contact", v, nil)
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "<script>") {
		t.Fatalf("html error page must escape field names: %d %s", rec.Code, rec.Body)
	}
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t, harnessCfg) // 5 per minute
	for i := 0; i < 5; i++ {
		rec := h.postForm("/_forms/contact", url.Values{}, nil) // invalid but counted
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited too early", i)
		}
	}
	rec := h.postForm("/_forms/contact", url.Values{}, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("want 429 with Retry-After, got %d", rec.Code)
	}
}

func TestRateLimitUsesForwardedForOnlyWhenTrusted(t *testing.T) {
	h := newHarness(t, harnessCfg)
	for i := 0; i < 6; i++ {
		h.postForm("/_forms/contact", url.Values{}, map[string]string{"X-Forwarded-For": "198.51.100." + string(rune('1'+i))})
	}
	rec := h.postForm("/_forms/contact", url.Values{}, map[string]string{"X-Forwarded-For": "198.51.100.99"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("untrusted X-Forwarded-For must be ignored (spoofing): got %d", rec.Code)
	}

	h2 := newHarness(t, "trust_proxy: true\n"+harnessCfg)
	for i := 0; i < 6; i++ {
		h2.postForm("/_forms/contact", url.Values{}, map[string]string{"X-Forwarded-For": "10.0.0.1, 198.51.100.5"})
	}
	if rec := h2.postForm("/_forms/contact", url.Values{}, map[string]string{"X-Forwarded-For": "198.51.100.6"}); rec.Code == http.StatusTooManyRequests {
		t.Fatal("different client behind trusted proxy must have its own bucket")
	}
	if rec := h2.postForm("/_forms/contact", url.Values{}, map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.5"}); rec.Code != http.StatusTooManyRequests {
		t.Fatal("client-supplied left-most XFF entries must not evade the limit (right-most is used)")
	}
}

func TestRequestShapeErrors(t *testing.T) {
	h := newHarness(t, harnessCfg)
	if rec := h.do(h.pub, http.MethodPost, "/_forms/nope", strings.NewReader("a=1"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown form: %d", rec.Code)
	}
	if rec := h.do(h.pub, http.MethodGet, "/_forms/contact", nil, nil); rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Header().Get("Allow"), "POST") {
		t.Fatalf("GET on form: %d", rec.Code)
	}
	if rec := h.do(h.pub, http.MethodPost, "/_forms/contact", strings.NewReader("x"), map[string]string{"Content-Type": "text/plain"}); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("media type: %d", rec.Code)
	}
	big := strings.NewReader("message=" + strings.Repeat("a", 70000))
	if rec := h.do(h.pub, http.MethodPost, "/_forms/contact", big, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("size limit: %d", rec.Code)
	}
	if rec := h.do(h.pub, http.MethodPost, "/_forms/contact", strings.NewReader("a=1&a=2"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", rec.Code)
	}
	if rec := h.do(h.pub, http.MethodGet, "/other/path/here", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rec.Code)
	}
}

func TestCaptcha(t *testing.T) {
	h := newHarness(t, harnessCfg)
	v := url.Values{"email": {"a@example.com"}, "cf-turnstile-response": {"tok123"}}
	h.captcha.ok = false
	rec := h.postForm("/_forms/quote", v, map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusBadRequest || jsonField(t, rec.Body.String(), "code") != "captcha_failed" || h.captcha.got != "tok123" {
		t.Fatalf("failed captcha: %d %s got=%q", rec.Code, rec.Body, h.captcha.got)
	}
	h.captcha.err = errors.New("upstream down")
	if rec = h.postForm("/_forms/quote", v, map[string]string{"Accept": "application/json"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("verifier error must fail closed: %d", rec.Code)
	}
	h.captcha.err, h.captcha.ok = nil, true
	if rec = h.postForm("/_forms/quote", v, map[string]string{"Accept": "application/json"}); rec.Code != http.StatusCreated {
		t.Fatalf("good captcha: %d %s", rec.Code, rec.Body)
	}
	ls := h.leads()
	if len(ls) != 1 {
		t.Fatalf("leads: %d", len(ls))
	}
	if _, ok := ls[0].Fields["cf-turnstile-response"]; ok {
		t.Fatal("captcha response must not be stored")
	}
}

func TestOriginAndCORS(t *testing.T) {
	h := newHarness(t, harnessCfg)
	cross := map[string]string{"Origin": "https://evil.example", "Accept": "application/json"}
	if rec := h.postForm("/_forms/quote", url.Values{"email": {"a@example.com"}}, cross); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin must be rejected by default: %d", rec.Code)
	}
	if len(h.leads()) != 0 {
		t.Fatal("cross-origin post stored")
	}
	// httptest requests default to Host example.com.
	r := h.do(h.pub, http.MethodPost, "/_forms/quote", strings.NewReader("email=a%40example.com&cf-turnstile-response=x"), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://example.com", "Accept": "application/json"})
	if r.Code != http.StatusCreated {
		t.Fatalf("same-origin: %d %s", r.Code, r.Body)
	}
	if r.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("same-origin needs no CORS headers")
	}
	allowed := map[string]string{"Origin": "https://www.example.com", "Accept": "application/json"}
	rec := h.postForm("/_forms/quote", url.Values{"email": {"b@example.com"}, "cf-turnstile-response": {"x"}}, allowed)
	if rec.Code != http.StatusCreated || rec.Header().Get("Access-Control-Allow-Origin") != "https://www.example.com" || rec.Header().Get("Vary") != "Origin" {
		t.Fatalf("allow-listed origin: %d %v", rec.Code, rec.Header())
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("credentials must never be allowed")
	}
	pre := h.do(h.pub, http.MethodOptions, "/_forms/quote", nil, map[string]string{"Origin": "https://www.example.com", "Access-Control-Request-Method": "POST"})
	if pre.Code != http.StatusNoContent || !strings.Contains(pre.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Fatalf("preflight: %d %v", pre.Code, pre.Header())
	}
	pre = h.do(h.pub, http.MethodOptions, "/_forms/quote", nil, map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	if pre.Code != http.StatusForbidden {
		t.Fatalf("preflight from unknown origin: %d", pre.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t, harnessCfg)
	resp := h.do(h.pub, http.MethodGet, "/_forms/contact/token", nil, nil)
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "no-store",
		"X-Frame-Options":        "DENY",
	}
	for k, v := range want {
		if resp.Header().Get(k) != v {
			t.Errorf("%s = %q, want %q", k, resp.Header().Get(k), v)
		}
	}
	if !strings.Contains(resp.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("CSP: %q", resp.Header().Get("Content-Security-Policy"))
	}
	for _, p := range []string{"/_forms/nope", "/healthz"} {
		if r := h.do(h.pub, http.MethodGet, p, nil, nil); r.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers missing on %d", p, r.Code)
		}
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t, harnessCfg)
	if rec := h.do(h.pub, http.MethodGet, "/healthz", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	if rec := h.do(h.api, http.MethodGet, "/healthz", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("api healthz: %d", rec.Code)
	}
}

func TestSubmitNotifiesByEmail(t *testing.T) {
	h := newHarness(t, harnessCfg)
	v := goodValues(h.token("contact"))
	v.Set("name", "Ada\r\nBcc: x@evil.example")
	rec := h.postForm("/_forms/contact", v, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("CRLF in name must be rejected as invalid characters: %d", rec.Code)
	}
	v.Set("name", "Ada")
	if rec = h.postForm("/_forms/contact", v, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("submit: %d", rec.Code)
	}
	h.app.dispatcher.Flush()
	sent := h.mailer.all()
	if len(sent) != 1 {
		t.Fatalf("emails: %d", len(sent))
	}
	e := sent[0]
	if len(e.To) != 1 || e.To[0] != "owner@example.com" || e.ReplyTo != "ada@example.com" {
		t.Fatalf("addressing: %+v", e)
	}
	if e.Subject != "New Contact submission" || !strings.Contains(e.Text, "Hello there") {
		t.Fatalf("content: %+v", e)
	}
	ls := h.leads()
	ds, _ := h.app.store.Deliveries(context.Background(), ls[0].ID)
	if len(ds) != 1 || ds[0].Kind != "email" || !ds[0].OK {
		t.Fatalf("delivery not recorded: %+v", ds)
	}
}

func TestMailerFailureDoesNotLoseLead(t *testing.T) {
	h := newHarness(t, harnessCfg)
	h.mailer.err = errors.New("smtp down")
	rec := h.postForm("/_forms/contact", goodValues(h.token("contact")), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("submission must succeed even if notification fails: %d", rec.Code)
	}
	h.app.dispatcher.Flush()
	ls := h.leads()
	if len(ls) != 1 {
		t.Fatal("lead lost")
	}
	ds, _ := h.app.store.Deliveries(context.Background(), ls[0].ID)
	if len(ds) != 1 || ds[0].OK || !strings.Contains(ds[0].LastError, "smtp down") {
		t.Fatalf("failed delivery must be recorded: %+v", ds)
	}
}

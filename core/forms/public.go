package forms

import (
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Reserved input names that are not form fields.
const tokenField = "_t"

// publicHandler serves the same-origin form endpoints. Traefik strips the /_forms prefix
// before the request arrives; the prefix is also accepted so the service works unstripped.
type publicHandler struct {
	cfg      *Config
	store    *Store
	signer   *TokenSigner
	limiter  *RateLimiter
	captcha  CaptchaVerifier
	notifier *Dispatcher
	log      *slog.Logger
	now      func() time.Time
}

func (g *publicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	path := r.URL.Path
	if pre := g.cfg.PathPrefix; path == pre || strings.HasPrefix(path, pre+"/") {
		path = strings.TrimPrefix(path, pre)
	}
	path = strings.Trim(path, "/")
	if path == "healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	segs := strings.Split(path, "/")
	if path == "" || len(segs) > 2 || (len(segs) == 2 && segs[1] != "token") {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	form := g.cfg.Form(segs[0])
	if form == nil {
		writeError(w, http.StatusNotFound, "unknown_form", "no such form")
		return
	}
	if !g.checkOrigin(w, r) {
		return
	}
	isToken := len(segs) == 2
	switch {
	case r.Method == http.MethodOptions:
		g.preflight(w)
	case isToken && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"token": g.signer.Issue(form.ID)})
	case isToken:
		w.Header().Set("Allow", "GET, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
	case r.Method == http.MethodPost:
		g.submit(w, r, form)
	default:
		w.Header().Set("Allow", "POST, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
	}
}

// checkOrigin enforces the same-origin default: a request carrying an Origin header must
// match the host it was sent to, or be on the explicit allow-list. Requests without
// Origin (non-browser clients, same-origin GET) pass; they gain nothing from skipping it.
func (g *publicHandler) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := r.Host
	if g.cfg.TrustProxy {
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			host = xfh
		}
	}
	if u, err := url.Parse(origin); err == nil && u.Host != "" && u.Host == host {
		return true
	}
	if slices.Contains(g.cfg.CORSAllowedOrigins, origin) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		return true
	}
	writeError(w, http.StatusForbidden, "origin_not_allowed", "cross-origin requests are not allowed for this form")
	return false
}

func (g *publicHandler) preflight(w http.ResponseWriter) {
	if w.Header().Get("Access-Control-Allow-Origin") == "" { // same-origin or no Origin: nothing to negotiate
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") ||
		strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (g *publicHandler) fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if wantsJSON(r) {
		writeError(w, status, code, msg)
		return
	}
	writeHTMLPage(w, status, "Could not send your message", msg)
}

func (g *publicHandler) succeed(w http.ResponseWriter, r *http.Request, f *Form) {
	switch {
	case wantsJSON(r):
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	case f.Redirect != "":
		http.Redirect(w, r, f.Redirect, http.StatusSeeOther)
	default:
		writeHTMLPage(w, http.StatusOK, "Thank you", "Your message has been received.")
	}
}

func (g *publicHandler) submit(w http.ResponseWriter, r *http.Request, form *Form) {
	ip := clientIP(r, g.cfg.TrustProxy)
	if ok, retry := g.limiter.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		g.fail(w, r, http.StatusTooManyRequests, "rate_limited", "Too many submissions. Please try again later.")
		return
	}

	in, err := ParseBody(w, r, g.cfg.MaxBodyBytes)
	switch err {
	case nil:
	case ErrUnsupportedMedia:
		g.fail(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send the form as application/x-www-form-urlencoded or application/json.")
		return
	case ErrTooLarge:
		g.fail(w, r, http.StatusRequestEntityTooLarge, "too_large", "The submission is too large.")
		return
	default:
		g.fail(w, r, http.StatusBadRequest, "bad_request", "The submission could not be read.")
		return
	}

	// Pull anti-spam inputs out so they are neither validated as fields nor stored.
	honeypot := strings.TrimSpace(in[form.Honeypot])
	delete(in, form.Honeypot)
	token := in[tokenField]
	delete(in, tokenField)
	var captchaResp string
	if cf := CaptchaResponseField(g.cfg.Captcha.Provider); cf != "" {
		captchaResp = in[cf]
		delete(in, cf)
	}

	if honeypot != "" {
		// Look successful so the bot does not adapt; store and notify nothing.
		g.log.Info("submission dropped", "form", form.ID, "reason", "honeypot")
		g.succeed(w, r, form)
		return
	}

	if form.MinFill() > 0 || token != "" {
		if err := g.signer.Check(form.ID, token, form.MinFill()); err != nil {
			g.log.Info("submission rejected", "form", form.ID, "reason", err.Error())
			switch err {
			case ErrTooFast:
				g.fail(w, r, http.StatusBadRequest, "submitted_too_fast", "That was fast. Please wait a moment and submit again.")
			case ErrTokenExpired:
				g.fail(w, r, http.StatusBadRequest, "form_token_expired", "The form expired. Please reload the page and try again.")
			default:
				g.fail(w, r, http.StatusBadRequest, "form_token_invalid", "The form could not be verified. Please reload the page and try again.")
			}
			return
		}
	}

	if form.Captcha && g.captcha != nil {
		ok, err := g.captcha.Verify(r.Context(), captchaResp, ip)
		if err != nil {
			g.log.Error("captcha verification unavailable", "form", form.ID, "error", err)
			g.fail(w, r, http.StatusServiceUnavailable, "captcha_unavailable", "Verification is temporarily unavailable. Please try again.")
			return
		}
		if !ok {
			g.log.Info("submission rejected", "form", form.ID, "reason", "captcha")
			g.fail(w, r, http.StatusBadRequest, "captcha_failed", "The verification challenge failed.")
			return
		}
	}

	clean, ferrs := form.Validate(in)
	if len(ferrs) > 0 {
		if wantsJSON(r) {
			writeJSON(w, http.StatusUnprocessableEntity, apiError{Code: "invalid_input", Message: "Some fields are invalid.", Errors: ferrs})
			return
		}
		lines := make([]string, 0, len(ferrs))
		for _, e := range ferrs {
			lines = append(lines, e.Field+": "+e.Message)
		}
		writeHTMLPage(w, http.StatusUnprocessableEntity, "Please check your input", lines...)
		return
	}

	now := g.now()
	lead := &Lead{
		FormID:         form.ID,
		Fields:         clean,
		CreatedAt:      now,
		RetentionUntil: now.AddDate(0, 0, form.RetentionDays),
		Name:           clean["name"],
	}
	for _, fl := range form.Fields {
		if fl.Type == TypeEmail && clean[fl.Name] != "" {
			lead.Email = clean[fl.Name]
			break
		}
	}
	if err := g.store.Insert(r.Context(), lead); err != nil {
		g.log.Error("store lead", "form", form.ID, "error", err)
		g.fail(w, r, http.StatusInternalServerError, "internal", "Something went wrong. Please try again later.")
		return
	}
	g.log.Info("lead stored", "form", form.ID, "lead", lead.ID)
	g.notifier.Enqueue(lead, form)
	g.succeed(w, r, form)
}

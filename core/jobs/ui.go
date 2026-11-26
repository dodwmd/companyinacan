package jobs

// The server-rendered recruiting web UI of the jobs module (epic E16b, specs/027-jobs-ui).
// html/template, an embedded stylesheet, no JavaScript. It is a client of the Store exactly like
// the JSON API: every page calls the same Store methods, which scope and authorise by the caller's
// Principal, so there is no second authorisation path. The UI decides only presentation.

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/lazytmpl"
	"github.com/dodwmd/companyinacan/core/modclient"
)

//go:embed ui/templates ui/static
var uiAssets embed.FS

// boardLimit is how many cards one pipeline stage draws; the rest are counted, not drawn.
const boardLimit = 25

// listLimit bounds every other list the UI draws.
const listLimit = 50

// UI serves the web UI.
type UI struct {
	Store *Store
	Auth  Authenticator // the same chain the API uses; it validates the token presented at login
	// Sessions is can-core's launch-ticket API (specs/036-module-ui-sso); nil when the module is not
	// installed under can-core, which turns /login/ticket off (404).
	Sessions modclient.SessionAPI
	// NoTokenLogin turns the pasted-token sign-in off (ui.token_login: false).
	NoTokenLogin bool
	Log          *slog.Logger
	Site         SiteInfo
	// Secure sets the Secure attribute (and the __Host- cookie prefix). False only for plain-HTTP
	// local development.
	Secure     bool
	TrustProxy bool
	Now        func() time.Time
	// Timeout bounds the database work of one request; past it the page is a 503.
	Timeout time.Duration

	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	RecheckEvery    time.Duration

	once     sync.Once
	sessions sessionStore
	tpl      *lazytmpl.Set // pages are parsed on first use
	failIP   *rateLimiter
	perUser  *rateLimiter
	reveals  *rateLimiter // reveals of candidate data, per subject
}

type route struct {
	pattern string
	h       http.HandlerFunc
}

func (u *UI) routes() []route {
	return []route{
		{"GET /login", u.loginPage},
		{"POST /login", u.loginPost},
		{"GET /login/ticket", u.ticketPage},
		{"POST /login/ticket", u.ticketPost},
		{"POST /logout", u.authed(u.logout)},
		{"GET /{$}", u.authed(u.home)},
		{"GET /jobs", u.authed(u.jobList)},
		{"GET /jobs/new", u.authed(u.jobNew)},
		{"POST /jobs", u.authed(u.jobCreate)},
		{"GET /jobs/{id}", u.authed(u.jobPage)},
		{"GET /jobs/{id}/edit", u.authed(u.jobEdit)},
		{"POST /jobs/{id}", u.authed(u.jobSave)},
		{"POST /jobs/{id}/transition", u.authed(u.jobTransition)},
		{"POST /jobs/{id}/approval", u.authed(u.jobApproval)},
		{"GET /applications/{id}", u.authed(u.appPage)},
		{"POST /applications/{id}/move", u.authed(u.appMove)},
		{"POST /applications/{id}/reject", u.authed(u.appReject)},
		{"POST /applications/{id}/hire", u.authed(u.appHire)},
		{"POST /applications/{id}/interviewers", u.authed(u.appAssign)},
		{"POST /applications/{id}/interviewers/remove", u.authed(u.appUnassign)},
		{"GET /applications/{id}/resume", u.authed(u.appResume)},
		{"POST /applications/{id}/reveal", u.authed(u.appReveal)},
		{"POST /applications/{id}/notes", u.authed(u.appNotes)},
		{"POST /applications/{id}/scorecard", u.authed(u.appScorecard)},
		{"GET /candidates/{id}", u.authed(u.candidatePage)},
		{"POST /candidates/{id}/tags", u.authed(u.candidateTags)},
		{"POST /candidates/{id}/talent-pool", u.authed(u.candidatePool)},
		{"GET /interviews", u.authed(u.interviews)},
		{"GET /talent", u.authed(u.talent)},
		{"GET /reports/eeo", u.authed(u.eeoReport)},
		{"GET /settings", u.authed(u.settings)},
		{"GET /static/{file}", u.static},
		{"GET /manifest.webmanifest", u.manifest},
	}
}

// UIRoutes lists the "METHOD /path" patterns the UI serves; the contract test compares them with
// specs/027-jobs-ui/contracts/ui-routes.md.
func UIRoutes() []string {
	var out []string
	for _, r := range (&UI{}).routes() {
		out = append(out, r.pattern)
	}
	return out
}

func (u *UI) init() {
	if u.Now == nil {
		u.Now = time.Now
	}
	if u.Log == nil {
		u.Log = slog.Default()
	}
	if u.IdleTimeout <= 0 {
		u.IdleTimeout = defaultIdle
	}
	if u.AbsoluteTimeout <= 0 {
		u.AbsoluteTimeout = defaultAbsolute
	}
	if u.RecheckEvery <= 0 {
		u.RecheckEvery = defaultRecheck
	}
	if u.Timeout <= 0 {
		u.Timeout = 8 * time.Second
	}
	u.failIP = newRateLimiter(failedLoginBurst, time.Minute, 10000, u.Now)
	u.perUser = newRateLimiter(600, time.Minute, 10000, u.Now)
	u.reveals = newRateLimiter(10, time.Minute, 10000, u.Now)
	funcs := template.FuncMap{
		"t":     u.msg,
		"date":  fmtDate,
		"when":  fmtWhen,
		"day":   fmtDay,
		"money": fmtMoney,
		"human": human,
	}
	u.tpl = &lazytmpl.Set{FS: uiAssets, Dir: "ui/templates", Funcs: funcs}
}

// Handler returns the UI handler.
func (u *UI) Handler() http.Handler {
	u.once.Do(u.init)
	mux := http.NewServeMux()
	for _, r := range u.routes() {
		mux.HandleFunc(r.pattern, r.h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ss, p, ok := u.current(w, r); ok {
			u.renderError(w, r, ss, p, http.StatusNotFound, "err.not_found_title", "err.not_found")
			return
		}
		u.renderError(w, r, nil, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setUIHeaders(w)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				u.Log.Error("panic in ui handler", "panic", p)
				if rec.status == http.StatusOK {
					u.renderError(rec, r, nil, nil, http.StatusInternalServerError, "err.internal_title", "err.internal")
				}
			}
			// The query string is left out: it can hold names.
			u.Log.Info("ui request", "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				u.renderError(rec, r, nil, nil, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
			r.Body = http.MaxBytesReader(rec, r.Body, 128<<10)
		}
		mux.ServeHTTP(rec, r)
	})
}

// setUIHeaders are the headers of every response. There is no script of any kind, so scripts are
// forbidden outright, and no data: URL is allowed anywhere. No ETag or Last-Modified is ever set:
// authenticated pages are never cached or revalidated.
func setUIHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cache-Control", "no-store")
}

// uiCtx is what a handler gets: the session, the caller and the request.
type uiCtx struct {
	ss *uiSession
	p  *Principal
	r  *http.Request
	w  http.ResponseWriter
}

func (c *uiCtx) ctx() context.Context { return c.r.Context() }

type uiHandler func(c *uiCtx) error

// authed wraps a handler: it needs a live session, the shared per-subject limit, the CSRF token on
// unsafe methods and a bounded request context, and turns Store errors into pages.
func (u *UI) authed(fn uiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ss, p, ok := u.current(w, r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if ok, _ := u.perUser.allow(ss.subject); !ok {
			w.Header().Set("Retry-After", "5")
			u.renderError(w, r, ss, p, http.StatusTooManyRequests, "err.busy_title", "err.busy")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := r.ParseForm(); err != nil || !constEq(r.PostForm.Get("csrf"), ss.csrf) {
				u.renderError(w, r, ss, p, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
		}
		ctx, cancel := context.WithTimeout(MetaOnly(r.Context(), true), u.Timeout)
		defer cancel()
		r = r.WithContext(ctx)
		if err := fn(&uiCtx{ss: ss, p: p, r: r, w: w}); err != nil {
			u.fail(w, r, ss, p, err)
		}
	}
}

func (u *UI) fail(w http.ResponseWriter, r *http.Request, ss *uiSession, p *Principal, err error) {
	var inv *InvalidError
	switch {
	case errors.As(err, &inv):
		u.renderMsg(w, r, ss, p, http.StatusBadRequest, "err.invalid_title", inv.Error())
	case errors.Is(err, ErrNotFound):
		u.renderError(w, r, ss, p, http.StatusNotFound, "err.not_found_title", "err.not_found")
	case errors.Is(err, ErrForbidden):
		u.renderError(w, r, ss, p, http.StatusForbidden, "err.forbidden_title", "err.forbidden")
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "database is locked"):
		w.Header().Set("Retry-After", "5")
		u.renderError(w, r, ss, p, http.StatusServiceUnavailable, "err.unavailable_title", "err.unavailable")
	case errors.Is(err, ErrVersionMismatch):
		u.renderError(w, r, ss, p, http.StatusConflict, "err.conflict_title", "err.version")
	case errors.Is(err, ErrConflict):
		u.renderMsg(w, r, ss, p, http.StatusConflict, "err.conflict_title", err.Error())
	default:
		u.Log.Error("ui internal error", "error", err)
		u.renderError(w, r, ss, p, http.StatusInternalServerError, "err.internal_title", "err.internal")
	}
}

type navItem struct {
	Href, Label string
	Current     bool
}

// page is the data every template receives.
type uiPage struct {
	Lang    string
	Title   string
	Nav     []navItem
	CSRF    string
	Authed  bool
	Flash   string
	Subject string
	Role    string
	Caps    Caps
	Data    any
}

func (u *UI) nav(path string, c Caps) []navItem {
	var items []struct{ href, key string }
	items = append(items, struct{ href, key string }{"/", "nav.home"})
	if c.ReadJobs {
		items = append(items, struct{ href, key string }{"/jobs", "nav.jobs"})
	}
	if c.Score {
		items = append(items, struct{ href, key string }{"/interviews", "nav.interviews"})
	}
	if c.TalentPool {
		items = append(items, struct{ href, key string }{"/talent", "nav.talent"})
	}
	if c.Reports {
		items = append(items, struct{ href, key string }{"/reports/eeo", "nav.eeo"})
	}
	if c.ReadJobs {
		items = append(items, struct{ href, key string }{"/settings", "nav.settings"})
	}
	out := make([]navItem, 0, len(items))
	for _, it := range items {
		cur := path == it.href || (it.href != "/" && strings.HasPrefix(path, it.href+"/"))
		out = append(out, navItem{Href: it.href, Label: u.msg(it.key), Current: cur})
	}
	return out
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, name, titleKey string, data any) {
	pg := uiPage{Lang: "en", Title: u.msg(titleKey), Data: data}
	if c != nil {
		pg.Authed, pg.CSRF = true, c.ss.csrf
		pg.Caps = c.p.Caps()
		pg.Subject, pg.Role = c.p.Subject, c.p.Role
		if c.ss.ssoHandle != "" { // a launch-ticket session shows the person's name, not the account id
			if n := u.sessions.displayName(c.ss); n != "" {
				pg.Subject = n
			}
		}
		if status < 400 {
			pg.Nav = u.nav(r.URL.Path, pg.Caps)
		} else {
			pg.Nav = u.nav("", pg.Caps) // an error page must not show which section the path belongs to
		}
		pg.Flash = flashMsg(u, r.URL.Query().Get("ok"))
	}
	t, err := u.tpl.Get(name)
	if err != nil {
		panic("jobs ui: template " + name + ": " + err.Error())
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", pg); err != nil {
		u.Log.Error("ui render", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type errData struct {
	Heading, Message, RequestID string
	Authed                      bool
}

func newRequestID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (u *UI) renderError(w http.ResponseWriter, r *http.Request, ss *uiSession, p *Principal, status int, titleKey, msgKey string) {
	u.renderMsg(w, r, ss, p, status, titleKey, u.msg(msgKey))
}

func (u *UI) renderMsg(w http.ResponseWriter, r *http.Request, ss *uiSession, p *Principal, status int, titleKey, msg string) {
	u.once.Do(u.init)
	var c *uiCtx
	if ss != nil && p != nil {
		c = &uiCtx{ss: ss, p: p, r: r, w: w}
	}
	u.render(w, r, c, status, "error.html", titleKey, errData{Heading: u.msg(titleKey), Message: msg, RequestID: newRequestID(), Authed: c != nil})
}

// ---- login and logout ----------------------------------------------------------------------

type loginData struct{ Error string }

// loginCSRF sets (or keeps) the double-submit cookie of the login forms and returns its value. The
// token is kept while it is valid, so a failed attempt does not invalidate the form the user is
// looking at.
func (u *UI) loginCSRF(w http.ResponseWriter, r *http.Request) string {
	tok := ""
	if ck, err := r.Cookie(u.loginCSRFName()); err == nil && len(ck.Value) == 43 {
		tok = ck.Value
	}
	if tok == "" {
		tok = newToken()
	}
	u.setCookie(w, u.loginCSRFName(), tok, loginCSRFMaxAge)
	return tok
}

func (u *UI) loginForm(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	tok := u.loginCSRF(w, r)
	u.render(w, r, nil, status, "login.html", "login.title", struct {
		loginData
		CSRF       string
		TokenLogin bool
	}{loginData{errMsg}, tok, !u.NoTokenLogin})
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.current(w, r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.loginForm(w, r, http.StatusOK, "")
}

func (u *UI) loginPost(w http.ResponseWriter, r *http.Request) {
	if u.NoTokenLogin { // ui.token_login is false: launch tickets only; nothing is checked or counted
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.token_off"))
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, nil, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	ck, err := r.Cookie(u.loginCSRFName())
	if err != nil || !constEq(ck.Value, r.PostForm.Get("csrf")) {
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.csrf"))
		return
	}
	// Valid credentials are never throttled: authenticate first, and only a failure draws on the
	// per-address budget, so a flood of bad tokens cannot lock out someone with a good one. An
	// unknown, revoked and malformed token get the same answer.
	token := strings.TrimSpace(r.PostForm.Get("token"))
	p, err := u.authenticateToken(r, token)
	if err != nil {
		if ok, _ := u.failIP.allow(clientIP(r, u.TrustProxy)); !ok {
			w.Header().Set("Retry-After", "10")
			u.loginForm(w, r, http.StatusTooManyRequests, u.msg("login.locked"))
			return
		}
		u.loginForm(w, r, http.StatusUnauthorized, u.msg("login.invalid"))
		return
	}
	// A presented session id is never adopted (session fixation); the old one, if any, is dropped.
	if old, err := r.Cookie(u.sessionCookieName()); err == nil {
		u.sessions.drop(old.Value)
	}
	id, _ := u.sessions.create(*p, token, u.Now())
	u.setCookie(w, u.sessionCookieName(), id, 0)
	u.setCookie(w, u.loginCSRFName(), "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (u *UI) logout(c *uiCtx) error {
	if ck, err := c.r.Cookie(u.sessionCookieName()); err == nil {
		u.sessions.drop(ck.Value)
	}
	u.setCookie(c.w, u.sessionCookieName(), "", -1)
	http.Redirect(c.w, c.r, "/login", http.StatusSeeOther)
	return nil
}

// ---- static assets -------------------------------------------------------------------------

func (u *UI) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		u.renderError(w, r, nil, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	b, err := uiAssets.ReadFile("ui/static/" + name)
	if err != nil {
		u.renderError(w, r, nil, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	ct := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".css"):
		ct = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		ct = "image/svg+xml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

func (u *UI) manifest(w http.ResponseWriter, _ *http.Request) {
	b, _ := uiAssets.ReadFile("ui/static/manifest.webmanifest")
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// ---- small helpers -------------------------------------------------------------------------

func fmtDate(s string) string {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format("2 Jan 2006")
	}
	return s
}

func fmtWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("Mon 2 Jan 2006 15:04 UTC")
}

func fmtDay(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2 Jan 2006")
}

func fmtMoney(v float64) string { return fmt.Sprintf("%.0f", v) }

func withOK(path, key string) string { return path + "?ok=" + url.QueryEscape(key) }

// human turns a vocabulary value such as prefer_not_to_say into "Prefer not to say".
func human(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

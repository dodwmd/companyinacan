package equity

// The server-rendered web UI of the equity module (epic E17g, specs/066-equity-ui). html/template,
// an embedded stylesheet, no JavaScript. It is a client of the Service exactly like the JSON API:
// every page calls Service methods, which ask the policy layer, so there is no second
// authorisation path. These files decide presentation only and hold no authorisation and no SQL.

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/lazytmpl"
	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

//go:embed ui/templates ui/static
var uiAssets embed.FS

const (
	uiFormLimit = 64 << 10
	uiDeadline  = 45 * time.Second
)

// UI serves the web UI.
type UI struct {
	Svc  *Service
	Auth Authenticator // the same chain the API uses; it validates the token presented at login
	Log  *slog.Logger
	// Secure sets the Secure attribute (and the __Host- cookie prefix). False only for plain-HTTP
	// local development.
	Secure bool
	// TrustProxy takes the client address for throttling failed logins from X-Forwarded-For.
	TrustProxy bool
	Now        func() time.Time

	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	RecheckEvery    time.Duration

	// Sessions is can-core's launch-ticket API (single sign-on from the admin UI's Apps page).
	// Nil: the ticket routes answer 404 and only pasted tokens sign in.
	Sessions modclient.SessionAPI
	// NoTokenLogin switches the "paste a personal access token" sign-in off (ui.token_login: false).
	NoTokenLogin bool

	once     sync.Once
	sessions *session.Manager[Principal]
	tpl      *lazytmpl.Set // pages are parsed on first use
	failIP   *rateLimiter
	perUser  *rateLimiter
}

// withUI serves the web UI beside the API: /v1 and the health check stay the API's.
func withUI(api, ui http.Handler) http.Handler {
	top := http.NewServeMux()
	top.Handle("/v1/", api)
	top.Handle("GET /healthz", api)
	top.Handle("/", ui)
	return top
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

		// the holder's own equity
		{"GET /me", u.authed(u.myEquity)},
		{"GET /me/documents", u.authed(u.myDocuments)},
		{"GET /me/grants/{id}", u.authed(u.myGrantPage)},
		{"GET /me/grants/{id}/statement", u.authed(u.myStatement)},
		{"POST /me/grants/{id}/exercise/review", u.authed(u.myExerciseReview)},
		{"POST /me/grants/{id}/exercise", u.authed(u.myExerciseRequest)},
		{"POST /me/exercises/{id}/cancel", u.authed(u.myExerciseCancel)},

		// administration and finance
		{"GET /company", u.authed(u.companies)},
		{"GET /company/entities/{id}", u.authed(u.company)},
		{"GET /company/entities/{id}/captable", u.authed(u.capTable)},
		{"GET /company/entities/{id}/exports", u.authed(u.exportsPage)},
		{"POST /company/entities/{id}/exports", u.authed(u.exportDownload)},
		{"GET /company/entities/{id}/stakeholders", u.authed(u.stakeholders)},
		{"POST /company/entities/{id}/stakeholders", u.authed(u.stakeholderCreate)},
		{"GET /company/stakeholders/{id}", u.authed(u.stakeholderPage)},
		{"POST /company/stakeholders/{id}/verify", u.authed(u.stakeholderVerify)},
		{"POST /company/stakeholders/{id}/attest", u.authed(u.stakeholderAttest)},
		{"GET /company/identity", u.authed(u.identityQueue)},
		{"GET /company/entities/{id}/grants", u.authed(u.grants)},
		{"GET /company/entities/{id}/grants/new", u.authed(u.grantNew)},
		{"POST /company/entities/{id}/grants", u.authed(u.grantCreate)},
		{"GET /company/grants/{id}", u.authed(u.grantPage)},
		{"POST /company/grants/{id}/submit", u.authed(u.grantSubmit)},
		{"POST /company/grants/{id}/delete", u.authed(u.grantDelete)},
		{"POST /company/grants/{id}/approve", u.authed(u.grantApprove)},
		{"POST /company/grants/{id}/reject", u.authed(u.grantReject)},
		{"POST /company/grants/{id}/activate", u.authed(u.grantActivate)},
		{"GET /company/entities/{id}/exercises", u.authed(u.exercises)},
		{"GET /company/exercises/{id}", u.authed(u.exercisePage)},
		{"POST /company/exercises/{id}/approve", u.authed(u.exerciseApprove)},
		{"POST /company/exercises/{id}/reject", u.authed(u.exerciseReject)},
		{"POST /company/exercises/{id}/cancel", u.authed(u.exerciseCancel)},
		{"POST /company/exercises/{id}/payment", u.authed(u.exercisePayment)},
		{"POST /company/exercises/{id}/issue", u.authed(u.exerciseIssue)},

		{"GET /static/{file}", u.static},
		{"GET /manifest.webmanifest", u.manifest},
	}
}

// UIRoutes lists the "METHOD /path" patterns the UI serves; a test compares them with
// specs/066-equity-ui/contracts/ui-routes.md.
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
		u.IdleTimeout = session.DefaultIdle
	}
	if u.AbsoluteTimeout <= 0 {
		u.AbsoluteTimeout = session.DefaultAbsolute
	}
	if u.RecheckEvery <= 0 {
		u.RecheckEvery = session.DefaultRecheck
	}
	u.sessions = u.newSessions()
	u.failIP = newRateLimiter(10, 0.2, 10000, nil)
	u.perUser = newRateLimiter(120, 20, 10000, nil)
	funcs := template.FuncMap{
		"t":      u.msg,
		"date":   fmtDate,
		"qty":    fmtQty,
		"money":  fmtMoney,
		"amount": FormatMinor,
		"pct":    fmtPPM,
		"label":  u.label,
		"dict2":  func(g Grant, schedule []string) termsData { return termsData{g, schedule} },
	}
	u.tpl = &lazytmpl.Set{FS: uiAssets, Dir: "ui/templates", Funcs: funcs}
}

// termsData is what the "terms" partial shows.
type termsData struct {
	Grant    Grant
	Schedule []string
}

// Handler returns the UI handler.
func (u *UI) Handler() http.Handler {
	u.once.Do(u.init)
	mux := http.NewServeMux()
	for _, r := range u.routes() {
		mux.HandleFunc(r.pattern, r.h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if c, ok := u.current(w, r); ok {
			u.renderError(w, r, c, http.StatusNotFound, "err.not_found_title", u.msg("err.not_found"))
			return
		}
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", u.msg("err.not_found"))
	})
	return withDeadlineFor(uiDeadline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session.SetHeaders(w.Header())
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				u.Log.Error("panic in ui handler", "panic", p)
				u.renderError(rec, r, nil, http.StatusInternalServerError, "err.internal_title", u.msg("err.internal"))
			}
			// The path and the query string are left out: they can hold ids.
			u.Log.Info("ui request", "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !session.SameOrigin(r) {
				u.renderError(rec, r, nil, http.StatusForbidden, "err.csrf_title", u.msg("err.csrf"))
				return
			}
			r.Body = http.MaxBytesReader(rec, r.Body, uiFormLimit)
		}
		mux.ServeHTTP(rec, r)
	}))
}

func withDeadlineFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// uiCtx is what a handler gets: the session, the resolved caller and the request.
type uiCtx struct {
	ss   *uiSession
	a    Actor
	caps Capabilities
	r    *http.Request
	w    http.ResponseWriter
}

func (c *uiCtx) ctx() context.Context  { return c.r.Context() }
func (c *uiCtx) id() string            { return c.r.PathValue("id") }
func (c *uiCtx) form(n string) string  { return strings.TrimSpace(c.r.PostForm.Get(n)) }
func (c *uiCtx) query(n string) string { return strings.TrimSpace(c.r.URL.Query().Get(n)) }
func (c *uiCtx) redirect(to string)    { http.Redirect(c.w, c.r, to, http.StatusSeeOther) }
func (c *uiCtx) done(to, flash string) { c.redirect(to + "?ok=" + url.QueryEscape(flash)) }
func (c *uiCtx) after() int64          { return qint(c.r, "after") }

type uiHandler func(c *uiCtx) error

// authed wraps a handler: it needs a live session, the per-subject limit, the CSRF token on unsafe
// methods, and turns Service errors into pages.
func (u *UI) authed(fn uiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := u.current(w, r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !u.perUser.allow(c.ss.Subject) {
			w.Header().Set("Retry-After", "5")
			u.renderError(w, r, c, http.StatusTooManyRequests, "err.busy_title", u.msg("err.busy"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := r.ParseForm(); err != nil || !session.Equal(r.PostForm.Get("csrf"), c.ss.CSRF) {
				u.renderError(w, r, c, http.StatusForbidden, "err.csrf_title", u.msg("err.csrf"))
				return
			}
		}
		if err := fn(c); err != nil {
			status, title, msg := u.explain(r, err)
			u.renderError(w, r, c, status, title, msg)
		}
	}
}

// uiError is a refusal of the UI itself (an unconfirmed form, a failed step-up).
type uiError struct {
	status int
	key    string
}

func (e *uiError) Error() string { return e.key }

// explain turns an error into the status, heading and message of a page. The messages of the
// Service's refusals are its own words, as the JSON API gives them.
func (u *UI) explain(r *http.Request, err error) (int, string, string) {
	var inv *InvalidError
	var ue *uiError
	switch {
	case errors.As(err, &ue):
		return ue.status, "err.stepup_title", u.msg(ue.key)
	case errors.As(err, &inv):
		return http.StatusBadRequest, "err.invalid_title", inv.Msg
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "err.not_found_title", u.msg("err.not_found")
	case errors.Is(err, ErrIdentityRequired):
		return http.StatusForbidden, "err.identity_title", u.msg("err.identity")
	case errors.Is(err, ErrIdentityUnavailable):
		return http.StatusServiceUnavailable, "err.unavailable_title", u.msg("err.directory")
	case errors.Is(err, ErrSeparation):
		return http.StatusForbidden, "err.separation_title", u.msg("err.separation")
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "err.forbidden_title", u.msg("err.forbidden")
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "err.conflict_title", err.Error()
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "database is locked"):
		return http.StatusServiceUnavailable, "err.unavailable_title", u.msg("err.unavailable")
	}
	u.Log.Error("ui internal error", "route", r.Pattern, "error", err)
	return http.StatusInternalServerError, "err.internal_title", u.msg("err.internal")
}

type navItem struct {
	Href, Label string
	Current     bool
}

// page is the data every template receives.
type page struct {
	Lang   string
	Title  string
	Nav    []navItem
	CSRF   string
	Authed bool
	SSO    bool
	Caps   Capabilities
	Flash  string
	Data   any
}

func (u *UI) nav(caps Capabilities, path string) []navItem {
	items := []struct {
		show      bool
		href, key string
	}{
		{true, "/", "nav.home"},
		{caps.Self, "/me", "nav.mine"},
		{caps.Self, "/me/documents", "nav.documents"},
		{caps.Structure, "/company", "nav.company"},
		{caps.Stakeholders, "/company/identity", "nav.identity"},
	}
	var out []navItem
	best := -1
	for _, it := range items {
		if !it.show {
			continue
		}
		out = append(out, navItem{Href: it.href, Label: u.msg(it.key)})
		if path == it.href || (it.href != "/" && strings.HasPrefix(path, it.href+"/")) {
			if best < 0 || len(it.href) > len(out[best].Href) {
				best = len(out) - 1
			}
		}
	}
	if best >= 0 {
		out[best].Current = true
	}
	return out
}

// view renders a page for a signed-in caller.
func (u *UI) view(c *uiCtx, name, titleKey string, data any) error {
	u.render(c.w, c.r, c, http.StatusOK, name, titleKey, data, "")
	return nil
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, name, titleKey string, data any, flash string) {
	pg := page{Lang: "en", Title: u.msg(titleKey), Data: data}
	if c != nil {
		pg.Authed, pg.CSRF, pg.SSO, pg.Caps = true, c.ss.CSRF, c.ss.SSO(), c.caps
		path := ""
		if status < 400 {
			path = r.URL.Path // an error page must not show which section the path belongs to
		}
		pg.Nav = u.nav(c.caps, path)
		if flash == "" && status < 400 {
			flash = r.URL.Query().Get("ok")
		}
		if k := flash; strings.HasPrefix(k, "flash.") {
			if v, ok := uiMessagesEN[k]; ok {
				pg.Flash = v
			}
		}
	}
	t, err := u.tpl.Get(name)
	if err != nil {
		panic("equity ui: template " + name + ": " + err.Error())
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

func (u *UI) renderError(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, titleKey, msg string) {
	u.render(w, r, c, status, "error.html", titleKey, errData{Heading: u.msg(titleKey), Message: msg, RequestID: newRequestID(), Authed: c != nil}, "")
}

// ---- login and logout ----------------------------------------------------------------------

func (u *UI) clientHost(r *http.Request) string {
	// Behind a proxy the last entry of the last X-Forwarded-For line is the one the proxy added.
	if vs := r.Header.Values("X-Forwarded-For"); u.TrustProxy && len(vs) > 0 {
		parts := strings.Split(vs[len(vs)-1], ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
			return ip
		}
	}
	return clientHost(r)
}

type loginData struct {
	CSRF, Error string
	// TokenLogin is whether the paste-a-token form is offered (ui.token_login).
	TokenLogin bool
}

func (u *UI) loginForm(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	tok := u.sessions.LoginCSRF(w, r)
	u.render(w, r, nil, status, "login.html", "login.title", loginData{CSRF: tok, Error: errMsg, TokenLogin: !u.NoTokenLogin}, "")
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.current(w, r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.loginForm(w, r, http.StatusOK, "")
}

func (u *UI) loginPost(w http.ResponseWriter, r *http.Request) {
	modclient.LimitPublicForm(w, r) // anyone can post here
	if u.NoTokenLogin {             // pasted-token sign-in is off (ui.token_login: false): launch tickets only
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.token_off"))
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, http.StatusBadRequest, "err.invalid_title", u.msg("err.bad_form"))
		return
	}
	if !u.sessions.CheckLoginCSRF(r) {
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.csrf"))
		return
	}
	// Valid credentials are never throttled: authenticate first, and only a failure draws on the
	// per-address budget, so a flood of bad tokens cannot lock out someone with a good one.
	token := strings.TrimSpace(r.PostForm.Get("token"))
	p, err := u.authenticateToken(r, token)
	if err != nil {
		if !u.failIP.allow(u.clientHost(r)) {
			w.Header().Set("Retry-After", "10")
			u.loginForm(w, r, http.StatusTooManyRequests, u.msg("login.locked"))
			return
		}
		u.loginForm(w, r, http.StatusUnauthorized, u.msg("login.invalid"))
		return
	}
	u.sessions.Start(w, r, p, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (u *UI) logout(c *uiCtx) error {
	u.sessions.End(c.w, c.r)
	c.redirect("/login")
	return nil
}

// ---- static assets -------------------------------------------------------------------------

func (u *UI) static(w http.ResponseWriter, r *http.Request) {
	types := map[string]string{"equity.css": "text/css; charset=utf-8", "icon.svg": "image/svg+xml"}
	name := r.PathValue("file")
	ct, ok := types[name]
	b, err := uiAssets.ReadFile("ui/static/" + name)
	if !ok || err != nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", u.msg("err.not_found"))
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

func (u *UI) manifest(w http.ResponseWriter, r *http.Request) {
	b, _ := uiAssets.ReadFile("ui/static/manifest.webmanifest")
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

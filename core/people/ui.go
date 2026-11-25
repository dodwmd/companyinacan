package people

// The server-rendered self-service web UI of the people module (epic E13c, specs/026-people-ui).
// html/template, an embedded stylesheet, no JavaScript. It is a client of the Service exactly like
// the JSON API: every page calls the same Service methods, which ask the policy layer, so there is
// no second authorisation path. The UI decides only presentation.

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

//go:embed ui/templates ui/static
var uiAssets embed.FS

// ApprovalItem is one approval waiting on the signed-in user.
type ApprovalItem struct {
	Title string
	URL   string
}

// ApprovalSource is the can-core approvals seam. It stays an interface: with no implementation
// wired, the home page shows no approvals section at all.
type ApprovalSource interface {
	Waiting(ctx context.Context, subject string) ([]ApprovalItem, error)
}

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

	// Sessions is can-core's launch-ticket API (single sign-on from the admin UI's Apps page). Nil:
	// the ticket routes answer 404 and only pasted tokens sign in.
	Sessions modclient.SessionAPI
	// NoTokenLogin switches the "paste a personal access token" sign-in off (ui.token_login: false).
	NoTokenLogin bool

	Approvals        ApprovalSource // nil: no approvals section
	NotificationsURL string         // empty: no notifications link

	once     sync.Once
	sessions sessionStore
	tpl      map[string]*template.Template
	failIP   *rateLimiter
	perUser  *rateLimiter
	reveals  *rateLimiter // reveals of other people's sensitive data, per subject
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
		{"POST /tasks/{id}/complete", u.authed(u.completeTask)},
		{"GET /me", u.authed(u.profile)},
		{"POST /me", u.authed(u.profileSave)},
		{"GET /me/sensitive", u.authed(u.sensitiveNotice)},
		{"POST /me/sensitive", u.authed(u.sensitiveReveal)},
		{"GET /people", u.authed(u.directory)},
		{"GET /people/{id}", u.authed(u.person)},
		{"GET /people/{id}/sensitive", u.authed(u.sensitiveNotice)},
		{"POST /people/{id}/sensitive", u.authed(u.sensitiveReveal)},
		{"GET /org", u.authed(u.org)},
		{"GET /org/{id}", u.authed(u.org)},
		{"GET /team", u.authed(u.team)},
		{"GET /oneonones", u.authed(u.seriesList)},
		{"POST /oneonones", u.authed(u.seriesCreate)},
		{"GET /oneonones/{id}", u.authed(u.seriesPage)},
		{"POST /oneonones/{id}/meetings", u.authed(u.meetingSchedule)},
		{"GET /meetings/{id}", u.authed(u.meetingPage)},
		{"POST /meetings/{id}/agenda", u.authed(u.meetingAgenda)},
		{"POST /meetings/{id}/notes", u.authed(u.meetingNotes(false))},
		{"POST /meetings/{id}/private-notes", u.authed(u.meetingNotes(true))},
		{"POST /meetings/{id}/actions", u.authed(u.meetingAction)},
		{"POST /meetings/{id}/status", u.authed(u.meetingStatus)},
		{"POST /actions/{id}/status", u.authed(u.actionStatus)},
		{"GET /goals", u.authed(u.goals)},
		{"POST /goals", u.authed(u.goalCreate)},
		{"GET /goals/{id}", u.authed(u.goal)},
		{"POST /goals/{id}", u.authed(u.goalUpdate)},
		{"POST /goals/{id}/key-results", u.authed(u.krCreate)},
		{"POST /key-results/{id}/progress", u.authed(u.krProgress)},
		{"GET /timeoff", u.authed(u.timeoff)},
		{"POST /timeoff", u.authed(u.timeoffCreate)},
		{"GET /timeoff/queue", u.authed(u.timeoffQueue)},
		{"GET /timeoff/calendar", u.authed(u.calendar)},
		{"GET /timeoff/{id}", u.authed(u.timeoffDetail)},
		{"POST /timeoff/{id}/submit", u.authed(u.timeoffSubmit)},
		{"POST /timeoff/{id}/cancel", u.authed(u.timeoffCancel)},
		{"POST /timeoff/{id}/decide", u.authed(u.timeoffDecide)},
		{"GET /reviews", u.authed(u.reviews)},
		{"GET /reviews/{id}", u.authed(u.review)},
		{"POST /reviews/{id}/self", u.authed(u.reviewSelf)},
		{"POST /reviews/{id}/manager", u.authed(u.reviewManager)},
		{"POST /reviews/{id}/nominations", u.authed(u.nominate)},
		{"POST /reviews/{id}/acknowledge", u.authed(u.reviewAck)},
		{"POST /nominations/{id}/decide", u.authed(u.nominationDecide)},
		{"GET /feedback", u.authed(u.feedbackInbox)},
		{"GET /feedback/{id}", u.authed(u.feedbackForm)},
		{"POST /feedback/{id}", u.authed(u.feedbackSubmit)},
		{"GET /static/{file}", u.static},
		{"GET /manifest.webmanifest", u.manifest},
	}
}

// UIRoutes lists the "METHOD /path" patterns the UI serves; the contract test compares them with
// specs/026-people-ui/contracts/ui-routes.md.
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
	u.failIP = newRateLimiter(failedLoginBurst, 0.2, 10000, nil)
	u.perUser = newRateLimiter(300, 30, 10000, nil)
	u.reveals = newRateLimiter(10, 10.0/60, 10000, nil)
	funcs := template.FuncMap{
		"t":    u.msg,
		"date": func(s string) string { return fmtDate(s) },
		"when": func(s string) string { return fmtWhen(s) },
		"pair": func(p page, items []actionRow) map[string]any { return map[string]any{"P": p, "Items": items} },
	}
	u.tpl = map[string]*template.Template{}
	entries, err := fs.ReadDir(uiAssets, "ui/templates")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		if e.Name() == "layout.html" || e.Name() == "partials.html" {
			continue
		}
		t, err := template.New(e.Name()).Funcs(funcs).ParseFS(uiAssets, "ui/templates/layout.html", "ui/templates/partials.html", "ui/templates/"+e.Name())
		if err != nil {
			panic("people ui: " + err.Error())
		}
		u.tpl[e.Name()] = t
	}
}

// Handler returns the UI handler.
func (u *UI) Handler() http.Handler {
	u.once.Do(u.init)
	mux := http.NewServeMux()
	for _, r := range u.routes() {
		mux.HandleFunc(r.pattern, r.h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ss, a, ok := u.current(w, r); ok {
			u.renderError(w, r, ss, a, http.StatusNotFound, "err.not_found_title", "err.not_found")
			return
		}
		u.renderError(w, r, nil, Actor{}, http.StatusNotFound, "err.not_found_title", "err.not_found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setUIHeaders(w)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				u.Log.Error("panic in ui handler", "panic", p)
				if rec.status == http.StatusOK {
					u.renderError(rec, r, nil, Actor{}, http.StatusInternalServerError, "err.internal_title", "err.internal")
				}
			}
			// The query string is left out: it can hold names and ids.
			u.Log.Info("ui request", "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				u.renderError(rec, r, nil, Actor{}, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
			r.Body = http.MaxBytesReader(rec, r.Body, 64<<10)
		}
		mux.ServeHTTP(rec, r)
	})
}

// setUIHeaders are the headers of every response. There is no script of any kind, so scripts are
// forbidden outright. No ETag or Last-Modified is ever set: authenticated pages are never cached
// or revalidated (the anonymity-sensitive review pages in particular).
func setUIHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cache-Control", "no-store")
}

// uiCtx is what a handler gets: the session, the resolved caller and the request.
type uiCtx struct {
	ss *uiSession
	a  Actor
	r  *http.Request
	w  http.ResponseWriter
}

type uiHandler func(c *uiCtx) error

// authed wraps a handler: it needs a live session, the shared per-subject limit, the CSRF token on
// unsafe methods, and turns Service errors into pages.
func (u *UI) authed(fn uiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ss, a, ok := u.current(w, r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !u.perUser.allow(ss.subject) {
			w.Header().Set("Retry-After", "5")
			u.renderError(w, r, ss, a, http.StatusTooManyRequests, "err.busy_title", "err.busy")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := r.ParseForm(); err != nil || !constEq(r.PostForm.Get("csrf"), ss.csrf) {
				u.renderError(w, r, ss, a, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
		}
		if err := fn(&uiCtx{ss: ss, a: a, r: r, w: w}); err != nil {
			u.fail(w, r, ss, a, err)
		}
	}
}

func (u *UI) fail(w http.ResponseWriter, r *http.Request, ss *uiSession, a Actor, err error) {
	var inv *InvalidError
	switch {
	case errors.As(err, &inv):
		u.renderMsg(w, r, ss, a, http.StatusBadRequest, "err.invalid_title", inv.Msg)
	case errors.Is(err, ErrNotFound):
		u.renderError(w, r, ss, a, http.StatusNotFound, "err.not_found_title", "err.not_found")
	case errors.Is(err, ErrForbidden):
		u.renderError(w, r, ss, a, http.StatusForbidden, "err.forbidden_title", "err.forbidden")
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "database is locked"):
		w.Header().Set("Retry-After", "5")
		u.renderError(w, r, ss, a, http.StatusServiceUnavailable, "err.unavailable_title", "err.unavailable")
	case errors.Is(err, ErrConflict):
		u.renderMsg(w, r, ss, a, http.StatusConflict, "err.conflict_title", err.Error())
	default:
		u.Log.Error("ui internal error", "error", err)
		u.renderError(w, r, ss, a, http.StatusInternalServerError, "err.internal_title", "err.internal")
	}
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
	Flash  string
	Notif  string
	Data   any
}

func (u *UI) nav(path string) []navItem {
	items := []struct{ href, key string }{
		{"/", "nav.home"}, {"/me", "nav.profile"}, {"/people", "nav.directory"}, {"/org", "nav.org"},
		{"/team", "nav.team"}, {"/oneonones", "nav.oneonones"}, {"/goals", "nav.goals"},
		{"/timeoff", "nav.timeoff"}, {"/reviews", "nav.reviews"}, {"/feedback", "nav.feedback"},
	}
	out := make([]navItem, 0, len(items))
	for _, it := range items {
		cur := path == it.href || (it.href != "/" && strings.HasPrefix(path, it.href+"/"))
		out = append(out, navItem{Href: it.href, Label: u.msg(it.key), Current: cur})
	}
	return out
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, name, titleKey string, data any) {
	pg := page{Lang: "en", Title: u.msg(titleKey), Data: data}
	if c != nil {
		pg.Authed, pg.CSRF = true, c.ss.csrf
		if status < 400 {
			pg.Nav = u.nav(r.URL.Path)
		} else {
			pg.Nav = u.nav("") // an error page must not show which section the path belongs to
		}
		pg.Notif = u.NotificationsURL
		pg.Flash = flashMsg(u, r.URL.Query().Get("ok"))
	}
	t := u.tpl[name]
	if t == nil {
		panic("people ui: no template " + name)
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

func (u *UI) renderError(w http.ResponseWriter, r *http.Request, ss *uiSession, a Actor, status int, titleKey, msgKey string) {
	u.renderMsg(w, r, ss, a, status, titleKey, u.msg(msgKey))
}

func (u *UI) renderMsg(w http.ResponseWriter, r *http.Request, ss *uiSession, a Actor, status int, titleKey, msg string) {
	var c *uiCtx
	if ss != nil {
		c = &uiCtx{ss: ss, a: a, r: r, w: w}
	}
	u.render(w, r, c, status, "error.html", titleKey, errData{Heading: u.msg(titleKey), Message: msg, RequestID: newRequestID(), Authed: ss != nil})
}

// ---- login and logout ----------------------------------------------------------------------

func (u *UI) clientHost(r *http.Request) string {
	if u.TrustProxy {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			parts := strings.Split(xf, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type loginData struct {
	Error string
	// TokenLogin is whether the paste-a-token form is offered (ui.token_login).
	TokenLogin bool
}

func (u *UI) loginForm(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	// The double-submit token is kept while it is valid, so a failed attempt does not invalidate
	// the form the user is looking at.
	tok := ""
	if ck, err := r.Cookie(u.loginCSRFName()); err == nil && len(ck.Value) == 43 {
		tok = ck.Value
	}
	if tok == "" {
		tok = newToken()
	}
	u.setCookie(w, u.loginCSRFName(), tok, loginCSRFMaxAge)
	u.render(w, r, nil, status, "login.html", "login.title", struct {
		loginData
		CSRF string
	}{loginData{errMsg, !u.NoTokenLogin}, tok})
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.current(w, r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.loginForm(w, r, http.StatusOK, "")
}

func (u *UI) loginPost(w http.ResponseWriter, r *http.Request) {
	if u.NoTokenLogin { // pasted-token sign-in is off (ui.token_login: false): launch tickets only
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.token_off"))
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, Actor{}, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	ck, err := r.Cookie(u.loginCSRFName())
	if err != nil || !constEq(ck.Value, r.PostForm.Get("csrf")) {
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.csrf"))
		return
	}
	// Valid credentials are never throttled: authenticate first, and only a failure draws on the
	// per-address budget, so a flood of bad tokens cannot lock out someone with a good one.
	p, err := u.authenticateToken(r, strings.TrimSpace(r.PostForm.Get("token")))
	var a Actor
	if err == nil {
		a, err = u.Svc.ResolveActor(r.Context(), p)
		if err == nil && (!a.HasPerson || !a.Active()) {
			err = ErrUnauthenticated // a token with no active person behind it is no login
		}
	}
	if err != nil {
		if !u.failIP.allow(u.clientHost(r)) {
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
	id, _ := u.sessions.create(*p, strings.TrimSpace(r.PostForm.Get("token")), u.Now())
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
		u.renderError(w, r, nil, Actor{}, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	b, err := uiAssets.ReadFile("ui/static/" + name)
	if err != nil {
		u.renderError(w, r, nil, Actor{}, http.StatusNotFound, "err.not_found_title", "err.not_found")
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

func (u *UI) manifest(w http.ResponseWriter, r *http.Request) {
	b, _ := uiAssets.ReadFile("ui/static/manifest.webmanifest")
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// ---- small helpers -------------------------------------------------------------------------

func fmtDate(s string) string {
	if t, err := time.Parse(dateLayout, s); err == nil {
		return t.Format("2 Jan 2006")
	}
	return s
}

func fmtWhen(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("Mon 2 Jan 2006 15:04 UTC")
	}
	return s
}

func withOK(path, key string) string { return path + "?ok=" + url.QueryEscape(key) }

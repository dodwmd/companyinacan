package files

// The server-rendered web UI of the files module (epic E13f, specs/050-files-ui). html/template,
// an embedded stylesheet, no JavaScript. It is a client of the Store exactly like the JSON API:
// every page calls the same Store methods, which decide access by the caller's permissions and
// space role, audit what the API audits and refuse what the API refuses. There is no second
// authorisation path: the UI decides only presentation, plus the permission check per route that
// the API's guard makes (Principal.Has).

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/lazytmpl"
	"github.com/dodwmd/companyinacan/core/modclient"
)

//go:embed ui/templates ui/static
var uiAssets embed.FS

const (
	uiPerSubjectPerMin = 600
	uiMaxFormBytes     = 64 << 10
	// uiUploadOverhead is how much larger than the largest file a multipart upload request may be:
	// the boundaries and the few short text fields beside the file.
	uiUploadOverhead = 64 << 10
	uiPageSize       = 50
)

// UI serves the web UI.
type UI struct {
	Store  *Store
	Auth   Authenticator // the chain the API uses; it validates the token presented at login
	Groups GroupResolver // optional, as for the API
	Log    *slog.Logger
	// Secure sets the Secure attribute (and the __Host- cookie prefix). False only for plain-HTTP
	// local development.
	Secure bool
	// TrustProxy takes the client address for throttling failed logins from X-Forwarded-For.
	TrustProxy bool
	Now        func() time.Time
	// Sessions is can-core's launch-ticket API (single sign-on from the admin UI, specs/036-module-ui-sso).
	// Nil: /login/ticket does not exist.
	Sessions modclient.SessionAPI
	// NoTokenLogin switches off the pasted-token sign-in (ui.token_login: false).
	NoTokenLogin bool

	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	RecheckEvery    time.Duration

	once     sync.Once
	sessions sessionStore
	tpl      *lazytmpl.Set // pages parse on first use: parsing all of them costs RAM at idle
	failIP   *failLimiter
	perUser  *failLimiter
}

type uiRoute struct {
	pattern string
	perm    string // permission the route needs ("" = a session is enough)
	upload  bool   // a multipart/form-data post that is streamed, never parsed whole
	h       uiHandler
}

func (u *UI) routes() []uiRoute {
	r, w, a := PermRead, PermWrite, PermAdmin
	return []uiRoute{
		{"GET /{$}", r, false, u.home},
		{"POST /spaces", w, false, u.spaceCreate},
		{"GET /spaces/{id}", r, false, u.space},
		{"GET /spaces/{id}/members", r, false, u.members},
		{"POST /spaces/{id}/members", w, false, u.memberSet},
		{"POST /spaces/{id}/members/remove", w, false, u.memberRemove},
		{"POST /spaces/{id}/folders", w, false, u.folderCreate},
		{"POST /folders/{id}/delete", w, false, u.folderDelete},
		{"GET /spaces/{id}/upload", w, false, u.uploadForm},
		{"POST /spaces/{id}/files", w, true, u.upload},
		{"GET /spaces/{id}/trash", r, false, u.trash},

		{"GET /files/{id}", r, false, u.file},
		{"GET /files/{id}/content", r, false, u.download},
		{"POST /files/{id}/update", w, false, u.fileUpdate},
		{"POST /files/{id}/versions", w, true, u.versionUpload},
		{"POST /files/{id}/delete", w, false, u.fileDelete},
		{"POST /files/{id}/restore", w, false, u.fileRestore},
		{"POST /files/{id}/shares", w, false, u.shareCreate},
		{"POST /shares/{id}/revoke", w, false, u.shareRevoke},

		{"GET /admin/files", a, false, u.adminList},
		{"GET /admin/files/{id}/hold", a, false, u.holdForm},
		{"POST /admin/files/{id}/hold", a, false, u.holdSet},
		{"GET /admin/erasure", a, false, u.eraseForm},
		{"POST /admin/erasure", a, false, u.erasePost},
	}
}

// publicRoutes need no session.
func (u *UI) publicRoutes() []struct {
	pattern string
	h       http.HandlerFunc
} {
	return []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET /login", u.loginPage},
		{"POST /login", u.loginPost},
		{"GET /login/ticket", u.ticketPage},
		{"POST /login/ticket", u.ticketPost},
		{"GET /static/{file}", u.static},
		{"GET /manifest.webmanifest", u.manifest},
	}
}

// UIRoutes lists the "METHOD /path" patterns the UI serves, with the permission each needs; the
// contract test compares them with specs/050-files-ui/contracts/ui-routes.md.
func UIRoutes() (patterns []string, scope map[string]string) {
	u := &UI{}
	scope = map[string]string{}
	for _, p := range u.publicRoutes() {
		patterns = append(patterns, p.pattern)
		scope[p.pattern] = "-"
	}
	patterns = append(patterns, "POST /logout")
	scope["POST /logout"] = "session"
	for _, r := range u.routes() {
		patterns = append(patterns, r.pattern)
		scope[r.pattern] = strings.TrimPrefix(r.perm, "files.")
	}
	return patterns, scope
}

func (u *UI) init() {
	if u.Now == nil {
		u.Now = time.Now
	}
	if u.Log == nil {
		u.Log = slog.Default()
	}
	if u.IdleTimeout <= 0 {
		u.IdleTimeout = uiDefaultIdle
	}
	if u.AbsoluteTimeout <= 0 {
		u.AbsoluteTimeout = uiDefaultAbsolute
	}
	if u.RecheckEvery <= 0 {
		u.RecheckEvery = uiRecheckDefault
	}
	u.failIP = newFailLimiter(uiFailedLogins, u.Now)
	u.perUser = newFailLimiter(uiPerSubjectPerMin, u.Now)
	u.tpl = &lazytmpl.Set{FS: uiAssets, Dir: "ui/templates", Funcs: template.FuncMap{
		"t":     u.msg,
		"when":  func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"day":   func(t *time.Time) string { return t.UTC().Format(time.DateOnly) },
		"size":  humanSize,
		"scan":  func(v string) string { return u.msgOr("scan."+v, v) },
		"role":  func(v string) string { return u.msgOr("role."+v, v) },
		"state": func(v string) string { return u.msgOr("state."+v, v) },
	}}
}

// humanSize shows a byte count with a binary unit, one decimal above a KiB.
func humanSize(n int64) string {
	if n < 1<<10 {
		return strconv.FormatInt(n, 10) + " B"
	}
	v, unit := float64(n)/(1<<10), "KiB"
	for _, next := range []string{"MiB", "GiB", "TiB"} {
		if v < 1024 {
			break
		}
		v, unit = v/1024, next
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

// Handler returns the UI handler.
func (u *UI) Handler() http.Handler {
	u.once.Do(u.init)
	mux := http.NewServeMux()
	for _, p := range u.publicRoutes() {
		h := p.h
		mux.HandleFunc(p.pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, uiMaxFormBytes)
			}
			h(w, r)
		})
	}
	mux.HandleFunc("POST /logout", u.authed(uiRoute{h: u.logout}))
	for _, r := range u.routes() {
		mux.HandleFunc(r.pattern, u.authed(r))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ss, p, ok := u.current(w, r); ok {
			u.renderError(w, r, &uiCtx{u: u, ss: ss, p: &p, r: r, w: w}, http.StatusNotFound, "err.not_found_title", "err.not_found")
			return
		}
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setUIHeaders(w)
		rec := &uiRecorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			p := recover()
			// The route pattern only: a query string can hold subject ids and a ticket.
			u.Log.Info("ui request", "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { //nolint:errorlint // the sentinel is panicked, never wrapped
				panic(p) // a download that failed its checksum: the connection must be cut, not completed
			}
			u.Log.Error("panic in ui handler", "panic", p)
			if rec.status == 0 {
				u.renderError(rec, r, nil, http.StatusInternalServerError, "err.internal_title", "err.internal")
			}
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				u.renderError(rec, r, nil, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
		}
		mux.ServeHTTP(rec, r)
	})
}

type uiRecorder struct {
	http.ResponseWriter
	status int
}

func (r *uiRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *uiRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (upload and download deadlines).
func (r *uiRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// setUIHeaders are the headers of every response. There is no script of any kind, so scripts are
// forbidden outright. No ETag or Last-Modified is ever set: authenticated pages are never cached.
// A download replaces the policy with the API's own (serveContent).
func setUIHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
}

// uiCtx is what a handler gets: the session, the resolved caller and the request. parts is set on
// upload routes: the rest of the multipart body after the CSRF field.
type uiCtx struct {
	u     *UI
	ss    *uiSession
	p     *Principal
	r     *http.Request
	w     http.ResponseWriter
	parts *multipart.Reader
}

type uiHandler func(c *uiCtx) error

func (c *uiCtx) ctx() context.Context { return c.r.Context() }
func (c *uiCtx) id() string           { return c.r.PathValue("id") }
func (c *uiCtx) q(name string) string { return c.r.URL.Query().Get(name) }
func (c *uiCtx) form(name string) string {
	return strings.TrimSpace(c.r.PostForm.Get(name))
}

// redirect answers a post. path is always built from a validated id.
func (c *uiCtx) redirect(path, okKey string) error {
	if okKey != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "ok=" + url.QueryEscape(okKey)
	}
	http.Redirect(c.w, c.r, path, http.StatusSeeOther)
	return nil
}

// authed wraps a handler: it needs a live session, the per-subject limit, the CSRF token on unsafe
// methods, the permission the API's guard asks for on the same operation (and the same groups), and
// turns Store errors into pages. The CSRF token of an upload is the first part of its body, so it
// is checked before a byte of the file is read.
func (u *UI) authed(rt uiRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ss, p, ok := u.current(w, r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		c := &uiCtx{u: u, ss: ss, p: &p, r: r, w: w}
		if u.perUser.blocked(ss.subject) {
			w.Header().Set("Retry-After", "60")
			u.renderError(w, r, c, http.StatusTooManyRequests, "err.busy_title", "err.busy")
			return
		}
		u.perUser.fail(ss.subject) // counts every request of the subject
		unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead
		switch {
		case unsafe && rt.upload:
			limit := u.Store.cfg.Limits.MaxFileBytes + uiUploadOverhead
			if r.ContentLength > limit {
				// refused on the declared length, before a byte is read
				u.fail(c, ErrTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			uploadDeadlines(w, u.Store.cfg)
			mr, err := r.MultipartReader()
			if err != nil || !csrfPart(mr, ss.csrf) {
				u.renderError(w, r, c, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
			c.parts = mr
		case unsafe:
			r.Body = http.MaxBytesReader(w, r.Body, uiMaxFormBytes)
			if err := r.ParseForm(); err != nil || !constEq(r.PostForm.Get("csrf"), ss.csrf) {
				u.renderError(w, r, c, http.StatusForbidden, "err.csrf_title", "err.csrf")
				return
			}
		}
		if rt.perm != "" && !c.p.Has(rt.perm) {
			u.renderError(w, r, c, http.StatusForbidden, "err.forbidden_title", "err.forbidden")
			return
		}
		if u.Groups != nil {
			gs, err := u.Groups.Groups(r.Context(), c.p.Subject)
			if err != nil {
				u.Log.Error("group lookup unavailable", "route", r.Pattern)
				u.fail(c, ErrUnavailable)
				return
			}
			c.p.Groups = gs
		}
		if err := rt.h(c); err != nil {
			if rt.upload {
				// Read what the browser is still sending (bounded by the limit and the upload
				// deadline), or it reports a broken connection instead of showing the page.
				_, _ = io.Copy(io.Discard, r.Body)
			}
			u.fail(c, err)
		}
	}
}

// csrfPart reads the first part of a multipart body, which must be the CSRF token.
func csrfPart(mr *multipart.Reader, want string) bool {
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "csrf" || part.FileName() != "" {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(part, 128))
	return err == nil && constEq(string(b), want)
}

// formError is a problem with what the person typed that never reached the Store.
type formError struct{ key string }

func (e formError) Error() string { return e.key }

// uiStatus maps a Store error to the HTTP status of the page that reports it.
func uiStatus(err error) int {
	var mbe *http.MaxBytesError
	var fe formError
	switch {
	case errors.As(err, &mbe), errors.Is(err, ErrTooLarge), errors.Is(err, ErrQuota):
		return http.StatusRequestEntityTooLarge
	case errors.As(err, &fe), errors.Is(err, ErrInvalid), errors.Is(err, ErrInfected):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrType):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "database is locked"):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// fail renders the page for an error. The messages of this package's sentinel errors are written
// to be shown (publicMsg), exactly as the API shows them.
func (u *UI) fail(c *uiCtx, err error) {
	st := uiStatus(err)
	var fe formError
	show := func(title, msg string) { u.renderMsg(c.w, c.r, c, st, title, msg) }
	switch {
	case errors.As(err, &fe):
		show("err.invalid_title", u.msg(fe.key))
	case errors.Is(err, ErrInfected):
		show("err.infected_title", u.msg("err.infected"))
	case errors.Is(err, ErrQuota):
		show("err.quota_title", publicMsg(err))
	case st == http.StatusRequestEntityTooLarge:
		show("err.too_large_title", u.msg("err.too_large")+" "+humanSize(u.Store.cfg.Limits.MaxFileBytes)+".")
	case st == http.StatusNotFound:
		show("err.not_found_title", u.msg("err.not_found")) // one page for every kind of "no"
	case st == http.StatusForbidden:
		show("err.forbidden_title", u.msg("err.forbidden"))
	case errors.Is(err, ErrConflict):
		show("err.conflict_title", publicMsg(err))
	case errors.Is(err, ErrInvalid):
		show("err.invalid_title", publicMsg(err))
	case errors.Is(err, ErrType):
		show("err.type_title", publicMsg(err))
	case st == http.StatusServiceUnavailable:
		c.w.Header().Set("Retry-After", "5")
		msg := u.msg("err.unavailable")
		if errors.Is(err, ErrUnavailable) && err.Error() != ErrUnavailable.Error() {
			msg = publicMsg(err)
		}
		show("err.unavailable_title", msg)
	default:
		u.Log.Error("ui internal error", "route", c.r.Pattern, "error", err)
		show("err.internal_title", u.msg("err.internal"))
	}
}

// ---- rendering -------------------------------------------------------------------------------

type navItem struct {
	Href, Label string
	Current     bool
}

// page is the data every template receives. Write and Admin only show or hide controls: the routes
// and the Store check the same things again.
type page struct {
	Lang    string
	Title   string
	Nav     []navItem
	CSRF    string
	Authed  bool
	Flash   string
	Write   bool
	Admin   bool
	Subject string
	Data    any
}

func (u *UI) nav(path string, admin bool) []navItem {
	items := []struct {
		href, key string
		show      bool
	}{{"/", "nav.spaces", true}, {"/admin/files", "nav.admin_files", admin}, {"/admin/erasure", "nav.erasure", admin}}
	out := []navItem{}
	for _, i := range items {
		if !i.show {
			continue
		}
		cur := path == i.href || strings.HasPrefix(path, i.href+"/")
		if i.href == "/" {
			cur = path == "/" || strings.HasPrefix(path, "/spaces/") || strings.HasPrefix(path, "/files/")
		}
		out = append(out, navItem{Href: i.href, Label: u.msg(i.key), Current: cur})
	}
	return out
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, name, titleKey string, data any) {
	pg := page{Lang: "en", Title: u.msg(titleKey), Data: data}
	if c != nil {
		pg.Authed, pg.CSRF, pg.Subject = true, c.ss.csrf, c.p.Subject
		pg.Write, pg.Admin = c.p.canWrite(), c.p.Has(PermAdmin)
		current := r.URL.Path
		if status >= 400 {
			current = "" // an error page must not show which section the path belongs to
		} else {
			pg.Flash = u.flash(r.URL.Query().Get("ok"))
		}
		pg.Nav = u.nav(current, pg.Admin)
	}
	t, err := u.tpl.Get(name)
	if err != nil {
		panic("files ui: template " + name + ": " + err.Error())
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

func (u *UI) renderError(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, titleKey, msgKey string) {
	u.renderMsg(w, r, c, status, titleKey, u.msg(msgKey))
}

func (u *UI) renderMsg(w http.ResponseWriter, r *http.Request, c *uiCtx, status int, titleKey, msg string) {
	u.render(w, r, c, status, "error.html", titleKey, errData{Heading: u.msg(titleKey), Message: msg, RequestID: newRequestID(), Authed: c != nil})
}

// ---- login and logout ------------------------------------------------------------------------

type loginData struct {
	Error      string
	CSRF       string
	TokenLogin bool // the paste-a-token form is offered
}

// loginCSRF returns the double-submit token of the login forms and sets its cookie. It is kept while
// it is valid, so a failed attempt does not invalidate the form the user is looking at.
func (u *UI) loginCSRF(w http.ResponseWriter, r *http.Request) string {
	tok := ""
	if ck, err := r.Cookie(u.loginCSRFName()); err == nil && len(ck.Value) == 43 {
		tok = ck.Value
	}
	if tok == "" {
		tok = newToken()
	}
	u.setCookie(w, u.loginCSRFName(), tok, uiLoginCSRFMaxAge)
	return tok
}

func (u *UI) loginForm(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	u.render(w, r, nil, status, "login.html", "login.title", loginData{Error: errMsg, CSRF: u.loginCSRF(w, r), TokenLogin: !u.NoTokenLogin})
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := u.current(w, r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u.loginForm(w, r, http.StatusOK, "")
}

func (u *UI) loginPost(w http.ResponseWriter, r *http.Request) {
	if u.NoTokenLogin { // ui.token_login: false: launch tickets only; nothing is authenticated or counted
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.token_off"))
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	ck, err := r.Cookie(u.loginCSRFName())
	if err != nil || !constEq(ck.Value, r.PostForm.Get("csrf")) {
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.csrf"))
		return
	}
	// Valid credentials are never throttled: authenticate first, and only a failure counts against
	// the address, so a flood of bad tokens cannot lock out someone with a good one.
	token := strings.TrimSpace(r.PostForm.Get("token"))
	p, err := u.authenticateToken(r.Context(), token)
	if errors.Is(err, ErrUnavailable) {
		u.loginForm(w, r, http.StatusServiceUnavailable, u.msg("login.unavailable"))
		return
	}
	if err != nil || p == nil {
		ip := clientIP(r, u.TrustProxy)
		u.failIP.fail(ip)
		if u.failIP.blocked(ip) {
			w.Header().Set("Retry-After", "60")
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
	id := u.sessions.add(&uiSession{subject: p.Subject, princ: *p, token: token}, u.Now())
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

// ---- static assets ---------------------------------------------------------------------------

func (u *UI) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ct := ""
	switch name {
	case "files.css":
		ct = "text/css; charset=utf-8"
	case "icon.svg":
		ct = "image/svg+xml"
	default:
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	b, _ := uiAssets.ReadFile("ui/static/" + name)
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

// canWrite is presentation only: it decides which controls a page offers. The route table and the
// Store check the permission again when a form is posted.
func (p *Principal) canWrite() bool { return p.Has(PermWrite) }

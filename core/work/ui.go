package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The web UI (specs/023-work-ui) is server-rendered HTML served by can-work itself under /ui/. It
// talks to the Store with the caller's own Principal, so project scope and permissions are the same
// code paths the API uses (an invisible project or issue is a 404, never a 403). There is no
// JavaScript: every action is a form post.

//go:embed webui/templates webui/static
var uiAssets embed.FS

const (
	uiPrefix        = "/ui/"
	uiFormLimit     = 512 << 10
	uiRequestBudget = 12 * time.Second
)

// UIRoute describes one UI route for the contract test: whether it needs a signed-in session and
// which permission it checks ("" for none).
type UIRoute struct {
	Method  string
	Path    string
	Session bool
	Perm    string
}

type uiRoute struct {
	UIRoute
	h func(*uiCtx)
}

// webUI holds the UI's state: sessions, templates and the API it shares limits and the
// authenticator with.
type webUI struct {
	a        *api
	sessions *uiSessions
	pages    map[string]*template.Template
	static   map[string]uiAsset
	now      func() time.Time
}

type uiAsset struct {
	body []byte
	typ  string
	etag string
}

// UIRoutes lists the UI routes with the session and permission each requires. A contract test
// compares it with specs/023-work-ui/contracts/ui-routes.md.
func UIRoutes() []UIRoute {
	var out []UIRoute
	for _, r := range (&webUI{}).routes() {
		out = append(out, r.UIRoute)
	}
	return out
}

func (u *webUI) routes() []uiRoute {
	r, w, none := PermRead, PermWrite, ""
	return []uiRoute{
		{UIRoute{"GET", "/ui/login", false, none}, u.loginPage},
		{UIRoute{"POST", "/ui/login", false, none}, u.loginPost},
		{UIRoute{"POST", "/ui/logout", true, r}, u.logout},
		{UIRoute{"GET", "/ui/{$}", true, r}, u.projects},
		{UIRoute{"GET", "/ui/dashboard", true, r}, u.dashboard},
		{UIRoute{"GET", "/ui/projects/{key}", true, r}, u.projectPage},
		{UIRoute{"POST", "/ui/projects/{key}/issues", true, w}, u.createIssue},
		{UIRoute{"GET", "/ui/projects/{key}/board", true, r}, u.board},
		{UIRoute{"GET", "/ui/projects/{key}/sprints", true, r}, u.sprints},
		{UIRoute{"POST", "/ui/projects/{key}/sprints", true, w}, u.createSprint},
		{UIRoute{"GET", "/ui/projects/{key}/epics", true, r}, u.epics},
		{UIRoute{"POST", "/ui/sprints/{id}/start", true, w}, u.startSprint},
		{UIRoute{"POST", "/ui/sprints/{id}/complete", true, w}, u.completeSprint},
		{UIRoute{"GET", "/ui/issues/{issue}", true, r}, u.issuePage},
		{UIRoute{"POST", "/ui/issues/{issue}/edit", true, w}, u.issueEdit},
		{UIRoute{"POST", "/ui/issues/{issue}/transition", true, w}, u.issueTransition},
		{UIRoute{"POST", "/ui/issues/{issue}/priority", true, w}, u.issuePriority},
		{UIRoute{"POST", "/ui/issues/{issue}/assignee", true, w}, u.issueAssignee},
		{UIRoute{"POST", "/ui/issues/{issue}/sprint", true, w}, u.issueSprint},
		{UIRoute{"POST", "/ui/issues/{issue}/rank", true, w}, u.issueRank},
		{UIRoute{"POST", "/ui/issues/{issue}/comments", true, w}, u.issueComment},
		{UIRoute{"POST", "/ui/issues/{issue}/links", true, w}, u.issueLinkAdd},
		{UIRoute{"POST", "/ui/issues/{issue}/links/{link}/remove", true, w}, u.issueLinkRemove},
		{UIRoute{"POST", "/ui/issues/{issue}/watch", true, w}, u.issueWatch},
		{UIRoute{"GET", "/ui/static/{file}", false, none}, u.serveStatic},
		{UIRoute{"GET", "/ui/manifest.webmanifest", false, none}, u.serveStatic},
	}
}

func newWebUI(a *api, now func() time.Time) (*webUI, error) {
	u := &webUI{a: a, sessions: newUISessions(now), now: now, pages: map[string]*template.Template{}, static: map[string]uiAsset{}}
	funcs := u.funcs()
	entries, err := fs.ReadDir(uiAssets, "webui/templates")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" || name == "partials.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(uiAssets, "webui/templates/layout.html", "webui/templates/partials.html", "webui/templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("ui template %s: %w", name, err)
		}
		u.pages[strings.TrimSuffix(name, ".html")] = t
	}
	files, err := fs.ReadDir(uiAssets, "webui/static")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		b, err := fs.ReadFile(uiAssets, "webui/static/"+f.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		typ := "application/octet-stream"
		switch {
		case strings.HasSuffix(f.Name(), ".css"):
			typ = "text/css; charset=utf-8"
		case strings.HasSuffix(f.Name(), ".svg"):
			typ = "image/svg+xml"
		case strings.HasSuffix(f.Name(), ".webmanifest"):
			typ = "application/manifest+json"
		}
		u.static[f.Name()] = uiAsset{body: b, typ: typ, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	}
	return u, nil
}

// mount registers the UI on the API's mux.
func (u *webUI) mount(mux *http.ServeMux) {
	routes := u.routes()
	for _, rt := range routes {
		mux.HandleFunc(rt.Method+" "+rt.Path, u.wrap(rt))
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusSeeOther) })
	mux.HandleFunc(uiPrefix, u.wrapCatchAll(routes))
}

// uiHeaders replaces the API's all-denying headers with the UI's: only same-origin styles, images
// and forms; no scripts at all.
func uiHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

type uiCtx struct {
	u    *webUI
	w    http.ResponseWriter
	r    *http.Request
	p    *Principal
	sess *uiSession
	id   string // session id (cookie value)
}

func (u *webUI) secure(r *http.Request) bool {
	return r.TLS != nil || (u.a.trustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

func (u *webUI) sessionCookieName(r *http.Request) string {
	if u.secure(r) {
		return "__Host-work_session"
	}
	return "work_session"
}

func (u *webUI) loginCookieName(r *http.Request) string {
	if u.secure(r) {
		return "__Host-work_login_csrf"
	}
	return "work_login_csrf"
}

func (u *webUI) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: u.secure(r), SameSite: http.SameSiteStrictMode})
}

// authenticate resolves the session and re-checks the token against the authenticators on every
// request, so revocation, expiry and permission changes apply at once.
func (u *webUI) authenticate(r *http.Request) (*Principal, *uiSession, string, bool) {
	c, err := r.Cookie(u.sessionCookieName(r))
	if err != nil {
		return nil, nil, "", false
	}
	sess, ok := u.sessions.get(c.Value)
	if !ok {
		return nil, nil, "", false
	}
	p, err := u.principalFor(r, sess.token)
	if err != nil || !p.Can(PermRead) {
		u.sessions.delete(c.Value)
		return nil, nil, "", false
	}
	return p, sess, c.Value, true
}

func (u *webUI) principalFor(r *http.Request, token string) (*Principal, error) {
	req := r.Clone(r.Context())
	req.Header = http.Header{"Authorization": {"Bearer " + token}}
	return u.a.auth.Authenticate(req)
}

// sameSite refuses cross-site posts: Sec-Fetch-Site must be same-origin (or none), and an Origin
// header, when present, must name this host.
func sameSite(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		ou, err := url.Parse(o)
		if err != nil || !strings.EqualFold(ou.Host, r.Host) {
			return false
		}
	} else if o == "null" {
		return false
	}
	return true
}

func (u *webUI) wrap(rt uiRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uiHeaders(w)
		ctx, cancel := context.WithTimeout(r.Context(), uiRequestBudget)
		defer cancel()
		r = r.WithContext(ctx)
		c := &uiCtx{u: u, w: w, r: r}
		if rt.Session {
			p, sess, id, ok := u.authenticate(r)
			if !ok {
				if _, err := r.Cookie(u.sessionCookieName(r)); err == nil {
					u.setCookie(w, r, u.sessionCookieName(r), "", -1)
				}
				c.toLogin()
				return
			}
			c.p, c.sess, c.id = p, sess, id
			key := "ui:" + p.Subject
			if ok, wait := u.a.limiter.allow(key); !ok {
				w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
				c.errorPage(http.StatusTooManyRequests, "err.busy", "err.busy_msg")
				return
			}
		}
		if r.Method == http.MethodPost {
			if !sameSite(r) {
				c.errorPage(http.StatusForbidden, "err.forbidden", "err.forbidden_msg")
				return
			}
			limit := int64(uiFormLimit)
			if u.a.maxBody > 0 && u.a.maxBody < limit {
				limit = u.a.maxBody
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			if err := r.ParseForm(); err != nil {
				var mb *http.MaxBytesError
				if errors.As(err, &mb) {
					c.errorPage(http.StatusRequestEntityTooLarge, "err.too_large", "err.too_large_msg")
				} else {
					c.errorPage(http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
				}
				return
			}
			if rt.Session && subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(c.sess.csrf)) != 1 {
				c.errorPage(http.StatusForbidden, "err.forbidden", "err.forbidden_msg")
				return
			}
		}
		if rt.Perm != "" && !c.p.Can(rt.Perm) {
			c.errorPage(http.StatusForbidden, "err.read_only", "err.read_only_msg")
			return
		}
		rt.h(c)
	}
}

// wrapCatchAll answers every other path under /ui/ with an HTML 404, or 405 when the path exists
// for a different method.
func (u *webUI) wrapCatchAll(routes []uiRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uiHeaders(w)
		c := &uiCtx{u: u, w: w, r: r}
		c.p, c.sess, c.id, _ = u.authenticate(r)
		var allow []string
		for _, rt := range routes {
			if rt.Method != r.Method && matchPath(rt.Path, r.URL.Path) {
				allow = append(allow, rt.Method)
			}
		}
		if len(allow) > 0 {
			w.Header().Set("Allow", strings.Join(allow, ", "))
			c.errorPage(http.StatusMethodNotAllowed, "err.method", "err.method_msg")
			return
		}
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
	}
}

func matchPath(pattern, path string) bool {
	ps, xs := strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(xs) {
		return false
	}
	for i := range ps {
		if strings.HasPrefix(ps[i], "{") {
			continue
		}
		if ps[i] != xs[i] {
			return false
		}
	}
	return true
}

// toLogin sends an unauthenticated caller to the login form, remembering a safe place to return to.
func (c *uiCtx) toLogin() {
	loc := "/ui/login"
	if c.r.Method == http.MethodGet {
		if n, ok := safeNext(c.r.URL.RequestURI()); ok {
			loc += "?next=" + url.QueryEscape(n)
		}
	}
	http.Redirect(c.w, c.r, loc, http.StatusSeeOther)
}

// safeNext accepts only local paths under /ui/ (never the login page, never a path with dot
// segments, backslashes, control characters or encoded dots), so a login link cannot bounce a
// person to another site or to the API.
func safeNext(s string) (string, bool) {
	if len(s) < 4 || len(s) > 2000 || !strings.HasPrefix(s, uiPrefix) || strings.HasPrefix(s, "/ui/login") {
		return "", false
	}
	low := strings.ToLower(s)
	for _, bad := range []string{"..", "\\", "%2e", "%5c", "%00", "%0d", "%0a", "//"} {
		if strings.Contains(low, bad) {
			return "", false
		}
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "", false
	}
	return s, true
}

// withFlash adds ok=<key> to a local URL (replacing a previous one).
func withFlash(next, key string) string {
	if key == "" {
		return next
	}
	frag := ""
	if i := strings.IndexByte(next, '#'); i >= 0 {
		next, frag = next[:i], next[i:]
	}
	path, q, _ := strings.Cut(next, "?")
	var parts []string
	for _, p := range strings.Split(q, "&") {
		if p != "" && !strings.HasPrefix(p, "ok=") {
			parts = append(parts, p)
		}
	}
	parts = append(parts, "ok="+key)
	return path + "?" + strings.Join(parts, "&") + frag
}

func (c *uiCtx) redirect(def, flash string) {
	next := def
	if n, ok := safeNext(c.r.PostFormValue("next")); ok {
		next = n
	}
	http.Redirect(c.w, c.r, withFlash(next, flash), http.StatusSeeOther)
}

// ---- rendering ---------------------------------------------------------------------------

type uiNav struct {
	Href, Label string
	Current     bool
}

type uiBase struct {
	Lang, Title, User, CSRF, Flash string
	Current                        string // nav entry
	Project                        string // key when inside a project
	ProjectTab                     string
	CanWrite                       bool
	Nav                            []uiNav
	D                              any
}

func (c *uiCtx) base(title, current string) *uiBase {
	b := &uiBase{Lang: "en", Title: title, Current: current}
	if c.p != nil {
		b.User, b.CSRF, b.CanWrite = c.p.Subject, c.sess.csrf, c.p.Can(PermWrite)
		b.Nav = []uiNav{
			{"/ui/", msg("nav.projects"), current == "projects"},
			{"/ui/dashboard", msg("nav.dashboard"), current == "dashboard"},
		}
	}
	if ok := c.r.URL.Query().Get("ok"); ok != "" {
		if m, found := messagesEN["flash."+ok]; found {
			b.Flash = m
		}
	}
	return b
}

func (c *uiCtx) render(status int, page string, b *uiBase) {
	t := c.u.pages[page]
	if t == nil {
		panic("work ui: no template " + page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", b); err != nil {
		c.u.a.log.Error("ui render", "page", page, "error", err)
		http.Error(c.w, "internal error", http.StatusInternalServerError)
		return
	}
	c.w.WriteHeader(status)
	_, _ = c.w.Write(buf.Bytes())
}

type uiErrorView struct {
	Message  string
	Problems []ProblemField
	Back     string
	BackText string
	Extra    any
}

func (c *uiCtx) errorPage(status int, titleKey, msgKey string) {
	c.errorView(status, msg(titleKey), &uiErrorView{Message: msg(msgKey)})
}

func (c *uiCtx) errorView(status int, title string, v *uiErrorView) {
	b := c.base(title, "")
	if v.Back == "" {
		v.Back, v.BackText = "/ui/", msg("err.home")
	}
	if v.BackText == "" {
		v.BackText = msg("err.back")
	}
	b.D = v
	c.render(status, "error", b)
}

// fail maps a store error to a page. Invisible things are 404 whatever the reason.
func (c *uiCtx) fail(err error, back string) {
	var ie *InvalidError
	switch {
	case errors.As(err, &ie):
		c.errorView(http.StatusUnprocessableEntity, msg("err.invalid"), &uiErrorView{Message: msg("err.invalid_msg"), Problems: ie.Problems, Back: back})
	case errors.Is(err, ErrNotFound):
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
	case errors.Is(err, ErrForbidden):
		c.errorView(http.StatusForbidden, msg("err.forbidden"), &uiErrorView{Message: msg("err.not_allowed"), Back: back})
	case errors.Is(err, ErrVersionMismatch):
		c.errorView(http.StatusConflict, msg("err.stale"), &uiErrorView{Message: msg("err.stale_msg"), Back: back, BackText: msg("err.reload")})
	case errors.Is(err, ErrConflict):
		c.errorView(http.StatusConflict, msg("err.conflict"), &uiErrorView{Message: conflictText(err), Back: back})
	case errors.Is(err, context.DeadlineExceeded):
		c.w.Header().Set("Retry-After", "5")
		c.errorPage(http.StatusServiceUnavailable, "err.slow", "err.slow_msg")
	default:
		id := randToken(6)
		c.u.a.log.Error("ui request failed", "error", err, "request", id)
		c.errorView(http.StatusInternalServerError, msg("err.internal"), &uiErrorView{Message: msg("err.internal_msg", id)})
	}
}

// conflictText is the server's own explanation ("cannot move X from a to b (allowed: ...)").
func conflictText(err error) string {
	s := err.Error()
	return strings.TrimPrefix(s, ErrConflict.Error()+": ")
}

func (u *webUI) serveStatic(c *uiCtx) {
	name := c.r.PathValue("file")
	if strings.HasSuffix(c.r.URL.Path, "manifest.webmanifest") {
		name = "manifest.webmanifest"
	}
	a, ok := u.static[name]
	if !ok {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	h := c.w.Header()
	h.Set("Content-Type", a.typ)
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("ETag", a.etag)
	if c.r.Header.Get("If-None-Match") == a.etag {
		c.w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = c.w.Write(a.body)
}

package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/lazytmpl"
	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
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
	sessions *session.Manager[Principal]
	pages    *lazytmpl.Set // parsed on first use
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
	r, w, adm, none := PermRead, PermWrite, PermAdmin, ""
	return []uiRoute{
		{UIRoute{"GET", "/ui/login", false, none}, u.loginPage},
		{UIRoute{"POST", "/ui/login", false, none}, u.loginPost},
		{UIRoute{"GET", "/ui/login/ticket", false, none}, u.ticketPage},
		{UIRoute{"POST", "/ui/login/ticket", false, none}, u.ticketPost},
		{UIRoute{"POST", "/ui/logout", true, r}, u.logout},
		{UIRoute{"GET", "/ui/{$}", true, r}, u.projects},
		{UIRoute{"GET", "/ui/dashboard", true, r}, u.dashboard},
		{UIRoute{"GET", "/ui/team/workload", true, r}, u.workloadPage},
		{UIRoute{"GET", "/ui/team/ageing", true, r}, u.ageingPage},
		{UIRoute{"GET", "/ui/team/stale", true, r}, u.stalePage},
		{UIRoute{"GET", "/ui/filters", true, r}, u.filtersPage},
		{UIRoute{"GET", "/ui/filters/{filter}", true, r}, u.filterPage},
		{UIRoute{"POST", "/ui/filters", true, w}, u.filterCreate},
		{UIRoute{"POST", "/ui/filters/{filter}/delete", true, w}, u.filterDelete},
		{UIRoute{"GET", "/ui/projects/new", true, adm}, u.newProjectPage},
		{UIRoute{"POST", "/ui/projects", true, adm}, u.createProject},
		{UIRoute{"GET", "/ui/projects/{key}/settings", true, adm}, u.projectSettings},
		{UIRoute{"POST", "/ui/projects/{key}/settings", true, adm}, u.saveSettings},
		{UIRoute{"POST", "/ui/projects/{key}/archive", true, adm}, u.archiveProject},
		{UIRoute{"POST", "/ui/projects/{key}/members", true, adm}, u.memberAdd},
		{UIRoute{"POST", "/ui/projects/{key}/members/remove", true, adm}, u.memberRemove},
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
	u := &webUI{a: a, now: now, static: map[string]uiAsset{}}
	u.sessions = u.newSessions()
	u.pages = &lazytmpl.Set{FS: uiAssets, Dir: "webui/templates", Funcs: u.funcs()}
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

// uiHeaders replaces the API's all-denying headers with the UI's.
func uiHeaders(w http.ResponseWriter) {
	session.SetHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

type uiCtx struct {
	u    *webUI
	w    http.ResponseWriter
	r    *http.Request
	p    *Principal
	sess *uiSession
}

func (u *webUI) secure(r *http.Request) bool {
	return u.a.forceSecure || r.TLS != nil || (u.a.trustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

// authenticate resolves the session of the request.
func (u *webUI) authenticate(w http.ResponseWriter, r *http.Request) (*Principal, *uiSession, bool) {
	sess, p, ok := u.sessions.Current(w, r)
	if !ok {
		return nil, nil, false
	}
	return &p, sess, true
}

func (u *webUI) principalFor(r *http.Request, token string) (*Principal, error) {
	req := r.Clone(r.Context())
	req.Header = http.Header{"Authorization": {"Bearer " + token}}
	return u.a.auth.Authenticate(req)
}

func (u *webUI) wrap(rt uiRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uiHeaders(w)
		ctx, cancel := context.WithTimeout(r.Context(), uiRequestBudget)
		defer cancel()
		r = r.WithContext(ctx)
		c := &uiCtx{u: u, w: w, r: r}
		if rt.Session {
			p, sess, ok := u.authenticate(w, r)
			if !ok {
				c.toLogin()
				return
			}
			c.p, c.sess = p, sess
			key := "ui:" + p.Subject
			if ok, wait := u.a.limiter.allow(key); !ok {
				w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
				c.errorPage(http.StatusTooManyRequests, "err.busy", "err.busy_msg")
				return
			}
		}
		if r.Method == http.MethodPost {
			if !session.SameOrigin(r) {
				c.errorPage(http.StatusForbidden, "err.forbidden", "err.forbidden_msg")
				return
			}
			limit := int64(uiFormLimit)
			if u.a.maxBody > 0 && u.a.maxBody < limit {
				limit = u.a.maxBody
			}
			if rt.Session {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			} else {
				modclient.LimitPublicForm(w, r) // anyone can post here
			}
			if err := r.ParseForm(); err != nil {
				var mb *http.MaxBytesError
				if errors.As(err, &mb) || errors.Is(err, modclient.ErrPublicForm) {
					c.errorPage(http.StatusRequestEntityTooLarge, "err.too_large", "err.too_large_msg")
				} else {
					c.errorPage(http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
				}
				return
			}
			if rt.Session && !session.Equal(r.PostFormValue("csrf"), c.sess.CSRF) {
				c.errorPage(http.StatusForbidden, "err.forbidden", "err.forbidden_msg")
				return
			}
		}
		if rt.Perm != "" && !c.p.Can(rt.Perm) {
			if rt.Perm == PermAdmin {
				c.errorPage(http.StatusForbidden, "err.forbidden", "err.admin_only")
				return
			}
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
		c.p, c.sess, _ = u.authenticate(w, r)
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
	TeamTab                        string // set on the manager views
	CanWrite, CanAdmin             bool
	CanCreateProject               bool
	Nav                            []uiNav
	D                              any
}

func (c *uiCtx) base(title, current string) *uiBase {
	b := &uiBase{Lang: "en", Title: title, Current: current}
	if c.p != nil {
		b.User, b.CSRF, b.CanWrite = c.p.Subject, c.sess.CSRF, c.p.Can(PermWrite)
		b.CanAdmin = c.p.Can(PermAdmin)
		b.CanCreateProject = b.CanAdmin && c.p.Unscoped()
		if c.sess.SSO() { // a launch-ticket session shows the person's name, not their account id
			if n := c.u.sessions.Name(c.sess); n != "" {
				b.User = n
			}
		}
		b.Nav = []uiNav{
			{"/ui/", msg("nav.projects"), current == "projects"},
			{"/ui/team/workload", msg("nav.team"), current == "team"},
			{"/ui/filters", msg("nav.filters"), current == "filters"},
		}
	}
	if ok := c.r.URL.Query().Get("ok"); ok != "" {
		if m, found := messagesEN["flash."+ok]; found {
			b.Flash = m
		}
	}
	return b
}

var uiBufs = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (c *uiCtx) render(status int, page string, b *uiBase) {
	t, err := c.u.pages.Get(page + ".html")
	if err != nil {
		panic("work ui: template " + page + ": " + err.Error())
	}
	buf := uiBufs.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= 1<<20 { // do not keep a huge page's buffer alive
			uiBufs.Put(buf)
		}
	}()
	if err := t.ExecuteTemplate(buf, "layout", b); err != nil {
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

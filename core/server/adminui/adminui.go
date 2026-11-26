// Package adminui is the server-rendered admin web UI, mounted by can-core on the admin listener
// next to the JSON API. It uses html/template and embedded static assets only: no JavaScript, no
// build step. Sessions, CSRF tokens, throttling and audit are the ones of the JSON API
// (core/server/auth, core/server/audit); see specs/010-admin-ui.
package adminui

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/lazytmpl"
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/overview"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
	"github.com/dodwmd/companyinacan/core/server/users"
)

//go:embed templates static
var assets embed.FS

// pageSize is the number of rows of the list pages.
const pageSize = 50

// Company is what the dashboard shows about the company (from can-core configuration).
type Company struct {
	Name   string
	Domain string
}

// Server holds the UI's dependencies. The fields mirror adminapi.Server so the two share services.
type Server struct {
	Auth     *auth.Service
	DB       *store.DB
	Audit    *audit.Log
	Registry *registry.Registry
	// Shared platform services (epic E11a, platform.go). RBAC nil enforces no permissions (the
	// single-admin behaviour of earlier versions); the approval and notification pages answer 503
	// without their service.
	RBAC      *rbac.Service
	Approvals *approvals.Engine
	Notify    *notify.Service
	// Mod is module management (catalogue, changes, wizard, status; epic E12b). Nil answers 503 on
	// its pages and shows none of its navigation.
	Mod *modmgr.Service
	// PATs issues personal access tokens for module APIs; the tokens page answers 503 without it.
	PATs *pat.Service
	// SSO is OpenID Connect sign-in (epic E11b); nil hides every trace of it.
	SSO *sso.Service
	// Tickets mints the launch tickets of single sign-on into module UIs (E11c); nil hides the Apps page.
	Tickets *ticket.Service
	// Users is user and role administration (E11e, users.go); nil builds one over the fields above,
	// without e-mail.
	Users *users.Service
	// Email is the email.send outbox (E03d, email.go); nil hides the delivery page.
	Email *email.Service
	// Overview collects the modules' figures for the business overview (overview.go); nil is built
	// from Tickets, Registry and DB, and without Tickets the page shows only what can-core knows.
	Overview *overview.Service
	// Leads reads leads through the `leads` capability; nil means "not configured".
	Leads   LeadsClient
	Company Company
	Log     *slog.Logger
	// Secure sets the Secure cookie attribute (false only for plain-HTTP local development).
	Secure bool
	// TrustProxy takes the client address from the ingress's X-Forwarded-For.
	TrustProxy bool
	Now        func() time.Time

	// heavy serializes the whole-log operations (verify, export): each holds the only database
	// connection for as long as the log is long. A second request while one runs is refused.
	heavy sync.Mutex
	once  sync.Once
	tpl   *lazytmpl.Set // pages are parsed on first use
}

type route struct {
	pattern string
	h       http.HandlerFunc
}

func (s *Server) routes() []route {
	return append(append(append(append(append(append(s.coreRoutes(), s.platformRoutes()...), s.modRoutes()...), s.wizardRoutes()...), s.tokenRoutes()...), s.ssoRoutes()...), append(append(s.appRoutes(), s.emailRoutes()...), append(s.userRoutes(), s.overviewRoutes()...)...)...)
}

func (s *Server) coreRoutes() []route {
	return []route{
		{"GET /login", s.loginPage},
		{"POST /login", s.loginPost},
		{"POST /logout", s.authed(s.logout)},
		{"GET /{$}", s.authed(s.wizardGate(s.home))},
		{"GET /dashboard", s.authed(s.homeGate(s.dashboard))},
		{"GET /modules", s.authedPerm(rbac.PermView, s.modules)},
		{"GET /modules/{id}", s.authedPerm(rbac.PermView, s.module)},
		{"GET /audit", s.authedPerm(rbac.PermAudit, s.auditPage)},
		{"POST /audit/verify", s.authedPerm(rbac.PermAudit, s.auditVerify)},
		{"POST /audit/export", s.authedPerm(rbac.PermAudit, s.auditExport)},
		{"GET /events", s.authedPerm(rbac.PermView, s.events)},
		{"GET /events/{id}", s.authedPerm(rbac.PermView, s.event)},
		{"POST /events/{id}/requeue", s.authedPerm(rbac.PermOperate, s.requeue)},
		{"GET /leads", s.authedPerm(rbac.PermView, s.leads)},
		{"GET /leads/{id}", s.authedPerm(rbac.PermView, s.lead)},
		{"POST /leads/{id}/status", s.authedPerm(rbac.PermOperate, s.leadStatus)},
		{"GET /settings", s.authed(s.settings)},
		{"POST /settings/password", s.authed(s.changePassword)},
		{"POST /settings/totp", s.authed(s.totpStart)},
		{"POST /settings/totp/confirm", s.authed(s.totpConfirm)},
		{"POST /settings/sessions/{sid}/revoke", s.authed(s.revokeSession)},
		{"GET /static/{file}", s.static},
		{"GET /manifest.webmanifest", s.manifest},
		{"GET /sw.js", s.serviceWorker},
	}
}

// Routes lists the "METHOD /path" patterns served; a contract test compares them with
// specs/010-admin-ui/contracts/ui-routes.md.
func Routes() []string {
	var out []string
	for _, r := range (&Server{}).routes() {
		out = append(out, r.pattern)
	}
	return out
}

// Handler returns the UI handler. When api is not nil, /api/ and /healthz are served by it and
// everything else by the UI, so can-core mounts one handler on the admin listener.
func (s *Server) Handler(api http.Handler) http.Handler {
	s.once.Do(s.init)
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.HandleFunc(r.pattern, r.h)
	}
	mux.HandleFunc("/", s.notFound)
	ui := httpx.Middleware(s.Log, "adminui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		mux.ServeHTTP(w, r)
	}))
	if api == nil {
		return ui
	}
	top := http.NewServeMux()
	top.Handle("/api/", api)
	top.Handle("GET /healthz", api)
	top.Handle("/", ui)
	return top
}

func (s *Server) init() {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.Overview == nil && s.Tickets != nil && s.Registry != nil {
		s.Overview = &overview.Service{Registry: s.Registry, DB: s.DB, Grants: s.Tickets, Log: s.Log, Now: s.Now}
	}
	funcs := template.FuncMap{
		"t":         s.msg,
		"ts":        func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") },
		"iso":       func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"staticURL": assetURL,
	}
	s.tpl = &lazytmpl.Set{FS: assets, Dir: "templates", Funcs: funcs}
}

// setHeaders applies the security headers of every UI response. There is no script of any kind, so
// scripts are forbidden outright; styles, images, the manifest and forms are same-origin only.
func setHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cache-Control", "no-store")
}

// Page is the data every template receives.
type Page struct {
	Lang      string
	Title     string
	Nav       []navItem
	User      string
	CSRF      string
	Flash     string
	RequestID string
	Data      any
}

type navItem struct {
	Href    string
	Label   string
	Current bool
	// Count is an unread badge shown next to the label ("99+" when capped); empty for none.
	Count string
}

// sessionCtx is an authenticated request.
type sessionCtx struct {
	Session store.Session
	User    store.AdminUser
	Token   string
}

func (s *Server) page(r *http.Request, sc *sessionCtx, active, title string, data any) *Page {
	p := &Page{Lang: "en", Title: title, Data: data, RequestID: httpx.RequestID(r.Context())}
	if k := r.URL.Query().Get("flash"); strings.HasPrefix(k, "flash.") {
		if _, ok := messagesEN[k]; ok {
			p.Flash = s.msg(k)
		}
	}
	if sc != nil {
		p.User, p.CSRF = sc.User.Username, sc.Session.CSRF
		for _, n := range []struct{ key, label, href string }{
			{"overview", "nav.overview", "/overview"}, {"dashboard", "nav.dashboard", "/dashboard"}, {"modules", "nav.modules", "/modules"}, {"audit", "nav.audit", "/audit"},
			{"events", "nav.events", "/events"}, {"leads", "nav.leads", "/leads"}, {"settings", "nav.settings", "/settings"},
		} {
			p.Nav = append(p.Nav, navItem{Href: n.href, Label: s.msg(n.label), Current: n.key == active})
		}
		p.Nav = append(p.Nav, s.platformNav(r, sc, active)...)
		p.Nav = append(p.Nav, s.appsNav(active)...)
		p.Nav = append(p.Nav, s.modNav(r, sc, active)...)
		p.Nav = append(p.Nav, s.ssoNav(r, sc, active)...)
	}
	return p
}

// render executes a page into a buffer first so a template error never sends half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p *Page) {
	t, err := s.tpl.Get(name)
	var buf bytes.Buffer
	if err != nil || t.ExecuteTemplate(&buf, "layout", p) != nil {
		s.Log.Error("template failed", "request_id", httpx.RequestID(r.Context()), "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type errorData struct {
	Heading, Message, RequestID, Home, HomeLabel string
}

// fail renders the styled error page for status with catalogue keys for heading and message.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, headingKey, msgKey string) {
	d := errorData{Heading: s.msg(headingKey), Message: s.msg(msgKey), RequestID: httpx.RequestID(r.Context()), Home: "/", HomeLabel: s.msg("err.home")}
	if sc == nil {
		d.Home, d.HomeLabel = "/login", s.msg("err.login")
	}
	s.render(w, r, status, "error.html", s.page(r, sc, "", d.Heading, d))
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error) {
	s.Log.Error("request failed", "request_id", httpx.RequestID(r.Context()), "route", r.Pattern, "error", err)
	s.fail(w, r, sc, http.StatusInternalServerError, "err.internal", "err.internal_msg")
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path, flash string) {
	if flash != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "flash=" + flash
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// currentSession resolves the cookie without redirecting; ok is false when not signed in.
func (s *Server) currentSession(r *http.Request) (*sessionCtx, bool, error) {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return nil, false, nil
	}
	sess, user, err := s.Auth.Authenticate(r.Context(), c.Value)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &sessionCtx{Session: sess, User: user, Token: c.Value}, true, nil
}

// authed requires a session (otherwise 303 to /login) and, for unsafe methods, a same-site request
// carrying the session's CSRF token in the `csrf` form field.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, *sessionCtx)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, ok, err := s.currentSession(r)
		if err != nil {
			s.internal(w, r, nil, err)
			return
		}
		if !ok {
			if _, cerr := r.Cookie(auth.CookieName); cerr == nil {
				http.SetCookie(w, auth.ClearCookie(s.Secure))
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				s.fail(w, r, sc, http.StatusForbidden, "err.forbidden", "err.forbidden_msg")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes)
			if err := r.ParseForm(); err != nil {
				s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
				return
			}
			if !auth.CSRFValid(sc.Session, r.PostFormValue("csrf")) {
				s.fail(w, r, sc, http.StatusForbidden, "err.forbidden", "err.forbidden_msg")
				return
			}
		}
		h(w, r, sc)
	}
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	sc, ok, err := s.currentSession(r)
	if err != nil || !ok {
		sc = nil
	}
	s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func equalTokens(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// --- static assets ----------------------------------------------------------------------------

var contentTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".png":         "image/png",
	".svg":         "image/svg+xml",
	".webmanifest": "application/manifest+json",
}

func (s *Server) serveAsset(w http.ResponseWriter, name, cache string) {
	b, err := fs.ReadFile(assets, "static/"+name)
	ct := contentTypes[path.Ext(name)]
	if err != nil || ct == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", cache)
	_, _ = w.Write(b)
}

const shortCache = "public, max-age=3600"

// assetHashes maps an embedded static file to the first 10 hex digits of its SHA-256. Templates link
// assets as /static/<file>?v=<hash>; a request carrying the current hash is cacheable forever
// (the URL changes when the content does), anything else keeps the one-hour revalidating policy.
var assetHashes = sync.OnceValue(func() map[string]string {
	out := map[string]string{}
	entries, _ := fs.ReadDir(assets, "static")
	for _, e := range entries {
		if b, err := fs.ReadFile(assets, "static/"+e.Name()); err == nil {
			sum := sha256.Sum256(b)
			out[e.Name()] = hex.EncodeToString(sum[:])[:10]
		}
	}
	return out
})

func assetURL(name string) string {
	if h, ok := assetHashes()[name]; ok {
		return "/static/" + name + "?v=" + h
	}
	return "/static/" + name
}

// serviceWorker serves the push-only worker (static/sw.js) from the site root so its scope is the
// whole site. It has its own response headers: revalidated on every update check, allowed scope "/",
// and a CSP that lets the worker do nothing but run (no network).
func (s *Server) serviceWorker(w http.ResponseWriter, _ *http.Request) {
	b, err := fs.ReadFile(assets, "static/sw.js")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Service-Worker-Allowed", "/")
	h.Set("Content-Security-Policy", "default-src 'none'")
	_, _ = w.Write(b)
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "" || name == "sw.js" || strings.HasPrefix(name, ".") || !strings.Contains(name, ".") {
		s.notFound(w, r)
		return
	}
	if _, err := fs.Stat(assets, "static/"+name); err != nil || strings.ContainsAny(name, "/\\") {
		s.notFound(w, r)
		return
	}
	cache := shortCache
	if v := r.URL.Query().Get("v"); v != "" && v == assetHashes()[name] {
		cache = "public, max-age=31536000, immutable"
	}
	s.serveAsset(w, name, cache)
}

func (s *Server) manifest(w http.ResponseWriter, _ *http.Request) {
	s.serveAsset(w, "manifest.webmanifest", shortCache)
}

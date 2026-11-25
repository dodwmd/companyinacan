// Package adminapi is the admin HTTP API served on the admin listener (reachable only through
// Traefik). Contract: specs/005-can-core-mvp/contracts/admin-api.yaml.
package adminapi

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Server holds the admin API's dependencies.
type Server struct {
	Auth       *auth.Service
	DB         *store.DB
	Audit      *audit.Log
	Registry   *registry.Registry
	Log        *slog.Logger
	Secure     bool // set the Secure cookie attribute (false only for plain-HTTP local dev)
	TrustProxy bool // take the client address from the ingress's X-Forwarded-For
	Now        func() time.Time
}

type route struct {
	pattern string
	h       http.HandlerFunc
}

type principal struct {
	Session store.Session
	User    store.AdminUser
	Token   string
}

func (s *Server) routes() []route {
	return []route{
		{"GET /healthz", httpx.Health},
		{"POST /api/v1/login", s.login},
		{"POST /api/v1/logout", s.authed(s.logout)},
		{"GET /api/v1/session", s.authed(s.session)},
		{"GET /api/v1/modules", s.authed(s.listModules)},
		{"GET /api/v1/modules/{id}", s.authed(s.getModule)},
		{"GET /api/v1/capabilities", s.authed(s.listCapabilities)},
		{"GET /api/v1/audit", s.authed(s.listAudit)},
		{"GET /api/v1/audit/verify", s.authed(s.verifyAudit)},
		{"GET /api/v1/deliveries", s.authed(s.listDeliveries)},
		{"GET /api/v1/deliveries/{id}/attempts", s.authed(s.listAttempts)},
		{"POST /api/v1/deliveries/{id}/requeue", s.authed(s.requeue)},
	}
}

// Routes lists the "METHOD /path" patterns served; a contract test compares them to the OpenAPI file.
func Routes() []string {
	var out []string
	for _, r := range (&Server{}).routes() {
		out = append(out, r.pattern)
	}
	return out
}

// Handler returns the API handler with logging, recovery and security headers.
func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.HandleFunc(r.pattern, r.h)
	}
	return httpx.Middleware(s.Log, "admin", mux)
}

// authed requires a valid session cookie and, for unsafe methods, a matching CSRF header.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(auth.CookieName)
		if err != nil {
			httpx.Problem(w, http.StatusUnauthorized, "login required")
			return
		}
		sess, user, err := s.Auth.Authenticate(r.Context(), c.Value)
		if errors.Is(err, auth.ErrUnauthenticated) {
			http.SetCookie(w, auth.ClearCookie(s.Secure))
			httpx.Problem(w, http.StatusUnauthorized, "login required")
			return
		}
		if err != nil {
			s.internal(w, r, err)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !auth.CSRFValid(sess, r.Header.Get("X-CSRF-Token")) {
				httpx.Problem(w, http.StatusForbidden, "missing or invalid X-CSRF-Token")
				return
			}
		}
		h(w, r, principal{Session: sess, User: user, Token: c.Value})
	}
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "request_id", httpx.RequestID(r.Context()), "error", err)
	httpx.Problem(w, http.StatusInternalServerError, "internal error")
}

type loginBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var b loginBody
	if !httpx.DecodeJSONMax(w, r, &b, httpx.MaxLoginBodyBytes) {
		return
	}
	res, err := s.Auth.Login(r.Context(), auth.LoginRequest{Username: b.Username, Password: b.Password, Code: b.TOTP, IP: httpx.ClientIP(r, s.TrustProxy)})
	var locked *auth.LockedError
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.RetryAfter.Seconds())+1))
		httpx.Problem(w, http.StatusTooManyRequests, "too many failed attempts; try again later")
		return
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "5")
		httpx.Problem(w, http.StatusServiceUnavailable, "login is busy; try again shortly")
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		httpx.Problem(w, http.StatusUnauthorized, "invalid username, password or authentication code")
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	http.SetCookie(w, auth.SessionCookie(res.Token, res.Session.ExpiresAt, s.Secure))
	httpx.WriteJSON(w, http.StatusOK, sessionView{Username: b.Username, CSRFToken: res.Session.CSRF, ExpiresAt: res.Session.ExpiresAt})
}

type sessionView struct {
	Username  string    `json:"username"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, p principal) {
	if err := s.Auth.Logout(r.Context(), p.Token); err != nil {
		s.internal(w, r, err)
		return
	}
	http.SetCookie(w, auth.ClearCookie(s.Secure))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) session(w http.ResponseWriter, _ *http.Request, p principal) {
	httpx.WriteJSON(w, http.StatusOK, sessionView{Username: p.User.Username, CSRFToken: p.Session.CSRF, ExpiresAt: p.Session.ExpiresAt})
}

type moduleView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Version   string    `json:"version"`
	State     string    `json:"state"`
	Namespace string    `json:"namespace"`
	LastError string    `json:"last_error,omitempty"`
	Provides  []string  `json:"provides"`
	RAMMB     int       `json:"ram_mb"`
	UpdatedAt time.Time `json:"updated_at"`
}

// view deliberately omits the HMAC secret and the full manifest.
func view(m store.Module) moduleView {
	v := moduleView{ID: m.ID, Name: m.Manifest.Name, Version: m.Version, State: m.State, Namespace: m.Namespace, LastError: m.LastError, Provides: []string{}, RAMMB: m.Manifest.Resources.RAMMB, UpdatedAt: m.UpdatedAt}
	for _, p := range m.Manifest.Provides {
		v.Provides = append(v.Provides, p.Capability)
	}
	return v
}

func (s *Server) listModules(w http.ResponseWriter, r *http.Request, _ principal) {
	ms, err := s.DB.ListModules(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]moduleView, 0, len(ms))
	for _, m := range ms {
		out = append(out, view(m))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"modules": out})
}

func (s *Server) getModule(w http.ResponseWriter, r *http.Request, _ principal) {
	m, err := s.DB.GetModule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Problem(w, http.StatusNotFound, "no such module")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view(m))
}

func (s *Server) listCapabilities(w http.ResponseWriter, r *http.Request, _ principal) {
	rs, err := s.Registry.List(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if rs == nil {
		rs = []registry.Resolution{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"capabilities": rs})
}

func intParam(r *http.Request, name string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v < 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, _ principal) {
	after := int64(intParam(r, "after", 0, 1<<62))
	recs, err := s.Audit.List(r.Context(), after, intParam(r, "limit", 100, 500))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if recs == nil {
		recs = []audit.Record{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"records": recs})
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request, _ principal) {
	opt := audit.VerifyOptions{}
	if h := r.URL.Query().Get("head"); h != "" {
		head, err := audit.ParseHead(h)
		if err != nil {
			httpx.Problem(w, http.StatusBadRequest, err.Error())
			return
		}
		opt.Expected = &head
	}
	res, err := s.Audit.Verify(r.Context(), opt)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

type deliveryView struct {
	ID       int64     `json:"id"`
	EventID  string    `json:"event_id"`
	Type     string    `json:"event_type"`
	Module   string    `json:"module"`
	Status   string    `json:"status"`
	Attempts int       `json:"attempts"`
	NextAt   time.Time `json:"next_attempt_at"`
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request, _ principal) {
	ds, err := s.DB.ListDeliveries(r.Context(), intParam(r, "limit", 100, 500))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]deliveryView, 0, len(ds))
	for _, d := range ds {
		out = append(out, deliveryView{ID: d.ID, EventID: d.Event.ID, Type: d.Event.Type, Module: d.ModuleID, Status: d.Status, Attempts: d.Attempts, NextAt: d.NextAttemptAt})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

func (s *Server) requeue(w http.ResponseWriter, r *http.Request, p principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Problem(w, http.StatusBadRequest, "delivery id must be a number")
		return
	}
	err = s.DB.InTx(r.Context(), func(tx *sql.Tx) error {
		if err := store.RequeueDeliveryTx(r.Context(), tx, id, s.Now()); err != nil {
			return err
		}
		_, err := s.Audit.AppendTx(r.Context(), tx, audit.Entry{Actor: p.User.Username, Action: "delivery.requeue", Target: strconv.FormatInt(id, 10)})
		return err // a failed audit write rolls the requeue back
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.Problem(w, http.StatusNotFound, "no such delivery")
		return
	case errors.Is(err, store.ErrNotDead):
		httpx.Problem(w, http.StatusConflict, "only dead deliveries can be requeued")
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type attemptView struct {
	Attempt    int       `json:"attempt"`
	At         time.Time `json:"at"`
	StatusCode int       `json:"status_code"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
}

func (s *Server) listAttempts(w http.ResponseWriter, r *http.Request, _ principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Problem(w, http.StatusBadRequest, "delivery id must be a number")
		return
	}
	as, err := s.DB.Attempts(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := make([]attemptView, 0, len(as))
	for _, a := range as {
		out = append(out, attemptView{Attempt: a.Number, At: a.At, StatusCode: a.StatusCode, Error: a.Error, DurationMS: a.Duration.Milliseconds()})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"attempts": out})
}

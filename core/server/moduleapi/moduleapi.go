// Package moduleapi is the module-facing HTTP API on its own listener (reachable only from module
// namespaces). Every call except /healthz carries the caller's projected ServiceAccount token.
// Contract: specs/005-can-core-mvp/contracts/module-api.yaml.
package moduleapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

// Authenticator resolves a bearer token to an installed, enabled module.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (store.Module, error)
}

// Server holds the module API's dependencies.
type Server struct {
	Auth     Authenticator
	Registry *registry.Registry
	Bus      *events.Bus
	Log      *slog.Logger
}

type route struct {
	pattern string
	h       http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{"GET /healthz", httpx.Health},
		{"GET /v1/self", s.authed(s.self)},
		{"POST /v1/tokens/introspect", s.authed(s.introspect)},
		{"GET /v1/capabilities/{name}", s.authed(s.capability)},
		{"POST /v1/events", s.authed(s.publish)},
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

// Handler returns the API handler.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.HandleFunc(r.pattern, r.h)
	}
	return httpx.Middleware(s.Log, "module", mux)
}

func (s *Server) authed(h func(http.ResponseWriter, *http.Request, store.Module)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := s.Auth.Authenticate(tokenreview.WithSource(r.Context(), httpx.ClientIP(r, false)), httpx.BearerToken(r))
		switch {
		case errors.Is(err, modauth.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Bearer realm="can"`)
			httpx.Problem(w, http.StatusUnauthorized, "a valid projected ServiceAccount token (audience can) is required")
			return
		case errors.Is(err, tokenreview.ErrRateLimited):
			w.Header().Set("Retry-After", "1")
			httpx.Problem(w, http.StatusTooManyRequests, "too many token reviews; slow down")
			return
		case errors.Is(err, modauth.ErrForbidden):
			httpx.Problem(w, http.StatusForbidden, "module is not enabled")
			return
		case err != nil:
			s.Log.Error("module authentication failed", "request_id", httpx.RequestID(r.Context()), "error", err)
			httpx.Problem(w, http.StatusBadGateway, "token review unavailable")
			return
		}
		h(w, r, m)
	}
}

func (s *Server) self(w http.ResponseWriter, _ *http.Request, m store.Module) {
	provides := []string{}
	for _, p := range m.Manifest.Provides {
		if !config.ProvidesServed(p.Status) {
			continue
		}
		provides = append(provides, p.Capability)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": m.ID, "version": m.Version, "namespace": m.Namespace, "service_account": m.ServiceAccount, "state": m.State, "provides": provides,
	})
}

type introspectRequest struct {
	Token string `json:"token"`
}

type introspectResponse struct {
	Active         bool   `json:"active"`
	ModuleID       string `json:"module_id,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`
}

// introspect lets an adapter verify a caller's token (SPEC §4.3). The answer for any token that is
// not an enabled module's is simply {"active": false}: it does not reveal why.
func (s *Server) introspect(w http.ResponseWriter, r *http.Request, _ store.Module) {
	var req introspectRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	m, err := s.Auth.Authenticate(r.Context(), req.Token)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, introspectResponse{Active: true, ModuleID: m.ID, Namespace: m.Namespace, ServiceAccount: m.ServiceAccount})
	case errors.Is(err, modauth.ErrUnauthenticated), errors.Is(err, modauth.ErrForbidden):
		httpx.WriteJSON(w, http.StatusOK, introspectResponse{})
	case errors.Is(err, tokenreview.ErrRateLimited):
		w.Header().Set("Retry-After", "1")
		httpx.Problem(w, http.StatusTooManyRequests, "too many token reviews; slow down")
	default:
		s.Log.Error("introspection failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		httpx.Problem(w, http.StatusBadGateway, "token review unavailable")
	}
}

func (s *Server) capability(w http.ResponseWriter, r *http.Request, m store.Module) {
	name := r.PathValue("name")
	if !registry.Declares(&m.Manifest, name) {
		httpx.Problem(w, http.StatusForbidden, "module does not declare "+name+" under requires.capabilities or consumes")
		return
	}
	res, err := s.Registry.Resolve(r.Context(), name)
	switch {
	case errors.Is(err, registry.ErrNoProvider):
		httpx.Problem(w, http.StatusNotFound, err.Error())
	case errors.Is(err, registry.ErrAmbiguous):
		httpx.Problem(w, http.StatusConflict, err.Error())
	case err != nil:
		s.Log.Error("resolve failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		httpx.Problem(w, http.StatusInternalServerError, "internal error")
	default:
		httpx.WriteJSON(w, http.StatusOK, res)
	}
}

type publishRequest struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request, m store.Module) {
	var req publishRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if !events.ValidType(req.Type) {
		httpx.Problem(w, http.StatusBadRequest, "invalid event type: want <noun>.<verb>.v<N>")
		return
	}
	declared := false
	for _, p := range m.Manifest.Publishes {
		declared = declared || p.Event == req.Type
	}
	if !declared {
		httpx.Problem(w, http.StatusForbidden, "module does not list "+req.Type+" under publishes")
		return
	}
	env, err := s.Bus.Publish(r.Context(), m.ID, req.Type, req.Data)
	if err != nil {
		httpx.Problem(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"id": env.ID, "time": env.Time.UTC().Format(time.RFC3339)})
}

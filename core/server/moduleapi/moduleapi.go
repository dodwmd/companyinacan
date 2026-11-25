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
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/identity"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
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
	// Shared platform services (epic E11a, platform.go); nil answers 503 on their routes.
	RBAC      *rbac.Service
	Approvals *approvals.Engine
	Notify    *notify.Service
	// PATs resolves personal access tokens presented to a module (E03c); nil means only module
	// tokens introspect.
	PATs *pat.Service
	// Identity performs access revocation for offboarded people (E13d); nil answers 503.
	Identity *identity.Service
	// Tickets redeems the launch tickets of single sign-on into module UIs (E11c); nil answers 503.
	Tickets *ticket.Service
	// Email is the email.send capability (E03d, email.go); nil answers 503.
	Email *email.Service
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
		{"POST /v1/check", s.authed(s.check)},
		{"POST /v1/approvals", s.authed(s.createApproval)},
		{"GET /v1/approvals/{id}", s.authed(s.getApproval)},
		{"POST /v1/approvals/{id}/cancel", s.authed(s.cancelApproval)},
		{"POST /v1/notifications", s.authed(s.sendNotification)},
		{"POST /v1/identity/revoke", s.authed(s.revokeIdentity)},
		{"POST /v1/tickets/peek", s.authed(s.peekTicket)},
		{"POST /v1/tickets/redeem", s.authed(s.redeemTicket)},
		{"POST /v1/tickets/revalidate", s.authed(s.revalidateSession)},
		{"POST /v1/email", s.authed(s.sendEmail)},
		{"GET /v1/email/{id}", s.authed(s.getEmail)},
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
	// Audience, when set, is the module the caller serves. A personal access token bound to any
	// other module is reported inactive.
	Audience string `json:"audience"`
}

type introspectResponse struct {
	Active bool `json:"active"`
	// Kind is "module" for a projected ServiceAccount token and "user" for a personal access token.
	Kind           string   `json:"kind,omitempty"`
	ModuleID       string   `json:"module_id,omitempty"`
	Namespace      string   `json:"namespace,omitempty"`
	ServiceAccount string   `json:"service_account,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	Permissions    []string `json:"permissions,omitempty"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
}

// introspect lets a module verify a caller's token (SPEC §4.3). A personal access token is
// answered only to the module it is bound to, with its subject and permissions; every other case
// (unknown, expired, revoked, another module's, malformed) is simply {"active": false}: it does
// not reveal why. A projected module token is verified by TokenReview as before.
func (s *Server) introspect(w http.ResponseWriter, r *http.Request, caller store.Module) {
	var req introspectRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if pat.LooksLikeToken(req.Token) {
		// Checked before any TokenReview: a personal access token is not a ServiceAccount token
		// and must not count against the review rate limit.
		if s.PATs == nil || (req.Audience != "" && req.Audience != caller.ID) {
			httpx.WriteJSON(w, http.StatusOK, introspectResponse{})
			return
		}
		res, ok := s.PATs.Introspect(r.Context(), caller.ID, req.Token)
		if !ok {
			httpx.WriteJSON(w, http.StatusOK, introspectResponse{})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, introspectResponse{Active: true, Kind: "user", Subject: res.Subject, Permissions: res.Permissions, ExpiresAt: res.ExpiresAt.UTC().Format(time.RFC3339)})
		return
	}
	m, err := s.Auth.Authenticate(r.Context(), req.Token)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, introspectResponse{Active: true, Kind: "module", ModuleID: m.ID, Namespace: m.Namespace, ServiceAccount: m.ServiceAccount})
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
	// SourceID is the publisher's own id of the event. Publishing it again returns the first
	// event instead of delivering twice, so a publisher can retry safely.
	SourceID string `json:"source_id"`
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
	if config.IsReservedEvent(req.Type) {
		httpx.Problem(w, http.StatusForbidden, "events in the approval. and notification. namespaces are published only by can-core")
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
	if req.SourceID != "" && !events.SourceIDRe.MatchString(req.SourceID) {
		httpx.Problem(w, http.StatusBadRequest, "source_id must be 1 to 128 characters of letters, digits and _ . : -")
		return
	}
	env, dup, err := s.Bus.PublishOnce(r.Context(), m.ID, req.Type, req.Data, req.SourceID)
	if err != nil {
		httpx.Problem(w, http.StatusBadRequest, err.Error())
		return
	}
	out := map[string]any{"id": env.ID}
	if !dup {
		out["time"] = env.Time.UTC().Format(time.RFC3339)
	}
	httpx.WriteJSON(w, http.StatusAccepted, out)
}

package moduleapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/identity"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Shared platform services for modules (SPEC §3.7): RBAC checks, approvals and notifications.
// Contract: specs/005-can-core-mvp/contracts/module-api.yaml, specs/015-approvals-notifications.

func (s *Server) unavailable(w http.ResponseWriter) {
	httpx.Problem(w, http.StatusServiceUnavailable, "this platform service is not available")
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "request_id", httpx.RequestID(r.Context()), "route", r.Pattern, "error", err)
	httpx.Problem(w, http.StatusInternalServerError, "internal error")
}

type checkRequest struct {
	Subject    string `json:"subject"`
	Permission string `json:"permission"`
	Resource   string `json:"resource"`
}

// check answers can.check(subject, permission, resource). A module may only ask about
// permissions its own manifest declares: asking about another module's permissions would be an
// oracle for who holds them. Errors answer 503, never "allowed".
func (s *Server) check(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.RBAC == nil {
		s.unavailable(w)
		return
	}
	var req checkRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if !rbac.ValidSubject(req.Subject) || req.Permission == "" || len(req.Resource) > 200 {
		httpx.Problem(w, http.StatusBadRequest, "subject (user:<name> or party:<id>), permission and an optional resource are required")
		return
	}
	declared := false
	for _, p := range m.Manifest.Permissions {
		declared = declared || p == req.Permission
	}
	if !declared {
		httpx.Problem(w, http.StatusForbidden, "module does not declare permission "+req.Permission)
		return
	}
	ok, err := s.RBAC.Check(r.Context(), req.Subject, req.Permission, req.Resource)
	if err != nil {
		s.Log.Error("permission check failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		httpx.Problem(w, http.StatusServiceUnavailable, "permission check unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"allowed": ok})
}

type createApprovalBody struct {
	Type      string          `json:"type"`
	Requester string          `json:"requester"`
	Ref       string          `json:"ref"`
	Title     string          `json:"title"`
	Amount    int64           `json:"amount"`
	Payload   json.RawMessage `json:"payload"`
}

type approvalView struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Requester string     `json:"requester"`
	Ref       string     `json:"ref,omitempty"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

func viewOf(r approvals.Request) approvalView {
	return approvalView{ID: r.ID, Type: r.Type, Requester: r.Requester, Ref: r.Ref, Status: r.Status, Reason: r.OutcomeReason, CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt}
}

// createApproval opens an approval request. Idempotency-Key is required; the same key and body
// return the original request with 200.
func (s *Server) createApproval(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Approvals == nil {
		s.unavailable(w)
		return
	}
	var b createApprovalBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	var decl *struct{ chain []string }
	for _, a := range m.Manifest.Approvals {
		if a.Type == b.Type {
			decl = &struct{ chain []string }{a.DefaultChain}
		}
	}
	if decl == nil {
		httpx.Problem(w, http.StatusForbidden, "module does not declare approval type "+b.Type+" in its manifest")
		return
	}
	req, created, err := s.Approvals.Create(r.Context(), approvals.CreateRequest{
		ModuleID: m.ID, Type: b.Type, Requester: b.Requester, Ref: b.Ref, Title: b.Title, Amount: b.Amount, Payload: b.Payload,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), DefaultChain: decl.chain,
	})
	switch {
	case errors.Is(err, approvals.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, approvals.ErrRateLimited):
		w.Header().Set("Retry-After", "300")
		httpx.Problem(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, approvals.ErrIdempotencyConflict):
		httpx.Problem(w, http.StatusConflict, err.Error())
	case errors.Is(err, approvals.ErrNoPolicy), errors.Is(err, approvals.ErrNoApprover):
		httpx.Problem(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, approvals.ErrResolver):
		s.Log.Error("reporting line lookup failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		httpx.Problem(w, http.StatusBadGateway, "the reporting line could not be looked up")
	case err != nil:
		s.internal(w, r, err)
	case created:
		httpx.WriteJSON(w, http.StatusCreated, viewOf(req))
	default:
		httpx.WriteJSON(w, http.StatusOK, viewOf(req))
	}
}

// ownApproval loads a request the calling module created; anything else is 404.
func (s *Server) ownApproval(w http.ResponseWriter, r *http.Request, m store.Module) (approvals.Request, bool) {
	if s.Approvals == nil {
		s.unavailable(w)
		return approvals.Request{}, false
	}
	d, err := s.Approvals.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, approvals.ErrNotFound) || (err == nil && d.Request.ModuleID != m.ID) {
		httpx.Problem(w, http.StatusNotFound, "no such approval request")
		return approvals.Request{}, false
	}
	if err != nil {
		s.internal(w, r, err)
		return approvals.Request{}, false
	}
	return d.Request, true
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request, m store.Module) {
	if req, ok := s.ownApproval(w, r, m); ok {
		httpx.WriteJSON(w, http.StatusOK, viewOf(req))
	}
}

type cancelBody struct {
	Reason string `json:"reason"`
}

func (s *Server) cancelApproval(w http.ResponseWriter, r *http.Request, m store.Module) {
	req, ok := s.ownApproval(w, r, m)
	if !ok {
		return
	}
	var b cancelBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	err := s.Approvals.Cancel(r.Context(), req.ID, "module:"+m.ID, b.Reason)
	switch {
	case errors.Is(err, approvals.ErrNotPending):
		httpx.Problem(w, http.StatusConflict, err.Error())
	case errors.Is(err, approvals.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

type notifyBody struct {
	Subject   string            `json:"subject"`
	Type      string            `json:"type"`
	DedupeKey string            `json:"dedupe_key"`
	Data      map[string]string `json:"data"`
	Link      string            `json:"link"`
	Urgent    bool              `json:"urgent"`
}

// sendNotification lets a module put an item in a person's inbox (reminders, attention items).
// The module must list `notify` under requires.capabilities or consumes, and its notification
// types are namespaced by its id so it cannot imitate platform notifications.
func (s *Server) sendNotification(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	if !registry.Declares(&m.Manifest, "notify") {
		httpx.Problem(w, http.StatusForbidden, "module does not declare notify under requires.capabilities or consumes")
		return
	}
	var b notifyBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	if !strings.HasPrefix(b.Type, m.ID+".") {
		httpx.Problem(w, http.StatusBadRequest, "notification type must start with the module id and a dot")
		return
	}
	key := b.DedupeKey
	if key != "" {
		key = m.ID + ":" + key // keys are per module: one module cannot suppress another's notices
	}
	_, created, err := s.Notify.Notify(r.Context(), notify.Notice{Subject: b.Subject, Type: b.Type, DedupeKey: key, Data: b.Data, Link: b.Link, Urgent: b.Urgent, RespectMute: true, Module: m.ID, Authored: true})
	switch {
	case notify.ErrInvalid(err):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		httpx.WriteJSON(w, http.StatusAccepted, map[string]bool{"created": created})
	}
}

// revokeIdentity ends the personal access tokens and sessions of one offboarded person (E13d).
// The service verifies that the caller provides identity.provision, that the people module
// reported the person as offboarded with a reached effective date, and that the subject is the
// account linked to them. Nothing else can be revoked through this route.
func (s *Server) revokeIdentity(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Identity == nil {
		s.unavailable(w)
		return
	}
	var req identity.Request
	if !httpx.DecodeJSONMax(w, r, &req, 4<<10) {
		return
	}
	res, err := s.Identity.Revoke(r.Context(), m, req)
	switch {
	case err == nil:
		httpx.WriteJSON(w, http.StatusOK, res)
	case errors.Is(err, identity.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, "party_id (pers_...) and subject (user:<name>) are required")
	case errors.Is(err, identity.ErrNotEntitled):
		httpx.Problem(w, http.StatusForbidden, "module does not provide identity.provision")
	case errors.Is(err, identity.ErrSubjectMismatch):
		httpx.Problem(w, http.StatusForbidden, "the subject is not the account linked to that person")
	case errors.Is(err, identity.ErrNotOffboarded):
		httpx.Problem(w, http.StatusConflict, "the person is not recorded as offboarded with a reached effective date")
	case errors.Is(err, identity.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		httpx.Problem(w, http.StatusTooManyRequests, "too many revocations; retry later")
	default:
		s.internal(w, r, err)
	}
}

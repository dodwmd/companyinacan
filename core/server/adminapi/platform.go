package adminapi

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/users"
)

// Shared platform services on the admin API: approvals, notifications, users and roles.
// Contract: specs/005-can-core-mvp/contracts/admin-api.yaml, specs/015-approvals-notifications.

func (s *Server) platformRoutes() []route {
	return []route{
		{"GET /api/v1/approvals", s.authed(s.listApprovals)},
		{"GET /api/v1/approvals/{id}", s.authed(s.getApproval)},
		{"POST /api/v1/approvals/{id}/decision", s.authed(s.decideApproval)},
		{"POST /api/v1/approvals/{id}/comments", s.authed(s.commentApproval)},
		{"POST /api/v1/approvals/{id}/cancel", s.authed(s.cancelApproval)},
		{"GET /api/v1/delegations", s.authed(s.listDelegations)},
		{"POST /api/v1/delegations", s.authed(s.createDelegation)},
		{"POST /api/v1/delegations/{id}/revoke", s.authed(s.revokeDelegation)},
		{"GET /api/v1/notifications", s.authed(s.listNotifications)},
		{"GET /api/v1/notifications/unread-count", s.authed(s.unreadCount)},
		{"POST /api/v1/notifications/read-all", s.authed(s.readAll)},
		{"POST /api/v1/notifications/{id}/read", s.authed(s.readOne)},
		{"GET /api/v1/notification-settings", s.authed(s.getNotifySettings)},
		{"POST /api/v1/notification-settings", s.authed(s.setNotifySettings)},
		{"GET /api/v1/notification-preferences", s.authed(s.listPrefs)},
		{"POST /api/v1/notification-preferences", s.authed(s.setPref)},
		{"GET /api/v1/notification-deliveries", s.authedPerm(rbac.PermNotifyAdmin, s.listNotifyDeliveries)},
		{"GET /api/v1/roles", s.authedPerm(rbac.PermManage, s.listRoles)},
		{"POST /api/v1/roles/{role}/overrides", s.authedPerm(rbac.PermManage, s.overrideRole)},
		{"GET /api/v1/users", s.authedPerm(rbac.PermManage, s.listUsers)},
		{"POST /api/v1/users/{username}/roles", s.authedPerm(rbac.PermManage, s.grantRole)},
		{"POST /api/v1/users/{username}/roles/revoke", s.authedPerm(rbac.PermManage, s.revokeRole)},
	}
}

// Subject is the principal's RBAC/approval/notification identity.
func (p principal) Subject() string { return "user:" + p.User.Username }

// authedPerm is authed plus a permission check. With no RBAC service configured nothing is
// enforced; with one, an error or a missing grant answers 403/500, never access.
func (s *Server) authedPerm(perm string, h func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, p principal) {
		if s.RBAC != nil {
			ok, err := s.RBAC.Check(r.Context(), p.Subject(), perm, "")
			if err != nil {
				s.internal(w, r, err)
				return
			}
			if !ok {
				httpx.Problem(w, http.StatusForbidden, "your roles do not include "+perm)
				return
			}
		}
		h(w, r, p)
	})
}

func (s *Server) can(r *http.Request, p principal, perm string) (bool, error) {
	if s.RBAC == nil {
		return true, nil
	}
	return s.RBAC.Check(r.Context(), p.Subject(), perm, "")
}

func (s *Server) unavailable(w http.ResponseWriter) {
	httpx.Problem(w, http.StatusServiceUnavailable, "this platform service is not available")
}

// approvalErr maps engine errors to statuses.
func (s *Server) approvalErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, approvals.ErrNotFound):
		httpx.Problem(w, http.StatusNotFound, "no such approval request")
	case errors.Is(err, approvals.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, approvals.ErrSelfApproval), errors.Is(err, approvals.ErrNotEligible):
		httpx.Problem(w, http.StatusForbidden, err.Error())
	case errors.Is(err, approvals.ErrNotPending), errors.Is(err, approvals.ErrAlreadyVoted):
		httpx.Problem(w, http.StatusConflict, err.Error())
	case errors.Is(err, approvals.ErrResolver), errors.Is(err, approvals.ErrNoApprover):
		s.Log.Error("approval staffing failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		httpx.Problem(w, http.StatusBadGateway, "the next approval step could not be staffed; try again shortly")
	default:
		s.internal(w, r, err)
	}
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Approvals == nil {
		s.unavailable(w)
		return
	}
	limit := intParam(r, "limit", 50, 200)
	var out []approvals.Request
	var err error
	switch v := r.URL.Query().Get("view"); v {
	case "", "waiting":
		out, err = s.Approvals.Waiting(r.Context(), p.Subject(), limit)
	case "requested":
		out, err = s.Approvals.Requested(r.Context(), p.Subject(), limit)
	case "all":
		ok, cerr := s.can(r, p, rbac.PermApprovalAdmin)
		if cerr != nil {
			s.internal(w, r, cerr)
			return
		}
		if !ok {
			httpx.Problem(w, http.StatusForbidden, "your roles do not include "+rbac.PermApprovalAdmin)
			return
		}
		out, err = s.Approvals.List(r.Context(), r.URL.Query().Get("status"), limit)
	default:
		httpx.Problem(w, http.StatusBadRequest, "view must be waiting, requested or all")
		return
	}
	if err != nil {
		s.approvalErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"approvals": out})
}

// visible loads a request the principal may see; anyone else gets 404 so ids reveal nothing.
func (s *Server) visible(w http.ResponseWriter, r *http.Request, p principal) (approvals.Detail, bool) {
	if s.Approvals == nil {
		s.unavailable(w)
		return approvals.Detail{}, false
	}
	d, err := s.Approvals.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.approvalErr(w, r, err)
		return approvals.Detail{}, false
	}
	if !s.Approvals.CanView(r.Context(), d, p.Subject()) {
		httpx.Problem(w, http.StatusNotFound, "no such approval request")
		return approvals.Detail{}, false
	}
	return d, true
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request, p principal) {
	// Audit before returning sensitive data: the payload is the module's, may be personal data.
	d, ok := s.visible(w, r, p)
	if !ok {
		return
	}
	if _, err := s.Audit.Append(r.Context(), auditEntry(p, "approval.read", d.Request.ID)); err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

type decisionBody struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request, p principal) {
	d, ok := s.visible(w, r, p)
	if !ok {
		return
	}
	var b decisionBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	req, err := s.Approvals.Decide(r.Context(), d.Request.ID, p.Subject(), b.Decision, b.Comment)
	if err != nil {
		s.approvalErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, req)
}

type commentBody struct {
	Comment string `json:"comment"`
}

func (s *Server) commentApproval(w http.ResponseWriter, r *http.Request, p principal) {
	d, ok := s.visible(w, r, p)
	if !ok {
		return
	}
	var b commentBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	if err := s.Approvals.Comment(r.Context(), d.Request.ID, p.Subject(), b.Comment); err != nil {
		s.approvalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type cancelBody struct {
	Reason string `json:"reason"`
}

func (s *Server) cancelApproval(w http.ResponseWriter, r *http.Request, p principal) {
	d, ok := s.visible(w, r, p)
	if !ok {
		return
	}
	var b cancelBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	if err := s.Approvals.Cancel(r.Context(), d.Request.ID, p.Subject(), b.Reason); err != nil {
		s.approvalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type delegationBody struct {
	Delegate string    `json:"delegate"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
}

func (s *Server) createDelegation(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Approvals == nil {
		s.unavailable(w)
		return
	}
	var b delegationBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	id, err := s.Approvals.Delegate(r.Context(), p.Subject(), b.Delegate, b.From, b.To)
	if err != nil {
		s.approvalErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) listDelegations(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Approvals == nil {
		s.unavailable(w)
		return
	}
	ds, err := s.Approvals.Delegations(r.Context(), p.Subject())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"delegations": ds})
}

func (s *Server) revokeDelegation(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Approvals == nil {
		s.unavailable(w)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Problem(w, http.StatusBadRequest, "delegation id must be a number")
		return
	}
	if err := s.Approvals.RevokeDelegation(r.Context(), p.Subject(), id); err != nil {
		s.approvalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- notifications ----------------------------------------------------------------------------

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	limit := intParam(r, "limit", 50, 100)
	items, err := s.Notify.List(r.Context(), p.Subject(), notify.ListOptions{UnreadOnly: r.URL.Query().Get("unread") == "true", BeforeID: int64(intParam(r, "before", 0, 1<<62)), Limit: limit})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if items == nil {
		items = []notify.Item{}
	}
	out := map[string]any{"items": items}
	if len(items) == limit && limit > 0 {
		out["next_before"] = items[len(items)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) unreadCount(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	n, err := s.Notify.UnreadCount(r.Context(), p.Subject())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"unread": n, "capped_at": notify.MaxUnreadCount})
}

func (s *Server) readAll(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	if err := s.Notify.MarkAllRead(r.Context(), p.Subject()); err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readOne(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Problem(w, http.StatusBadRequest, "notification id must be a number")
		return
	}
	switch err := s.Notify.MarkRead(r.Context(), p.Subject(), id); {
	case errors.Is(err, store.ErrNotFound):
		httpx.Problem(w, http.StatusNotFound, "no such notification")
	case err != nil:
		s.internal(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) getNotifySettings(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	set, err := s.Notify.Settings(r.Context(), p.Subject())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, set)
}

type settingsBody struct {
	Email      string `json:"email"`
	QuietStart int    `json:"quiet_start"`
	QuietEnd   int    `json:"quiet_end"`
	TZ         string `json:"tz"`
	Digest     string `json:"digest"`
}

func (s *Server) setNotifySettings(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	var b settingsBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	err := s.Notify.SetSettings(r.Context(), notify.Settings{Subject: p.Subject(), Email: b.Email, QuietStart: b.QuietStart, QuietEnd: b.QuietEnd, TZ: b.TZ, Digest: b.Digest})
	switch {
	case notify.ErrInvalid(err):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listPrefs(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	ps, err := s.Notify.Prefs(r.Context(), p.Subject())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"preferences": ps})
}

type prefBody struct {
	Type    string `json:"type"`
	Channel string `json:"channel"`
	Enabled bool   `json:"enabled"`
}

func (s *Server) setPref(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	var b prefBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	switch err := s.Notify.SetPref(r.Context(), p.Subject(), b.Type, b.Channel, b.Enabled); {
	case notify.ErrInvalid(err):
		httpx.Problem(w, http.StatusBadRequest, "type, channel (email, inapp or webpush) or subject is invalid")
	case err != nil:
		s.internal(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) listNotifyDeliveries(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	ds, err := s.Notify.Deliveries(r.Context(), intParam(r, "limit", 100, 500))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if ds == nil {
		ds = []notify.DeliveryLog{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deliveries": ds})
}

// --- users and roles --------------------------------------------------------------------------

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.RBAC == nil {
		s.unavailable(w)
		return
	}
	rs, err := s.RBAC.Roles(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"roles": rs})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.RBAC == nil {
		s.unavailable(w)
		return
	}
	us, err := s.RBAC.Users(r.Context(), intParam(r, "limit", 200, 500))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": us})
}

type roleBody struct {
	Role  string `json:"role"`
	Scope string `json:"scope"`
}

func (s *Server) roleChange(w http.ResponseWriter, r *http.Request, p principal, change func(a users.Actor, name, role, scope string) error) {
	if s.RBAC == nil {
		s.unavailable(w)
		return
	}
	name := r.PathValue("username")
	if !usernameRe.MatchString(name) {
		httpx.Problem(w, http.StatusNotFound, "no such user")
		return
	}
	var b roleBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	if err := change(actorOf(p), name, b.Role, b.Scope); err != nil {
		s.usersErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) grantRole(w http.ResponseWriter, r *http.Request, p principal) {
	s.roleChange(w, r, p, func(a users.Actor, name, role, scope string) error {
		return s.users().Grant(r.Context(), a, name, role, scope)
	})
}

func (s *Server) revokeRole(w http.ResponseWriter, r *http.Request, p principal) {
	s.roleChange(w, r, p, func(a users.Actor, name, role, scope string) error {
		return s.users().Revoke(r.Context(), a, name, role, scope)
	})
}

type overrideBody struct {
	Permission string `json:"permission"`
	Effect     string `json:"effect"`
}

func (s *Server) overrideRole(w http.ResponseWriter, r *http.Request, p principal) {
	if s.RBAC == nil {
		s.unavailable(w)
		return
	}
	var b overrideBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	if err := s.users().Override(r.Context(), actorOf(p), r.PathValue("role"), b.Permission, b.Effect); err != nil {
		s.usersErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func auditEntry(p principal, action, target string) audit.Entry {
	return audit.Entry{Actor: p.User.Username, Action: action, Target: target}
}

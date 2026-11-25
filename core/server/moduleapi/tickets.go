package moduleapi

import (
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Launch tickets (specs/036-module-ui-sso): a module redeems the one-time ticket the browser
// brought to its UI and later asks whether the person behind the session is still allowed. The
// caller is always the authenticated module; the audience of a ticket is never taken from the body.

type redeemRequest struct {
	Ticket string `json:"ticket"`
	// Binding is the nonce cookie of the browser that presented the ticket.
	Binding string `json:"binding"`
}

// peekTicket names who a ticket would sign in, without consuming it.
func (s *Server) peekTicket(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Tickets == nil {
		s.unavailable(w)
		return
	}
	var req redeemRequest
	if !httpx.DecodeJSONMax(w, r, &req, 2<<10) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	p, ok := s.Tickets.Peek(r.Context(), m.ID, req.Ticket, req.Binding)
	if !ok {
		httpx.WriteJSON(w, http.StatusOK, redeemResponse{})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, redeemResponse{Active: true, Subject: p.Subject, Name: p.Name})
}

type redeemResponse struct {
	Active      bool     `json:"active"`
	Subject     string   `json:"subject,omitempty"`
	Name        string   `json:"name,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	ReturnPath  string   `json:"return_path,omitempty"`
	// Session is the opaque handle the module keeps in memory to revalidate the person.
	Session   string `json:"session,omitempty"`
	ExpiresAt string `json:"session_expires_at,omitempty"`
}

// redeemTicket exchanges a ticket for the person. Every failure is {"active": false}.
func (s *Server) redeemTicket(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Tickets == nil {
		s.unavailable(w)
		return
	}
	var req redeemRequest
	if !httpx.DecodeJSONMax(w, r, &req, 2<<10) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	res, ok := s.Tickets.Redeem(r.Context(), m.ID, req.Ticket, req.Binding)
	if !ok {
		httpx.WriteJSON(w, http.StatusOK, redeemResponse{})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, redeemResponse{Active: true, Subject: res.Subject, Name: res.Name, Permissions: res.Permissions,
		ReturnPath: res.ReturnPath, Session: res.Handle, ExpiresAt: res.SessionExpires.UTC().Format(time.RFC3339)})
}

type revalidateRequest struct {
	Session string `json:"session"`
}

// revalidateSession answers whether the person behind a session handle is still allowed.
func (s *Server) revalidateSession(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Tickets == nil {
		s.unavailable(w)
		return
	}
	var req revalidateRequest
	if !httpx.DecodeJSONMax(w, r, &req, 2<<10) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	v, ok := s.Tickets.Revalidate(r.Context(), m.ID, req.Session)
	if !ok {
		httpx.WriteJSON(w, http.StatusOK, redeemResponse{})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, redeemResponse{Active: true, Subject: v.Subject, Name: v.Name, Permissions: v.Permissions})
}

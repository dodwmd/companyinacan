package moduleapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// The email.send capability (epic E03d, specs/039-email-send). A module chooses recipients (within
// its allow-list), a reply-to address, a one-line subject and a plain-text body; the sender address
// and every other header are can-core's.

const emailCapability = "email.send"

// maxEmailBody leaves room for the JSON escaping of the largest text.
const maxEmailBody = 4 * email.MaxTextBytes

type sendEmailRequest struct {
	To       []string `json:"to"`
	ReplyTo  string   `json:"reply_to"`
	Subject  string   `json:"subject"`
	Text     string   `json:"text"`
	SourceID string   `json:"source_id"`
}

func (s *Server) mayEmail(w http.ResponseWriter, m store.Module) bool {
	if !registry.Declares(&m.Manifest, emailCapability) {
		httpx.Problem(w, http.StatusForbidden, "module does not declare "+emailCapability+" under requires.capabilities")
		return false
	}
	if s.Email == nil {
		s.unavailable(w)
		return false
	}
	return true
}

func (s *Server) sendEmail(w http.ResponseWriter, r *http.Request, m store.Module) {
	if !s.mayEmail(w, m) {
		return
	}
	var req sendEmailRequest
	if !httpx.DecodeJSONMax(w, r, &req, maxEmailBody) {
		return
	}
	ids, duplicate, err := s.Email.Send(r.Context(), email.Request{Source: m.ID, To: req.To, ReplyTo: req.ReplyTo, Subject: req.Subject, Text: req.Text, SourceID: req.SourceID})
	var quota *email.QuotaError
	switch {
	case errors.As(err, &quota):
		w.Header().Set("Retry-After", strconv.Itoa(int(quota.RetryAfter.Seconds())))
		httpx.Problem(w, http.StatusTooManyRequests, quota.Error())
	case errors.Is(err, email.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, email.ErrNotAllowed):
		httpx.Problem(w, http.StatusForbidden, err.Error())
	case errors.Is(err, email.ErrConflict):
		httpx.Problem(w, http.StatusConflict, err.Error())
	case errors.Is(err, email.ErrNotConfigured):
		// The reason (which may name files or hosts) is for the operator, not for modules.
		s.Log.Warn("email.send refused: no usable relay", "module", m.ID, "error", err)
		httpx.Problem(w, http.StatusServiceUnavailable, email.ErrNotConfigured.Error())
	case err != nil:
		s.internal(w, r, err)
	case duplicate:
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ids": ids})
	default:
		httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"ids": ids})
	}
}

type emailStatus struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	Attempts  int        `json:"attempts"`
	LastError string     `json:"last_error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	Finished  *time.Time `json:"finished_at,omitempty"`
}

// getEmail answers the delivery status of the module's own message; anything else is the same 404.
func (s *Server) getEmail(w http.ResponseWriter, r *http.Request, m store.Module) {
	if !s.mayEmail(w, m) {
		return
	}
	msg, err := s.Email.Get(r.Context(), m.ID, r.PathValue("id"))
	switch {
	case errors.Is(err, email.ErrNotFound):
		httpx.Problem(w, http.StatusNotFound, email.ErrNotFound.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		out := emailStatus{ID: msg.ID, Status: msg.Status, Attempts: msg.Attempts, LastError: msg.LastError, CreatedAt: msg.CreatedAt.UTC()}
		if !msg.FinishedAt.IsZero() {
			t := msg.FinishedAt.UTC()
			out.Finished = &t
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

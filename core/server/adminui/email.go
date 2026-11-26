package adminui

import (
	"errors"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

// Email delivery status (epic E03d, specs/039-email-send). Addresses are shown masked; subjects and
// bodies are never shown.

func (s *Server) emailRoutes() []route {
	return []route{
		{"GET /email", s.authedPerm(rbac.PermNotifyAdmin, s.emailPage)},
		{"POST /email/{id}/requeue", s.authedPerm(rbac.PermNotifyAdmin, s.emailRequeue)},
	}
}

type emailRow struct {
	ID, Source, Recipient, StatusLabel, StatusClass, LastError string
	Attempts                                                   int
	Created                                                    time.Time
	Requeue                                                    bool
}

type emailData struct {
	Configured      bool
	Host, TLS, From string
	Port            int
	Problem         string
	Rows            []emailRow
	CanRequeue      bool
}

var emailStatusClass = map[string]string{email.StatusPending: "warn", email.StatusSent: "ok", email.StatusDead: "err"}

var emailStatusKey = map[string]string{email.StatusPending: "mail.st_pending", email.StatusSent: "mail.st_sent", email.StatusDead: "mail.st_dead"}

func (s *Server) emailPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Email == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	canRequeue, err := s.allowed(r, sc, rbac.PermOperate)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	list, err := s.Email.List(r.Context(), 100)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	// The list says who was written to (masked), so reading it is recorded before it is shown.
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: subjectOf(sc), Action: "email.view", Target: "outbox", Detail: map[string]any{"rows": len(list)}}); err != nil {
		s.internal(w, r, sc, err)
		return
	}
	relay, problem := s.Email.RelayStatus()
	d := emailData{Configured: problem == "", Host: relay.Host, Port: relay.Port, TLS: relay.TLS, From: relay.From, Problem: problem, CanRequeue: canRequeue}
	for _, m := range list {
		d.Rows = append(d.Rows, emailRow{ID: m.ID, Source: m.Source, Recipient: m.Recipient, StatusLabel: s.msg(emailStatusKey[m.Status]), StatusClass: emailStatusClass[m.Status],
			LastError: m.LastError, Attempts: m.Attempts, Created: m.CreatedAt, Requeue: canRequeue && m.Requeueable})
	}
	s.render(w, r, http.StatusOK, "email.html", s.page(r, sc, "email", s.msg("mail.title"), d))
}

func (s *Server) emailRequeue(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Email == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	ok, err := s.allowed(r, sc, rbac.PermOperate)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	if !ok {
		s.fail(w, r, sc, http.StatusForbidden, "err.no_permission", "err.no_permission_msg")
		return
	}
	switch err := s.Email.Requeue(r.Context(), subjectOf(sc), r.PathValue("id")); {
	case errors.Is(err, email.ErrNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
	case err != nil:
		s.internal(w, r, sc, err)
	default:
		s.redirect(w, r, "/email", "flash.email_requeued")
	}
}

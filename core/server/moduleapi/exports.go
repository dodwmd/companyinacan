package moduleapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/server/companyexport"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Exports takes a module's export stream; *companyexport.Service implements it.
type Exports interface {
	Accept(module string, r io.Reader) error
}

var _ Exports = (*companyexport.Service)(nil)

// putExport streams the body into the running export. A module may only send its own, and only
// while its export Job is the one running.
func (s *Server) putExport(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Exports == nil {
		httpx.Problem(w, http.StatusServiceUnavailable, "export is not available")
		return
	}
	if r.PathValue("module") != m.ID {
		httpx.Problem(w, http.StatusForbidden, "a module may only send its own export")
		return
	}
	rc := http.NewResponseController(w)
	// The stream goes at the speed of whoever downloads the export, within the Job's hour.
	_ = rc.SetReadDeadline(time.Now().Add(time.Hour))
	_ = rc.SetWriteDeadline(time.Now().Add(time.Hour + time.Minute))
	err := s.Exports.Accept(m.ID, r.Body)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, companyexport.ErrNoTurn):
		httpx.Problem(w, http.StatusConflict, "an export is only accepted, once, while this module's export Job is running")
	default:
		s.Log.Error("a module export failed", "request_id", httpx.RequestID(r.Context()), "module", m.ID, "error", err)
		httpx.Problem(w, http.StatusBadRequest, "the export stream could not be taken")
	}
}

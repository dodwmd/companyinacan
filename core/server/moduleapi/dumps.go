package moduleapi

import (
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Dumps is the dump store behind PUT/GET /v1/dumps (epic E08b); *dumps.Dumper implements it.
type Dumps interface {
	Accept(module, name string, r io.Reader) error
	Open(module, name string) (*os.File, error)
}

var _ Dumps = (*dumps.Dumper)(nil)

// putDump stores the body as the caller's own dump. A module may only write and read dumps under
// its own id; uploads are accepted only while a dump run is in progress, so a stray upload can
// never change data under a valid completion marker.
func (s *Server) putDump(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Dumps == nil {
		httpx.Problem(w, http.StatusServiceUnavailable, "the dump store is not configured")
		return
	}
	if r.PathValue("module") != m.ID {
		httpx.Problem(w, http.StatusForbidden, "a module may only use its own dumps")
		return
	}
	rc := http.NewResponseController(w)
	// The server-wide 30 s deadlines are for small JSON bodies.
	_ = rc.SetReadDeadline(time.Now().Add(15 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(16 * time.Minute))
	err := s.Dumps.Accept(m.ID, r.PathValue("name"), r.Body)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, dumps.ErrBadName):
		httpx.Problem(w, http.StatusBadRequest, "not an acceptable dump name")
	case errors.Is(err, dumps.ErrNoRun):
		httpx.Problem(w, http.StatusConflict, "dumps are only accepted while this module's backup hook is running")
	case errors.Is(err, dumps.ErrTooMany):
		httpx.Problem(w, http.StatusRequestEntityTooLarge, "too many files for one run")
	case errors.Is(err, dumps.ErrTooLarge):
		httpx.Problem(w, http.StatusRequestEntityTooLarge, "the dump exceeds the size limit")
	default:
		s.Log.Error("storing a dump failed", "request_id", httpx.RequestID(r.Context()), "module", m.ID, "error", err)
		httpx.Problem(w, http.StatusInternalServerError, "could not store the dump")
	}
}

// getDump returns the caller's own dump (the restore hook's download).
func (s *Server) getDump(w http.ResponseWriter, r *http.Request, m store.Module) {
	if s.Dumps == nil {
		httpx.Problem(w, http.StatusServiceUnavailable, "the dump store is not configured")
		return
	}
	if r.PathValue("module") != m.ID {
		httpx.Problem(w, http.StatusForbidden, "a module may only use its own dumps")
		return
	}
	f, err := s.Dumps.Open(m.ID, r.PathValue("name"))
	switch {
	case errors.Is(err, dumps.ErrNotRestoring):
		httpx.Problem(w, http.StatusConflict, "dumps can only be downloaded while the server is being restored")
		return
	case errors.Is(err, dumps.ErrBadName):
		httpx.Problem(w, http.StatusBadRequest, "not an acceptable dump name")
		return
	case errors.Is(err, os.ErrNotExist):
		httpx.Problem(w, http.StatusNotFound, "no such dump")
		return
	case err != nil:
		s.Log.Error("opening a dump failed", "request_id", httpx.RequestID(r.Context()), "module", m.ID, "error", err)
		httpx.Problem(w, http.StatusInternalServerError, "could not read the dump")
		return
	}
	defer func() { _ = f.Close() }()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

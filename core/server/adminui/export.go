package adminui

// Export of the company's data from the admin UI (specs/071-company-export): the owner
// (platform.export), after a recent proof of identity, downloads the archive. It is streamed to
// the browser; nothing is stored on the server.

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/dodwmd/companyinacan/core/exportcrypt"
	"github.com/dodwmd/companyinacan/core/server/companyexport"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

func (s *Server) exportRoutes() []route {
	return []route{
		{"GET /export", s.authedPerm(rbac.PermExport, s.exportPage)},
		{"POST /export", s.authedPerm(rbac.PermExport, s.exportDownload)},
	}
}

type exportData struct {
	Recent      bool
	StepUpURL   string
	Error       string
	Sealed      bool
	MinPassword int
}

func (s *Server) exportNav(r *http.Request, sc *sessionCtx, active string) []navItem {
	if s.Export == nil {
		return nil
	}
	if ok, err := s.allowed(r, sc, rbac.PermExport); err != nil || !ok {
		return nil
	}
	return []navItem{{Href: "/export", Label: s.msg("nav.export"), Current: active == "export"}}
}

func (s *Server) renderExport(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, errKey string, sealed bool) {
	d := exportData{Recent: s.users().Recent(actorOf(sc)), StepUpURL: "/reauth?next=" + url.QueryEscape("/export"), Sealed: sealed, MinPassword: exportcrypt.MinPassphrase}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	s.render(w, r, status, "export.html", s.page(r, sc, "export", s.msg("ex.title"), d))
}

func (s *Server) exportPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Export == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	s.renderExport(w, r, sc, http.StatusOK, "", false)
}

// lazyHeaders sets the download's headers with the first byte, so that a failure before it is
// still answered with a page.
type lazyHeaders struct {
	w       http.ResponseWriter
	name    string
	started bool
}

func (l *lazyHeaders) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		h := l.w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", `attachment; filename="`+l.name+`"`)
	}
	return l.w.Write(p)
}

func (s *Server) exportDownload(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Export == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	if !s.users().Recent(actorOf(sc)) {
		http.Redirect(w, r, "/reauth?next="+url.QueryEscape("/export"), http.StatusSeeOther)
		return
	}
	sealed := r.PostFormValue("sealed") == "yes"
	pass, again := r.PostFormValue("passphrase"), r.PostFormValue("passphrase2")
	bad := ""
	switch {
	case sealed && r.PostFormValue("understood") != "yes":
		bad = "ex.err_understood"
	case sealed && pass == "":
		bad = "ex.err_sealed_passphrase"
	case pass != again:
		bad = "ex.err_mismatch"
	case pass != "" && exportcrypt.CheckPassphrase([]byte(pass)) != nil:
		bad = "ex.err_short"
	}
	if bad != "" {
		s.renderExport(w, r, sc, http.StatusBadRequest, bad, sealed)
		return
	}
	req, err := s.Export.Prepare(r.Context(), companyexport.Options{IncludeSealed: sealed, Actor: sc.User.Username})
	switch {
	case errors.Is(err, companyexport.ErrBusy):
		w.Header().Set("Retry-After", "60")
		s.renderExport(w, r, sc, http.StatusServiceUnavailable, "ex.err_busy", sealed)
		return
	case err != nil:
		s.internal(w, r, sc, err)
		return
	}
	name := "company-export-" + s.Now().UTC().Format("20060102-150405") + ".tar.gz"
	if pass != "" {
		name += exportcrypt.Extension
	}
	// The server's 30 s deadlines are for pages.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(companyexport.Timeout))
	out := &lazyHeaders{w: w, name: name}
	var dst io.Writer = out
	var enc io.WriteCloser
	if pass != "" {
		if enc, err = exportcrypt.NewWriter(out, []byte(pass)); err != nil {
			req.Abort()
			s.internal(w, r, sc, err)
			return
		}
		dst = enc
	}
	_, err = req.Write(r.Context(), dst)
	if err == nil && enc != nil {
		err = enc.Close()
	}
	if err != nil {
		s.Log.Error("export download failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		if !out.started {
			s.renderExport(w, r, sc, http.StatusBadGateway, "ex.err_failed", sealed)
			return
		}
		// Breaks the connection, so the browser reports a failed download and not a whole file.
		panic(http.ErrAbortHandler)
	}
}

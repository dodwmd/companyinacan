package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
)

const (
	maxJSONBody      = 64 << 10
	authFailsPerMin  = 30
	downloadDeadline = 15 * time.Minute
)

// API is the files HTTP API. Handlers only parse and render: every decision (who may see what,
// what an upload must satisfy) is made by the Store.
type API struct {
	Store      *Store
	Auth       Authenticator
	Groups     GroupResolver // optional
	Log        *slog.Logger
	TrustProxy bool

	routes    []string
	authFails *failLimiter
}

type apiFn func(w http.ResponseWriter, r *http.Request, p *Principal) error

func (a *API) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// guard authenticates, checks the coarse permission and resolves groups. Credentials are checked
// first and always: only a request with invalid credentials counts as a failure, so a valid token
// is never refused because of other requests from its address.
func (a *API) guard(perm string, fn apiFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, a.TrustProxy)
		tok, ok := bearerToken(r)
		var p *Principal
		if ok {
			var err error
			if p, err = a.Auth.Authenticate(r.Context(), tok); err != nil {
				a.log().Error("authentication unavailable", "route", r.Pattern)
				w.Header().Set("Retry-After", "5")
				problem(w, http.StatusServiceUnavailable, "authentication is unavailable")
				return
			}
		}
		if p == nil {
			a.authFails.fail(ip)
			if a.authFails.blocked(ip) {
				w.Header().Set("Retry-After", "60")
				problem(w, http.StatusTooManyRequests, "too many failed attempts")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="files"`)
			problem(w, http.StatusUnauthorized, "a valid bearer token is required")
			return
		}
		if !p.Has(perm) {
			problem(w, http.StatusForbidden, "this needs the "+perm+" permission")
			return
		}
		if a.Groups != nil {
			gs, err := a.Groups.Groups(r.Context(), p.Subject)
			if err != nil {
				a.log().Error("group lookup unavailable", "route", r.Pattern)
				w.Header().Set("Retry-After", "5")
				problem(w, http.StatusServiceUnavailable, "the directory is unavailable")
				return
			}
			p.Groups = gs
		}
		if err := fn(w, r, p); err != nil {
			a.fail(w, r, err)
		}
	}
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe), errors.Is(err, ErrTooLarge):
		problem(w, http.StatusRequestEntityTooLarge, "the request is too large")
	case errors.Is(err, ErrQuota):
		problem(w, http.StatusRequestEntityTooLarge, publicMsg(err))
	case errors.Is(err, ErrNotFound):
		problem(w, http.StatusNotFound, "not found") // one body for every kind of "no": invisible, missing, malformed id
	case errors.Is(err, ErrForbidden):
		problem(w, http.StatusForbidden, "your role does not allow this")
	case errors.Is(err, ErrInvalid):
		problem(w, http.StatusBadRequest, publicMsg(err))
	case errors.Is(err, ErrConflict):
		problem(w, http.StatusConflict, publicMsg(err))
	case errors.Is(err, ErrType):
		problem(w, http.StatusUnsupportedMediaType, publicMsg(err))
	case errors.Is(err, ErrInfected):
		problem(w, http.StatusUnprocessableEntity, "the file was refused by the virus scanner")
	case errors.Is(err, ErrRate):
		w.Header().Set("Retry-After", "60")
		problem(w, http.StatusTooManyRequests, "too many failed attempts")
	case errors.Is(err, ErrUnavailable):
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusServiceUnavailable, publicMsg(err))
	case errors.Is(err, ErrIntegrity):
		a.log().Error("stored content failed its checksum", "route", r.Pattern)
		problem(w, http.StatusInternalServerError, "stored content failed its checksum")
	default:
		// the route pattern, never the URL: a share token is part of the path
		a.log().Error("request failed", "route", r.Pattern, "error", err)
		problem(w, http.StatusInternalServerError, "internal error")
	}
}

// publicMsg is the part of a wrapped sentinel error that is safe to show (messages are written by
// this package and carry no personal data or secrets).
func publicMsg(err error) string {
	s := err.Error()
	for _, sent := range []error{ErrInvalid, ErrConflict, ErrForbidden, ErrQuota, ErrType, ErrUnavailable} {
		s = strings.TrimPrefix(s, sent.Error()+": ")
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 { // errors.Join
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return invalid("Content-Type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return invalid("the body is not valid for this request")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("unexpected data after the JSON body")
	}
	return nil
}

// Handler returns the full service handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.routes = nil
	if a.Store != nil {
		a.authFails = newFailLimiter(authFailsPerMin, a.Store.now)
	}
	route := func(pattern string, h http.HandlerFunc) {
		a.routes = append(a.routes, pattern)
		mux.HandleFunc(pattern, h)
	}
	route("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	route("GET /s/{token}", a.shareDownload)
	route("POST /v1/spaces", a.guard(PermWrite, a.createSpace))
	route("GET /v1/spaces", a.guard(PermRead, a.listSpaces))
	route("GET /v1/spaces/{id}", a.guard(PermRead, a.getSpace))
	route("GET /v1/spaces/{id}/members", a.guard(PermRead, a.listMembers))
	route("PUT /v1/spaces/{id}/members", a.guard(PermWrite, a.setMember))
	route("DELETE /v1/spaces/{id}/members/{kind}/{principal}", a.guard(PermWrite, a.removeMember))
	route("POST /v1/spaces/{id}/folders", a.guard(PermWrite, a.createFolder))
	route("GET /v1/spaces/{id}/folders", a.guard(PermRead, a.listFolders))
	route("DELETE /v1/folders/{id}", a.guard(PermWrite, a.deleteFolder))
	route("POST /v1/spaces/{id}/files", a.guard(PermWrite, a.upload))
	route("GET /v1/spaces/{id}/files", a.guard(PermRead, a.listFiles))
	route("GET /v1/files", a.guard(PermRead, a.listFiles))
	route("GET /v1/files/{id}", a.guard(PermRead, a.getFile))
	route("PATCH /v1/files/{id}", a.guard(PermWrite, a.patchFile))
	route("DELETE /v1/files/{id}", a.guard(PermWrite, a.deleteFile))
	route("POST /v1/files/{id}/restore", a.guard(PermWrite, a.restoreFile))
	route("GET /v1/files/{id}/content", a.guard(PermRead, a.content))
	route("GET /v1/files/{id}/versions", a.guard(PermRead, a.listVersions))
	route("POST /v1/files/{id}/versions", a.guard(PermWrite, a.uploadVersion))
	route("GET /v1/files/{id}/shares", a.guard(PermRead, a.listShares))
	route("POST /v1/files/{id}/shares", a.guard(PermWrite, a.createShare))
	route("DELETE /v1/shares/{id}", a.guard(PermWrite, a.revokeShare))
	route("PUT /v1/files/{id}/hold", a.guard(PermAdmin, a.setHold))
	route("GET /v1/admin/files", a.guard(PermAdmin, a.adminFiles))
	route("POST /v1/subjects/{subject}/erasure", a.guard(PermAdmin, a.erase))
	route("GET /v1/erasures", a.guard(PermAdmin, a.listErasures))
	route("POST /v1/purge", a.guard(PermAdmin, a.purge))
	route("POST /v1/retention/run", a.guard(PermAdmin, a.runRetention))
	route("GET /v1/audit", a.guard(PermAdmin, a.audit))
	return obs.Instrument(securityHeaders(mux)) // request metrics and /readyz (epic E22)
}

// Routes lists the "METHOD /path" patterns the service serves; a contract test compares them to
// the OpenAPI document.
func Routes() []string {
	a := &API{}
	a.Handler()
	return a.routes
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		h.ServeHTTP(w, r)
	})
}

// ---- helpers -------------------------------------------------------------------------------------

func intQuery(r *http.Request, key string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(key))
	return n
}

// onlyParams refuses a query string with a parameter the route does not define.
func onlyParams(r *http.Request, allowed ...string) error {
	for k := range r.URL.Query() {
		ok := false
		for _, a := range allowed {
			ok = ok || a == k
		}
		if !ok {
			return invalid("unknown query parameter")
		}
	}
	return nil
}

func ifMatch(r *http.Request) int {
	v := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	n, _ := strconv.Atoi(v)
	return n
}

// ---- spaces, members, folders --------------------------------------------------------------------

func (a *API) createSpace(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	sp, err := a.Store.CreateSpace(r.Context(), p, in.Name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, sp)
	return nil
}

func (a *API) listSpaces(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := onlyParams(r, "cursor", "limit"); err != nil {
		return err
	}
	pg, err := a.Store.ListSpaces(r.Context(), p, r.URL.Query().Get("cursor"), intQuery(r, "limit"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pg)
	return nil
}

func (a *API) getSpace(w http.ResponseWriter, r *http.Request, p *Principal) error {
	sp, err := a.Store.GetSpace(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, sp)
	return nil
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ms, err := a.Store.ListMembers(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ms})
	return nil
}

func (a *API) setMember(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var m Member
	if err := readJSON(w, r, &m); err != nil {
		return err
	}
	if err := a.Store.SetMember(r.Context(), p, r.PathValue("id"), m); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request, p *Principal) error {
	id, err := url.PathUnescape(r.PathValue("principal"))
	if err != nil {
		return invalid("bad principal")
	}
	if err := a.Store.RemoveMember(r.Context(), p, r.PathValue("id"), r.PathValue("kind"), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) createFolder(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	f, err := a.Store.CreateFolder(r.Context(), p, r.PathValue("id"), in.ParentID, in.Name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, f)
	return nil
}

func (a *API) listFolders(w http.ResponseWriter, r *http.Request, p *Principal) error {
	fs, err := a.Store.ListFolders(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": fs})
	return nil
}

func (a *API) deleteFolder(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := a.Store.DeleteFolder(r.Context(), p, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- files ---------------------------------------------------------------------------------------

// body returns the upload body, bounded. A declared length over the limit is refused before a
// byte is read; the Store enforces the same limit while streaming.
func (a *API) body(w http.ResponseWriter, r *http.Request) (io.Reader, error) {
	max := a.Store.cfg.Limits.MaxFileBytes
	if r.ContentLength > max {
		return nil, ErrTooLarge
	}
	// one deadline for the whole upload: a client that trickles bytes cannot hold a slot
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(a.Store.cfg.UploadDeadline()))
	return http.MaxBytesReader(w, r.Body, max+1), nil
}

func (a *API) upload(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := onlyParams(r, "name", "description", "folder", "subject", "ref"); err != nil {
		return err
	}
	body, err := a.body(w, r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), a.Store.cfg.UploadDeadline()+a.Store.cfg.UploadDeadline()/2)
	defer cancel()
	f, err := a.Store.Upload(ctx, p, UploadRequest{SpaceID: r.PathValue("id"), FolderID: q.Get("folder"), Name: q.Get("name"), Description: q.Get("description"),
		Subject: q.Get("subject"), Ref: q.Get("ref"), Body: body})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, f)
	return nil
}

func (a *API) uploadVersion(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := onlyParams(r); err != nil {
		return err
	}
	body, err := a.body(w, r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.Store.cfg.UploadDeadline()+a.Store.cfg.UploadDeadline()/2)
	defer cancel()
	f, err := a.Store.UploadVersion(ctx, p, r.PathValue("id"), body)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, f)
	return nil
}

func (a *API) listFiles(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := onlyParams(r, "folder", "ref", "deleted", "cursor", "limit"); err != nil {
		return err
	}
	q := r.URL.Query()
	pg, err := a.Store.ListFiles(r.Context(), p, ListQuery{SpaceID: r.PathValue("id"), FolderID: q.Get("folder"), Ref: q.Get("ref"),
		Cursor: q.Get("cursor"), Limit: intQuery(r, "limit"), Deleted: q.Get("deleted") == "1" || q.Get("deleted") == "true"})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pg)
	return nil
}

func (a *API) getFile(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := a.Store.GetFile(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(f.Rev)+`"`)
	writeJSON(w, http.StatusOK, f)
	return nil
}

func (a *API) patchFile(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		FolderID    *string `json:"folder_id"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	f, err := a.Store.UpdateFile(r.Context(), p, r.PathValue("id"), FilePatch{Name: in.Name, Description: in.Description, FolderID: in.FolderID, Rev: ifMatch(r)})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(f.Rev)+`"`)
	writeJSON(w, http.StatusOK, f)
	return nil
}

func (a *API) deleteFile(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := a.Store.DeleteFile(r.Context(), p, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) restoreFile(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := a.Store.RestoreFile(r.Context(), p, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) listVersions(w http.ResponseWriter, r *http.Request, p *Principal) error {
	vs, err := a.Store.ListVersions(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": vs})
	return nil
}

// serve writes a Content: always an attachment of application/octet-stream, nosniff, sandboxed,
// not cached. Inline display exists only for the raster image types the operator listed, only when
// the caller asks, and still sandboxed. If the stored bytes fail their checksum the connection is
// aborted: the declared length is never reached, so the client sees a failed transfer.
func (a *API) serve(w http.ResponseWriter, r *http.Request, c *Content) error {
	defer func() { _ = c.Body.Close() }()
	h := w.Header()
	inline := c.InlineOK && r.URL.Query().Get("inline") == "1"
	if inline {
		h.Set("Content-Type", c.StoredType)
	} else {
		h.Set("Content-Type", c.ContentType)
	}
	h.Set("Content-Disposition", ContentDisposition(c.Name, inline))
	h.Set("Content-Length", strconv.FormatInt(c.Size, 10))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Referrer-Policy", "no-referrer")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadDeadline))
	w.WriteHeader(http.StatusOK)
	if err := c.Stream(r.Context(), w); err != nil {
		if errors.Is(err, ErrIntegrity) {
			a.log().Error("download aborted: stored content failed its checksum", "route", r.Pattern, "file_id", c.FileID)
		}
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (a *API) content(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := onlyParams(r, "version", "inline"); err != nil {
		return err
	}
	c, err := a.Store.OpenContent(r.Context(), p, r.PathValue("id"), intQuery(r, "version"))
	if err != nil {
		return err
	}
	return a.serve(w, r, c)
}

// shareDownload is the public route of a share link. The token is in the path and nowhere else:
// it is never logged (errors and metrics carry the route pattern), never echoed, and the answer
// to anything wrong with it is the same 404.
func (a *API) shareDownload(w http.ResponseWriter, r *http.Request) {
	c, err := a.Store.OpenShare(r.Context(), r.PathValue("token"), clientIP(r, a.TrustProxy))
	if err == nil {
		err = a.serve(w, r, c)
	}
	if err != nil {
		a.fail(w, r, err)
	}
}

// ---- shares --------------------------------------------------------------------------------------

func (a *API) createShare(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		ExpiresIn string `json:"expires_in"`
		OneTime   bool   `json:"one_time"`
		Label     string `json:"label"`
		Version   int    `json:"version"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	var ttl time.Duration
	if in.ExpiresIn != "" {
		var err error
		if ttl, err = time.ParseDuration(in.ExpiresIn); err != nil {
			return invalid("expires_in must be a duration such as 24h")
		}
	}
	tok, sh, err := a.Store.CreateShare(r.Context(), p, r.PathValue("id"), ShareRequest{TTL: ttl, OneTime: in.OneTime, Label: in.Label, Version: in.Version})
	if err != nil {
		return err
	}
	// the only time the token is ever shown
	base := strings.TrimRight(a.Store.cfg.Share.BaseURL, "/")
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "url": base + "/s/" + tok, "share": sh})
	return nil
}

func (a *API) listShares(w http.ResponseWriter, r *http.Request, p *Principal) error {
	shs, err := a.Store.ListShares(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": shs})
	return nil
}

func (a *API) revokeShare(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := a.Store.RevokeShare(r.Context(), p, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- administration ------------------------------------------------------------------------------

func (a *API) setHold(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		LegalHold   bool    `json:"legal_hold"`
		RetainUntil *string `json:"retain_until"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	var until *time.Time
	if in.RetainUntil != nil && *in.RetainUntil != "" {
		t, err := time.Parse(time.DateOnly, *in.RetainUntil)
		if err != nil {
			if t, err = time.Parse(time.RFC3339, *in.RetainUntil); err != nil {
				return invalid("retain_until is a date (2027-01-31) or an RFC 3339 time")
			}
		}
		until = &t
	}
	if err := a.Store.SetHold(r.Context(), p, r.PathValue("id"), in.LegalHold, until); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) adminFiles(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := onlyParams(r, "subject", "cursor", "limit"); err != nil {
		return err
	}
	q := r.URL.Query()
	pg, err := a.Store.AdminFiles(r.Context(), p, q.Get("subject"), q.Get("cursor"), intQuery(r, "limit"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pg)
	return nil
}

func (a *API) erase(w http.ResponseWriter, r *http.Request, p *Principal) error {
	er, err := a.Store.EraseSubject(r.Context(), p, r.PathValue("subject"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, er)
	return nil
}

func (a *API) listErasures(w http.ResponseWriter, r *http.Request, p *Principal) error {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	es, err := a.Store.ListErasures(r.Context(), p, after)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": es})
	return nil
}

func (a *API) purge(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var in struct {
		Subject string `json:"subject"`
		Class   string `json:"class"`
	}
	if err := readJSON(w, r, &in); err != nil {
		return err
	}
	if err := a.Store.PurgeSubject(r.Context(), p, in.Subject, in.Class); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) runRetention(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if !p.Has(PermAdmin) {
		return ErrForbidden
	}
	rep, err := a.Store.RunRetention(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rep)
	return nil
}

func (a *API) audit(w http.ResponseWriter, r *http.Request, p *Principal) error {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	pg, err := a.Store.ListAudit(r.Context(), p, before, intQuery(r, "limit"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pg)
	return nil
}

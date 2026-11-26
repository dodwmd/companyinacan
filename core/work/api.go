package work

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// api implements the `work` capability REST API (schemas/capabilities/work.v1.json,
// specs/011-work-tracker/contracts/openapi.yaml).
type api struct {
	store       *Store
	auth        Authenticator
	log         *slog.Logger
	maxBody     int64
	trustProxy  bool
	limiter     *rateLimiter         // per principal
	failures    *rateLimiter         // failed authentications per client address
	penalty     time.Duration        // delay added to failures from an address over the limit
	ui          *webUI               // the web UI under /ui/ (ui.go)
	forceSecure bool                 // UI cookies are always Secure
	tokenLogin  bool                 // the pasted-token sign-in is offered (ui.token_login)
	sso         modclient.SessionAPI // launch-ticket sign-in through can-core; nil when not installed under it
}

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unhealthy", "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	read, write, admin := PermRead, PermWrite, PermAdmin

	mux.HandleFunc("GET /v1/me", a.guard(read, a.me))

	mux.HandleFunc("GET /v1/projects", a.guard(read, a.listProjects))
	mux.HandleFunc("POST /v1/projects", a.guard(admin, a.createProject))
	mux.HandleFunc("GET /v1/projects/{key}", a.guard(read, a.getProject))
	mux.HandleFunc("PATCH /v1/projects/{key}", a.guard(admin, a.patchProject))
	mux.HandleFunc("POST /v1/projects/{key}/issues", a.guard(write, a.createIssue))
	mux.HandleFunc("POST /v1/projects/{key}/issues/bulk", a.guard(write, a.bulkCreate))
	mux.HandleFunc("POST /v1/projects/{key}/issues/import", a.guard(admin, a.importIssues))
	mux.HandleFunc("GET /v1/projects/{key}/board", a.guard(read, a.board))
	mux.HandleFunc("GET /v1/projects/{key}/sprints", a.guard(read, a.listSprints))
	mux.HandleFunc("POST /v1/projects/{key}/sprints", a.guard(write, a.createSprint))

	mux.HandleFunc("GET /v1/sprints/{id}", a.guard(read, a.getSprint))
	mux.HandleFunc("PATCH /v1/sprints/{id}", a.guard(write, a.patchSprint))
	mux.HandleFunc("POST /v1/sprints/{id}/start", a.guard(write, a.startSprint))
	mux.HandleFunc("POST /v1/sprints/{id}/complete", a.guard(write, a.completeSprint))

	mux.HandleFunc("GET /v1/issues", a.guard(read, a.listIssues))
	mux.HandleFunc("POST /v1/issues/bulk-update", a.guard(write, a.bulkUpdate))
	mux.HandleFunc("GET /v1/issues/{key}", a.guard(read, a.getIssue))
	mux.HandleFunc("PATCH /v1/issues/{key}", a.guard(write, a.patchIssue))
	mux.HandleFunc("GET /v1/issues/{key}/transitions", a.guard(read, a.allowedTransitions))
	mux.HandleFunc("POST /v1/issues/{key}/transitions", a.guard(write, a.transition))
	mux.HandleFunc("POST /v1/issues/{key}/rank", a.guard(write, a.rank))
	mux.HandleFunc("GET /v1/issues/{key}/comments", a.guard(read, a.listComments))
	mux.HandleFunc("POST /v1/issues/{key}/comments", a.guard(write, a.addComment))
	mux.HandleFunc("GET /v1/issues/{key}/activity", a.guard(read, a.listActivity))
	mux.HandleFunc("POST /v1/issues/{key}/links", a.guard(write, a.addLink))
	mux.HandleFunc("DELETE /v1/issues/{key}/links/{id}", a.guard(write, a.removeLink))
	mux.HandleFunc("PUT /v1/issues/{key}/watchers/{subject}", a.guard(write, a.watch(true)))
	mux.HandleFunc("DELETE /v1/issues/{key}/watchers/{subject}", a.guard(write, a.watch(false)))
	mux.HandleFunc("POST /v1/issues/{key}/attachments", a.guard(write, a.addAttachment))
	mux.HandleFunc("DELETE /v1/issues/{key}/attachments/{id}", a.guard(write, a.removeAttachment))

	mux.HandleFunc("GET /v1/tokens", a.guard(admin, a.listTokens))
	mux.HandleFunc("POST /v1/tokens", a.guard(admin, a.createToken))
	mux.HandleFunc("DELETE /v1/tokens/{id}", a.guard(admin, a.revokeToken))
	mux.HandleFunc("GET /v1/audit", a.guard(admin, a.listAudit))
	mux.HandleFunc("GET /v1/events/dead", a.guard(admin, a.listDead))
	mux.HandleFunc("POST /v1/events/{id}/replay", a.guard(admin, a.replayEvent))

	if a.ui != nil {
		a.ui.mount(mux)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		mux.ServeHTTP(w, r)
	})
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, p *Principal)

// guard authenticates, rate-limits, checks the permission and caps the body before the
// handler runs. Every request is authenticated, so a valid token always works: an attacker
// behind a shared address (an ingress, a NAT) cannot lock legitimate callers out. Only
// failures are counted per client address; past the limit a failure is answered 429 after a
// short delay, which makes a flood of guesses slow and costly without affecting valid callers.
func (a *api) guard(perm string, next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.Authenticate(r)
		if err != nil {
			ip := clientIP(r, a.trustProxy)
			if ok, wait := a.failures.allow(ip); !ok {
				select { // the cost: guessing from an address that is over the limit gets slower
				case <-time.After(a.penalty):
				case <-r.Context().Done():
				}
				tooMany(w, wait)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="can-work"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required")
			return
		}
		key := p.TokenID
		if key == "" {
			key = "subject:" + p.Subject
		}
		if ok, wait := a.limiter.allow(key); !ok {
			tooMany(w, wait)
			return
		}
		if !p.Can(perm) {
			writeError(w, http.StatusForbidden, "forbidden", "missing scope "+perm)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, a.maxBody)
		next(w, r, p)
	}
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
}

func (a *api) fail(w http.ResponseWriter, err error) { writeStoreError(w, a.log, err) }

func queryInt(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

// ---- me, projects ---------------------------------------------------------------------

func (a *api) me(w http.ResponseWriter, _ *http.Request, p *Principal) {
	projects := p.Projects
	if projects == nil {
		projects = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subject": p.Subject, "kind": p.Kind, "token_id": p.TokenID, "scopes": p.Permissions, "projects": projects})
}

func (a *api) listProjects(w http.ResponseWriter, r *http.Request, p *Principal) {
	ps, err := a.store.ListProjects(r.Context(), p)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ps})
}

func (a *api) createProject(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewProject
	if !decodeJSON(w, r, &in) {
		return
	}
	pr, err := a.store.CreateProject(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pr)
}

func (a *api) getProject(w http.ResponseWriter, r *http.Request, p *Principal) {
	pr, err := a.store.GetProject(r.Context(), p, r.PathValue("key"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

func (a *api) patchProject(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in ProjectPatch
	if !decodeJSON(w, r, &in) {
		return
	}
	pr, err := a.store.UpdateProject(r.Context(), p, r.PathValue("key"), in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

// ---- issues ---------------------------------------------------------------------------

func validIdemHeader(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return "", true
	}
	if len(k) < 8 || len(k) > 128 || strings.ContainsAny(k, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "bad_request", "Idempotency-Key must be 8 to 128 characters")
		return "", false
	}
	return k, true
}

func (a *api) createIssue(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in CreateIssue
	if !decodeJSON(w, r, &in) {
		return
	}
	is, replayed, err := a.store.CreateIssue(r.Context(), p, r.PathValue("key"), in, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, is.Version)
	w.Header().Set("Location", "/v1/issues/"+is.Key)
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, is)
		return
	}
	writeJSON(w, http.StatusCreated, is)
}

func (a *api) bulkCreate(w http.ResponseWriter, r *http.Request, p *Principal) {
	idem, ok := validIdemHeader(w, r)
	if !ok {
		return
	}
	var in struct {
		Issues []CreateIssue `json:"issues"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	out, replayed, err := a.store.BulkCreate(r.Context(), p, r.PathValue("key"), in.Issues, idem)
	if err != nil {
		a.fail(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"items": out})
}

func (a *api) listIssues(w http.ResponseWriter, r *http.Request, p *Principal) {
	q := r.URL.Query()
	off, ok := decodeCursor(q.Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
		return
	}
	items, next, err := a.store.ListIssues(r.Context(), p, q.Get("project"), q.Get("q"), q.Get("sort"), queryInt(r, "limit"), int(off))
	if err != nil {
		a.fail(w, err)
		return
	}
	resp := map[string]any{"items": items}
	if next >= 0 {
		resp["next_cursor"] = encodeCursor(int64(next))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) getIssue(w http.ResponseWriter, r *http.Request, p *Principal) {
	d, err := a.store.GetIssue(r.Context(), p, r.PathValue("key"))
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, d.Version)
	writeJSON(w, http.StatusOK, d)
}

func (a *api) patchIssue(w http.ResponseWriter, r *http.Request, p *Principal) {
	ifMatch, ok := parseIfMatch(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match must be an issue version such as \"3\"")
		return
	}
	var pt Patch
	if !decodeJSON(w, r, &pt) {
		return
	}
	is, err := a.store.UpdateIssue(r.Context(), p, r.PathValue("key"), &pt, ifMatch)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, is.Version)
	writeJSON(w, http.StatusOK, is)
}

func (a *api) allowedTransitions(w http.ResponseWriter, r *http.Request, p *Principal) {
	next, err := a.store.AllowedTransitions(r.Context(), p, r.PathValue("key"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed": next})
}

func (a *api) transition(w http.ResponseWriter, r *http.Request, p *Principal) {
	ifMatch, ok := parseIfMatch(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match must be an issue version such as \"3\"")
		return
	}
	var in struct {
		To string `json:"to"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	is, err := a.store.Transition(r.Context(), p, r.PathValue("key"), in.To, ifMatch)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, is.Version)
	writeJSON(w, http.StatusOK, is)
}

func (a *api) rank(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in RankRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	is, err := a.store.Rank(r.Context(), p, r.PathValue("key"), in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, is.Version)
	writeJSON(w, http.StatusOK, is)
}

func (a *api) bulkUpdate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Keys       []string `json:"keys"`
		Set        *Patch   `json:"set"`
		Transition string   `json:"transition"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	res, err := a.store.BulkUpdate(r.Context(), p, in.Keys, in.Set, in.Transition)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// ---- comments, activity, links, watchers, attachments ---------------------------------

func (a *api) addComment(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := a.store.AddComment(r.Context(), p, r.PathValue("key"), in.Body)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (a *api) listComments(w http.ResponseWriter, r *http.Request, p *Principal) {
	after, ok := decodeCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
		return
	}
	items, next, err := a.store.ListComments(r.Context(), p, r.PathValue("key"), after, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	resp := map[string]any{"items": items}
	if next > 0 {
		resp["next_cursor"] = encodeCursor(next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) listActivity(w http.ResponseWriter, r *http.Request, p *Principal) {
	after, ok := decodeCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
		return
	}
	items, next, err := a.store.ListActivity(r.Context(), p, r.PathValue("key"), after, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	resp := map[string]any{"items": items}
	if next > 0 {
		resp["next_cursor"] = encodeCursor(next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) addLink(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Type   string `json:"type"`
		Target string `json:"target"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	l, err := a.store.AddLink(r.Context(), p, r.PathValue("key"), in.Type, in.Target)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func pathInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n < 1 {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return 0, false
	}
	return n, true
}

func (a *api) removeLink(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := a.store.RemoveLink(r.Context(), p, r.PathValue("key"), id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) watch(on bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) {
		if err := a.store.SetWatcher(r.Context(), p, r.PathValue("key"), r.PathValue("subject"), on); err != nil {
			a.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *api) addAttachment(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewAttachment
	if !decodeJSON(w, r, &in) {
		return
	}
	at, err := a.store.AddAttachment(r.Context(), p, r.PathValue("key"), in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, at)
}

func (a *api) removeAttachment(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := a.store.RemoveAttachment(r.Context(), p, r.PathValue("key"), id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- sprints and board ----------------------------------------------------------------

func (a *api) listSprints(w http.ResponseWriter, r *http.Request, p *Principal) {
	ss, err := a.store.ListSprints(r.Context(), p, r.PathValue("key"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ss})
}

func (a *api) createSprint(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewSprint
	if !decodeJSON(w, r, &in) {
		return
	}
	sp, err := a.store.CreateSprint(r.Context(), p, r.PathValue("key"), in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sp)
}

func (a *api) getSprint(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	sp, err := a.store.GetSprint(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (a *api) patchSprint(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var in SprintPatch
	if !decodeJSON(w, r, &in) {
		return
	}
	sp, err := a.store.UpdateSprint(r.Context(), p, id, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (a *api) startSprint(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	sp, err := a.store.StartSprint(r.Context(), p, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (a *api) completeSprint(w http.ResponseWriter, r *http.Request, p *Principal) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		MoveTo *int64 `json:"move_to"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	sp, err := a.store.CompleteSprint(r.Context(), p, id, in.MoveTo)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sp)
}

func (a *api) board(w http.ResponseWriter, r *http.Request, p *Principal) {
	b, err := a.store.GetBoard(r.Context(), p, r.PathValue("key"), r.URL.Query().Get("sprint"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ---- tokens and audit (admin) ---------------------------------------------------------

func (a *api) listTokens(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !p.Unscoped() {
		a.fail(w, ErrForbidden)
		return
	}
	ts, err := a.store.ListTokens(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ts})
}

func (a *api) createToken(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewToken
	if !decodeJSON(w, r, &in) {
		return
	}
	info, secret, err := a.store.CreateToken(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": secret, "info": info, "note": "store the token now; it is not shown again"})
}

func (a *api) revokeToken(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !p.Unscoped() {
		a.fail(w, ErrForbidden)
		return
	}
	if err := a.store.RevokeToken(r.Context(), p, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listAudit(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !p.Unscoped() {
		a.fail(w, ErrForbidden)
		return
	}
	before, ok := decodeCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
		return
	}
	limit := clampLimit(queryInt(r, "limit"))
	items, err := a.store.ListAudit(r.Context(), before, limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	resp := map[string]any{"items": items}
	if len(items) == limit {
		resp["next_cursor"] = encodeCursor(items[len(items)-1].ID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) listDead(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !p.Unscoped() {
		a.fail(w, ErrForbidden)
		return
	}
	after, ok := decodeCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
		return
	}
	items, next, err := a.store.ListDeadEvents(r.Context(), after, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	resp := map[string]any{"items": items}
	if next > 0 {
		resp["next_cursor"] = encodeCursor(next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *api) replayEvent(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !p.Unscoped() {
		a.fail(w, ErrForbidden)
		return
	}
	if err := a.store.ReplayEvent(r.Context(), p, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

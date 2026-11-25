package jobs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// api implements the REST API (schemas/capabilities/jobs.v1.json and ats.v1.json,
// specs/013-jobs-recruiting/contracts/openapi.yaml). Public careers routes live in public.go.
type api struct {
	store      *Store
	auth       Authenticator
	log        *slog.Logger
	maxBody    int64
	trustProxy bool
	dbTimeout  time.Duration
	limiter    *rateLimiter  // per principal
	failures   *rateLimiter  // failed authentications per client address
	penalty    time.Duration // delay added to failures from an address over the limit
}

func (a *api) routes(mux *http.ServeMux) {
	g := a.guard
	mux.HandleFunc("GET /v1/me", g("", a.me))

	mux.HandleFunc("GET /v1/jobs", g(ActJobRead, a.listJobs))
	mux.HandleFunc("POST /v1/jobs", g(ActJobWrite, a.createJob))
	mux.HandleFunc("GET /v1/jobs/{id}", g(ActJobRead, a.getJob))
	mux.HandleFunc("PATCH /v1/jobs/{id}", g(ActJobWrite, a.patchJob))
	mux.HandleFunc("POST /v1/jobs/{id}/transitions", g(ActJobWrite, a.transitionJob))
	mux.HandleFunc("POST /v1/jobs/{id}/approval-requests", g(ActJobWrite, a.requestApproval))
	mux.HandleFunc("POST /v1/jobs/{id}/approval", g(ActApprove, a.decideApproval))
	mux.HandleFunc("GET /v1/jobs/{id}/syndication", g(ActJobRead, a.jobSyndication))
	mux.HandleFunc("GET /v1/boards", g(ActJobRead, a.boards))

	mux.HandleFunc("GET /v1/stages", g("", a.listStages))
	mux.HandleFunc("PUT /v1/stages", g(ActStages, a.setStages))

	mux.HandleFunc("GET /v1/applications", g(ActAppRead, a.listApplications))
	mux.HandleFunc("POST /v1/applications", g(ActAppCreate, a.createApplication))
	mux.HandleFunc("GET /v1/applications/{id}", g(ActAppRead, a.getApplication))
	mux.HandleFunc("POST /v1/applications/{id}/move", g(ActAppMove, a.moveApplication))
	mux.HandleFunc("POST /v1/applications/{id}/reject", g(ActAppMove, a.rejectApplication))
	mux.HandleFunc("POST /v1/applications/{id}/hire", g(ActHire, a.hireApplication))
	mux.HandleFunc("GET /v1/applications/{id}/resume", g(ActAppRead, a.downloadResume))
	mux.HandleFunc("PUT /v1/applications/{id}/interviewers/{subject}", g(ActAssign, a.assignInterviewer))
	mux.HandleFunc("DELETE /v1/applications/{id}/interviewers/{subject}", g(ActAssign, a.removeInterviewer))
	mux.HandleFunc("GET /v1/applications/{id}/scorecards", g(ActScore, a.listScorecards))
	mux.HandleFunc("PUT /v1/applications/{id}/scorecards/mine", g(ActScore, a.saveScorecard))
	mux.HandleFunc("POST /v1/applications/{id}/scorecards/mine/submit", g(ActScore, a.submitScorecard))

	mux.HandleFunc("GET /v1/candidates", g(ActCandidateRead, a.listCandidates))
	mux.HandleFunc("GET /v1/candidates/{id}", g(ActCandidateRead, a.getCandidate))
	mux.HandleFunc("PATCH /v1/candidates/{id}", g(ActCandidateEdit, a.patchCandidate))
	mux.HandleFunc("PUT /v1/candidates/{id}/talent-pool", g(ActTalentPool, a.setTalentPool))
	mux.HandleFunc("GET /v1/talent-pool", g(ActTalentPool, a.listTalentPool))
	mux.HandleFunc("PUT /v1/candidates/{id}/legal-hold", g(ActAdmin, a.setLegalHold))
	mux.HandleFunc("POST /v1/candidates/{id}/erasure", g(ActAdmin, a.erase))
	mux.HandleFunc("GET /v1/erasures", g(ActAdmin, a.listErasures))
	mux.HandleFunc("POST /v1/purge", g(ActAdmin, a.purge))
	mux.HandleFunc("POST /v1/retention/run", g(ActAdmin, a.runRetention))
	mux.HandleFunc("GET /v1/eeo-report", g(ActAdmin, a.eeoReport))

	mux.HandleFunc("GET /v1/tokens", g(ActAdmin, a.listTokens))
	mux.HandleFunc("POST /v1/tokens", g(ActAdmin, a.createToken))
	mux.HandleFunc("DELETE /v1/tokens/{id}", g(ActAdmin, a.revokeToken))
	mux.HandleFunc("GET /v1/audit", g(ActAdmin, a.listAudit))
	mux.HandleFunc("GET /v1/events/dead", g(ActAdmin, a.listDead))
	mux.HandleFunc("POST /v1/events/{id}/replay", g(ActAdmin, a.replayEvent))
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, p *Principal)

// guard authenticates, rate-limits, checks the action and bounds the request before the
// handler runs. Every request is authenticated first, so a valid token always works: an
// attacker behind a shared address (an ingress, a NAT) cannot lock legitimate callers out.
// Only failures are counted per client address; past the limit a failure is answered 429
// after a short delay, which makes a flood of guesses slow and costly without ever affecting
// valid callers. Database work for the request is bounded by the request deadline.
func (a *api) guard(act Action, next handlerFunc) http.HandlerFunc {
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
			w.Header().Set("WWW-Authenticate", `Bearer realm="can-jobs"`)
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
		ctx, cancel := context.WithTimeout(r.Context(), a.dbTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if act != "" && !p.Can(act) {
			a.store.auditDeny(ctx, p, "deny", r.Method+" "+r.Pattern)
			writeError(w, http.StatusForbidden, "forbidden", "this token cannot do that")
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

func (a *api) cursor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	c, ok := decodeCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor")
	}
	return c, ok
}

func page(w http.ResponseWriter, items any, next int64) {
	out := map[string]any{"items": items}
	if next > 0 {
		out["next_cursor"] = encodeCursor(next)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) me(w http.ResponseWriter, _ *http.Request, p *Principal) {
	jobs := p.Jobs
	if jobs == nil {
		jobs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subject": p.Subject, "kind": p.Kind, "token_id": p.TokenID, "role": p.Role, "jobs": jobs})
}

// ---- jobs ----------------------------------------------------------------------------------

func (a *api) listJobs(w http.ResponseWriter, r *http.Request, p *Principal) {
	cur, ok := a.cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	js, next, err := a.store.ListJobs(r.Context(), p, JobFilter{Status: q.Get("status"), Department: q.Get("department"), Cursor: cur, Limit: queryInt(r, "limit")})
	if err != nil {
		a.fail(w, err)
		return
	}
	page(w, js, next)
}

func (a *api) createJob(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in NewJob
	if !decodeJSON(w, r, &in) {
		return
	}
	j, err := a.store.CreateJob(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, j.Version)
	writeJSON(w, http.StatusCreated, j)
}

func (a *api) getJob(w http.ResponseWriter, r *http.Request, p *Principal) {
	j, err := a.store.GetJob(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, j.Version)
	writeJSON(w, http.StatusOK, j)
}

func (a *api) patchJob(w http.ResponseWriter, r *http.Request, p *Principal) {
	im, ok := parseIfMatch(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "If-Match must be an ETag returned by this API")
		return
	}
	var in JobPatch
	if !decodeJSON(w, r, &in) {
		return
	}
	j, err := a.store.PatchJob(r.Context(), p, r.PathValue("id"), in, im)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, j.Version)
	writeJSON(w, http.StatusOK, j)
}

func (a *api) transitionJob(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	j, err := a.store.TransitionJob(r.Context(), p, r.PathValue("id"), in.To, in.Reason)
	if err != nil {
		a.fail(w, err)
		return
	}
	setETag(w, j.Version)
	writeJSON(w, http.StatusOK, j)
}

func (a *api) requestApproval(w http.ResponseWriter, r *http.Request, p *Principal) {
	j, err := a.store.RequestApproval(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (a *api) decideApproval(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Decision string `json:"decision"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Decision != "approve" && in.Decision != "reject" {
		a.fail(w, invalid("decision", "must be approve or reject"))
		return
	}
	j, err := a.store.DecideApproval(r.Context(), p, r.PathValue("id"), in.Decision == "approve")
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (a *api) jobSyndication(w http.ResponseWriter, r *http.Request, p *Principal) {
	bs, err := a.store.JobSyndication(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": bs})
}

func (a *api) boards(w http.ResponseWriter, _ *http.Request, _ *Principal) {
	writeJSON(w, http.StatusOK, map[string]any{"items": a.store.Boards()})
}

// ---- stages ---------------------------------------------------------------------------------

func (a *api) listStages(w http.ResponseWriter, r *http.Request, _ *Principal) {
	ss, err := a.store.ListStages(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ss})
}

func (a *api) setStages(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Stages []Stage `json:"stages"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ss, err := a.store.SetStages(r.Context(), p, in.Stages)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": ss})
}

// ---- applications ----------------------------------------------------------------------------

func (a *api) listApplications(w http.ResponseWriter, r *http.Request, p *Principal) {
	cur, ok := a.cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	vs, next, err := a.store.ListApplications(r.Context(), p, AppFilter{JobID: q.Get("job_id"), CandidateID: q.Get("candidate_id"), Stage: q.Get("stage"), Status: q.Get("status"), Cursor: cur, Limit: queryInt(r, "limit")})
	if err != nil {
		a.fail(w, err)
		return
	}
	page(w, vs, next)
}

func (a *api) createApplication(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in ManualApplication
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.CreateApplication(r.Context(), p, in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *api) getApplication(w http.ResponseWriter, r *http.Request, p *Principal) {
	v, err := a.store.GetApplication(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) moveApplication(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Stage string `json:"stage"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.MoveApplication(r.Context(), p, r.PathValue("id"), in.Stage)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) rejectApplication(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := a.store.RejectApplication(r.Context(), p, r.PathValue("id"), in.Reason, in.Note)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *api) hireApplication(w http.ResponseWriter, r *http.Request, p *Principal) {
	v, err := a.store.HireApplication(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// downloadResume streams the stored file as an attachment with a fixed, sniffed content type;
// nosniff and the CSP set for every response keep a hostile file from running in the browser.
func (a *api) downloadResume(w http.ResponseWriter, r *http.Request, p *Principal) {
	m, rc, err := a.store.OpenResume(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	h := w.Header()
	h.Set("Content-Type", m.Type)
	h.Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(m.Name, `"`, "")+`"`)
	h.Set("Content-Length", strconv.FormatInt(m.Size, 10))
	h.Set("X-Resume-Scan", m.Scan)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (a *api) assignInterviewer(w http.ResponseWriter, r *http.Request, p *Principal) {
	if err := a.store.AssignInterviewer(r.Context(), p, r.PathValue("id"), r.PathValue("subject")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) removeInterviewer(w http.ResponseWriter, r *http.Request, p *Principal) {
	if err := a.store.RemoveInterviewer(r.Context(), p, r.PathValue("id"), r.PathValue("subject")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listScorecards(w http.ResponseWriter, r *http.Request, p *Principal) {
	l, err := a.store.ListScorecards(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (a *api) saveScorecard(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in ScorecardInput
	if !decodeJSON(w, r, &in) {
		return
	}
	sc, err := a.store.SaveScorecard(r.Context(), p, r.PathValue("id"), in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (a *api) submitScorecard(w http.ResponseWriter, r *http.Request, p *Principal) {
	sc, err := a.store.SubmitScorecard(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

// ---- candidates ------------------------------------------------------------------------------

func (a *api) listCandidates(w http.ResponseWriter, r *http.Request, p *Principal) {
	cur, ok := a.cursor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	cs, next, err := a.store.ListCandidates(r.Context(), p, CandidateFilter{Email: q.Get("email"), Name: q.Get("name"), Tag: q.Get("tag"), Cursor: cur, Limit: queryInt(r, "limit")})
	if err != nil {
		a.fail(w, err)
		return
	}
	page(w, cs, next)
}

func (a *api) getCandidate(w http.ResponseWriter, r *http.Request, p *Principal) {
	c, err := a.store.GetCandidate(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *api) patchCandidate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in CandidatePatch
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := a.store.PatchCandidate(r.Context(), p, r.PathValue("id"), in)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *api) setTalentPool(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Member bool `json:"member"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := a.store.SetTalentPool(r.Context(), p, r.PathValue("id"), in.Member)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *api) listTalentPool(w http.ResponseWriter, r *http.Request, p *Principal) {
	cur, ok := a.cursor(w, r)
	if !ok {
		return
	}
	cs, next, err := a.store.ListTalentPool(r.Context(), p, r.URL.Query().Get("tag"), cur, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	page(w, cs, next)
}

func (a *api) setLegalHold(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Hold bool `json:"hold"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.store.SetLegalHold(r.Context(), p, r.PathValue("id"), in.Hold); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) erase(w http.ResponseWriter, r *http.Request, p *Principal) {
	er, err := a.store.RequestErasure(r.Context(), p, r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, er)
}

func (a *api) listErasures(w http.ResponseWriter, r *http.Request, p *Principal) {
	es, err := a.store.ListErasures(r.Context(), p, int64(queryInt(r, "after")))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": es})
}

func (a *api) purge(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		Subject string `json:"subject"`
		Class   string `json:"class"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := a.store.PurgeSubject(r.Context(), p, in.Subject, in.Class); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) runRetention(w http.ResponseWriter, r *http.Request, _ *Principal) {
	rep, err := a.store.RunRetention(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (a *api) eeoReport(w http.ResponseWriter, r *http.Request, p *Principal) {
	rep, err := a.store.EEOAggregate(r.Context(), p, r.URL.Query().Get("job_id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ---- admin -------------------------------------------------------------------------------------

func (a *api) listTokens(w http.ResponseWriter, r *http.Request, p *Principal) {
	ts, err := a.store.ListTokens(r.Context(), p)
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
	writeJSON(w, http.StatusCreated, map[string]any{"token": info, "secret": secret})
}

func (a *api) revokeToken(w http.ResponseWriter, r *http.Request, p *Principal) {
	if err := a.store.RevokeToken(r.Context(), p, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listAudit(w http.ResponseWriter, r *http.Request, _ *Principal) {
	cur, ok := a.cursor(w, r)
	if !ok {
		return
	}
	es, next, err := a.store.ListAudit(r.Context(), cur, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	page(w, es, next)
}

func (a *api) listDead(w http.ResponseWriter, r *http.Request, _ *Principal) {
	cur, ok := a.cursor(w, r)
	if !ok {
		return
	}
	ds, next, err := a.store.ListDeadEvents(r.Context(), cur, queryInt(r, "limit"))
	if err != nil {
		a.fail(w, err)
		return
	}
	page(w, ds, next)
}

func (a *api) replayEvent(w http.ResponseWriter, r *http.Request, p *Principal) {
	if err := a.store.ReplayEvent(r.Context(), p, r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package people

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 64 << 10

// API is the REST JSON surface (contract: specs/012-people-directory/contracts/people-api.yaml).
// It only maps HTTP to Service calls; every authorisation decision is made in the Service
// through the policy layer.
type API struct {
	Svc  *Service
	Auth Authenticator
	Log  *slog.Logger
	// Can, when set, serves can-core's signed event deliveries at POST /v1/can/events (it
	// authenticates by the HMAC signature, not by a token, and is not part of the API contract).
	Can http.Handler
	// TrustProxy counts failed authentications against the client address the ingress appends
	// to X-Forwarded-For. Without it, behind a proxy, all callers share one allowance.
	TrustProxy bool

	routes []string

	once    sync.Once
	failIP  *rateLimiter // failed authentications
	perUser *rateLimiter // authenticated requests per subject
}

func (a *API) limiters() {
	a.once.Do(func() {
		a.failIP = newRateLimiter(20, 1, 10000, nil)
		a.perUser = newRateLimiter(120, 20, 10000, nil)
	})
}

// clientIP is the address failed authentications are counted against. Behind a trusted proxy it is
// the last X-Forwarded-For entry, the one the proxy wrote; anything earlier is the client's claim.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// Header lines, not Get: Get returns the first line, which the client may have sent.
		if vs := r.Header.Values("X-Forwarded-For"); len(vs) > 0 {
			parts := strings.Split(vs[len(vs)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	writeJSON(w, http.StatusTooManyRequests, apiError{"rate_limited", "too many requests"})
}

// Routes lists the registered "METHOD /path" patterns (after Handler has been called); the
// contract tests compare them with the OpenAPI document.
func (a *API) Routes() []string { return a.routes }

type handlerFn func(ctx context.Context, a Actor, r *http.Request) (int, any, error)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store") // personal data must not be cached by intermediaries
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (a *API) fail(w http.ResponseWriter, err error) {
	var inv *InvalidError
	switch {
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, apiError{"invalid", inv.Msg})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, apiError{"not_found", "not found"})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, apiError{"forbidden", "not permitted"})
	case errors.Is(err, ErrConflict):
		writeJSON(w, http.StatusConflict, apiError{"conflict", err.Error()})
	default:
		a.Log.Error("internal error", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError{"internal", "internal error"})
	}
}

// wrap authenticates, resolves the caller's person record and runs the handler. No route
// bypasses it except /healthz.
func (a *API) wrap(fn handlerFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.limiters()
		// Authenticate first: a request that presents a valid credential is never refused
		// because of what else the same address sent, so a flood of bad tokens cannot lock
		// valid users out. Only requests whose credentials FAIL are limited.
		p, err := a.Auth.Authenticate(r)
		if err != nil || p == nil {
			if !a.failIP.allow(clientIP(r, a.TrustProxy)) {
				tooMany(w)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="can-people"`)
			writeJSON(w, http.StatusUnauthorized, apiError{"unauthenticated", "a valid bearer token is required"})
			return
		}
		if !a.perUser.allow(p.Subject) {
			tooMany(w)
			return
		}
		actor, err := a.Svc.ResolveActor(r.Context(), p)
		if err != nil {
			a.fail(w, err)
			return
		}
		status, body, err := fn(r.Context(), actor, r)
		if err != nil {
			a.fail(w, err)
			return
		}
		writeJSON(w, status, body)
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("request body is not valid JSON for this endpoint")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("request body must be a single JSON value")
	}
	return nil
}

func qint(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	if n < 0 {
		n = 0
	}
	return n
}

// Handler returns the HTTP handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Svc.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, apiError{"unhealthy", "database unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	a.routes = []string{"GET /healthz"}
	route := func(pattern string, fn handlerFn) {
		a.routes = append(a.routes, pattern)
		mux.HandleFunc(pattern, a.wrap(fn))
	}

	// directory
	route("GET /v1/people", a.listPeople)
	route("POST /v1/import/people", a.importPeople)
	route("POST /v1/people", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in PersonInput
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		p, err := a.Svc.CreatePerson(ctx, ac, in)
		return http.StatusCreated, p, err
	})
	route("GET /v1/people/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		p, err := a.Svc.GetPerson(ctx, ac, r.PathValue("id"))
		return http.StatusOK, p, err
	})
	route("PATCH /v1/people/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in PersonPatch
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		p, err := a.Svc.UpdatePerson(ctx, ac, r.PathValue("id"), in)
		return http.StatusOK, p, err
	})
	route("GET /v1/people/{id}/sensitive", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var fields []string
		if f := r.URL.Query().Get("fields"); f != "" {
			fields = strings.Split(f, ",")
		}
		m, err := a.Svc.GetSensitive(ctx, ac, r.PathValue("id"), fields)
		return http.StatusOK, m, err
	})
	route("PUT /v1/people/{id}/sensitive", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in map[string]json.RawMessage
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.PutSensitive(ctx, ac, r.PathValue("id"), in)
	})
	route("POST /v1/people/{id}/status", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Status        Status `json:"status"`
			EffectiveDate string `json:"effective_date"`
			Note          string `json:"note"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		p, err := a.Svc.ChangeStatus(ctx, ac, r.PathValue("id"), in.Status, in.EffectiveDate, in.Note)
		return http.StatusOK, p, err
	})
	route("DELETE /v1/people/{id}/status/pending", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return http.StatusNoContent, nil, a.Svc.CancelPending(ctx, ac, r.PathValue("id"))
	})
	route("GET /v1/people/{id}/history", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		h, err := a.Svc.History(ctx, ac, r.PathValue("id"))
		return http.StatusOK, h, err
	})
	route("PUT /v1/people/{id}/manager", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			ManagerID string `json:"manager_id"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		p, err := a.Svc.SetManager(ctx, ac, r.PathValue("id"), in.ManagerID)
		return http.StatusOK, p, err
	})
	route("GET /v1/people/{id}/reports", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.Reports(ctx, ac, r.PathValue("id"))
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("PUT /v1/people/{id}/legal-hold", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Hold   bool   `json:"hold"`
			Reason string `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.SetLegalHold(ctx, ac, r.PathValue("id"), in.Hold, in.Reason)
	})
	route("POST /v1/people/{id}/erasure", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		e, err := a.Svc.RequestErasure(ctx, ac, r.PathValue("id"))
		return http.StatusOK, e, err
	})
	for _, kind := range []string{"departments", "teams", "positions"} {
		route("GET /v1/"+kind, func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
			l, err := a.Svc.ListOrgUnits(ctx, ac, kind)
			return http.StatusOK, map[string]any{"items": l}, err
		})
		route("POST /v1/"+kind, func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
			var in OrgInput
			if err := decode(r, &in); err != nil {
				return 0, nil, err
			}
			u, err := a.Svc.CreateOrgUnit(ctx, ac, kind, in)
			return http.StatusCreated, u, err
		})
	}

	// retention
	route("POST /v1/retention/run", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		if !Decide(ac, ActRetention, Fact{}) {
			return 0, nil, ErrForbidden
		}
		rep, err := a.Svc.RunRetention(ctx)
		return http.StatusOK, rep, err
	})
	route("GET /v1/erasures", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.ListErasures(ctx, ac, qint(r, "after"))
		return http.StatusOK, map[string]any{"items": l}, err
	})

	// onboarding / offboarding checklists
	route("GET /v1/checklist-templates", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.ListTemplates(ctx, ac)
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("POST /v1/checklist-templates", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in TemplateInput
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		t, err := a.Svc.CreateTemplate(ctx, ac, in)
		return http.StatusCreated, t, err
	})
	route("POST /v1/people/{id}/checklists", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			TemplateID string `json:"template_id"`
			AnchorDate string `json:"anchor_date"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		c, err := a.Svc.CreateChecklist(ctx, ac, r.PathValue("id"), in.TemplateID, in.AnchorDate)
		return http.StatusCreated, c, err
	})
	route("GET /v1/people/{id}/checklists", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.ListChecklists(ctx, ac, r.PathValue("id"))
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("GET /v1/tasks", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.MyTasks(ctx, ac)
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("POST /v1/tasks/{id}/complete", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		t, err := a.Svc.CompleteTask(ctx, ac, r.PathValue("id"))
		return http.StatusOK, t, err
	})

	// 1:1s
	route("POST /v1/oneonones", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			ReportID string `json:"report_id"`
			Cadence  string `json:"cadence"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		s, err := a.Svc.CreateSeries(ctx, ac, in.ReportID, in.Cadence)
		return http.StatusCreated, s, err
	})
	route("GET /v1/oneonones", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.ListSeries(ctx, ac)
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("GET /v1/oneonones/{id}/meetings", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.ListMeetings(ctx, ac, r.PathValue("id"))
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("POST /v1/oneonones/{id}/meetings", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			ScheduledAt time.Time `json:"scheduled_at"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		m, err := a.Svc.ScheduleMeeting(ctx, ac, r.PathValue("id"), in.ScheduledAt)
		return http.StatusCreated, m, err
	})
	route("GET /v1/meetings/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		m, err := a.Svc.GetMeeting(ctx, ac, r.PathValue("id"))
		return http.StatusOK, m, err
	})
	route("PATCH /v1/meetings/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Status string `json:"status"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.SetMeetingStatus(ctx, ac, r.PathValue("id"), in.Status)
	})
	route("POST /v1/meetings/{id}/agenda", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		it, err := a.Svc.AddAgendaItem(ctx, ac, r.PathValue("id"), in.Text)
		return http.StatusCreated, it, err
	})
	for path, private := range map[string]bool{"notes": false, "private-notes": true} {
		route("PUT /v1/meetings/{id}/"+path, func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
			var in struct {
				Text string `json:"text"`
			}
			if err := decode(r, &in); err != nil {
				return 0, nil, err
			}
			return http.StatusNoContent, nil, a.Svc.SetNotes(ctx, ac, r.PathValue("id"), in.Text, private)
		})
	}
	route("POST /v1/meetings/{id}/actions", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Text       string `json:"text"`
			AssigneeID string `json:"assignee_id"`
			DueDate    string `json:"due_date"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		it, err := a.Svc.AddAction(ctx, ac, r.PathValue("id"), in.Text, in.AssigneeID, in.DueDate)
		return http.StatusCreated, it, err
	})
	route("PATCH /v1/actions/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Status      *string `json:"status"`
			WorkIssueID *string `json:"work_issue_id"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		it, err := a.Svc.UpdateAction(ctx, ac, r.PathValue("id"), in.Status, in.WorkIssueID)
		return http.StatusOK, it, err
	})

	// goals
	route("POST /v1/goals", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in GoalInput
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		g, err := a.Svc.CreateGoal(ctx, ac, in)
		return http.StatusCreated, g, err
	})
	route("GET /v1/goals", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		q := r.URL.Query()
		l, err := a.Svc.ListGoals(ctx, ac, GoalFilter{OwnerID: q.Get("owner_id"), TeamID: q.Get("team_id"), ParentID: q.Get("parent_id"), Scope: q.Get("scope")})
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("GET /v1/goals/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		g, err := a.Svc.GetGoal(ctx, ac, r.PathValue("id"))
		return http.StatusOK, g, err
	})
	route("PATCH /v1/goals/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in GoalPatch
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		g, err := a.Svc.UpdateGoal(ctx, ac, r.PathValue("id"), in)
		return http.StatusOK, g, err
	})
	route("POST /v1/goals/{id}/key-results", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in KeyResultInput
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		k, err := a.Svc.AddKeyResult(ctx, ac, r.PathValue("id"), in)
		return http.StatusCreated, k, err
	})
	route("GET /v1/goals/{id}/updates", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		l, err := a.Svc.ListUpdates(ctx, ac, r.PathValue("id"))
		return http.StatusOK, map[string]any{"items": l}, err
	})
	route("POST /v1/key-results/{id}/progress", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Value   float64 `json:"value"`
			Comment string  `json:"comment"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		g, err := a.Svc.RecordProgress(ctx, ac, r.PathValue("id"), in.Value, in.Comment)
		return http.StatusOK, g, err
	})

	a.routesE14b(route)

	if a.Can != nil {
		mux.Handle("POST /v1/can/events", a.Can)
	}

	return withMiddleware(a.Log, mux)
}

func (a *API) listPeople(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	items, next, err := a.Svc.ListPeople(ctx, ac, PersonFilter{Status: Status(q.Get("status")), DepartmentID: q.Get("department_id"), TeamID: q.Get("team_id"), ManagerID: q.Get("manager_id"), AfterSeq: qint(r, "cursor"), Limit: limit})
	if items == nil {
		items = []Person{}
	}
	out := map[string]any{"items": items}
	if next > 0 {
		out["next_cursor"] = strconv.FormatInt(next, 10)
	}
	return http.StatusOK, out, err
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withMiddleware adds security headers, panic recovery and an access log that omits the query
// string (ids and filters can identify people) and any body.
func withMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in handler", "panic", p)
				if rec.status == http.StatusOK {
					writeJSON(rec, http.StatusInternalServerError, apiError{"internal", "internal error"})
				}
			}
			log.InfoContext(r.Context(), "request", "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}

package equity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const maxBody = 64 << 10

// API is the REST JSON surface (contract: specs/018-equity/contracts/equity-api.yaml). It only
// maps HTTP to Service calls; every authorisation decision is made in the Service through the
// policy layer.
type API struct {
	Svc  *Service
	Auth Authenticator
	Log  *slog.Logger

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

func clientHost(r *http.Request) string {
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

// result is what a handler returns: a JSON body, or a raw body (CSV) with a content type.
type result struct {
	status int
	body   any
	raw    func(w io.Writer) error
	ctype  string
}

type handlerFn func(ctx context.Context, ac Actor, r *http.Request) (result, error)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store") // equity data must not be cached by intermediaries
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
	case errors.Is(err, ErrIdentityRequired):
		writeJSON(w, http.StatusForbidden, apiError{"identity_required", ErrIdentityRequired.Error()})
	case errors.Is(err, ErrIdentityUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, apiError{"directory_unavailable", ErrIdentityUnavailable.Error()})
	case errors.Is(err, ErrSeparation):
		writeJSON(w, http.StatusForbidden, apiError{"separation_of_duties", ErrSeparation.Error()})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, apiError{"forbidden", "not permitted"})
	case errors.Is(err, ErrConflict):
		writeJSON(w, http.StatusConflict, apiError{"conflict", err.Error()})
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusServiceUnavailable, apiError{"timeout", "the request took too long"})
	default:
		a.Log.Error("internal error", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError{"internal", "internal error"})
	}
}

// wrap authenticates, resolves the caller and runs the handler. No route bypasses it except
// /healthz.
func (a *API) wrap(fn handlerFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.limiters()
		setSecurityHeaders(w)
		// Authenticate first: a request that presents a valid credential is never refused
		// because of what else the same address sent, so a flood of bad tokens cannot lock
		// valid users out. Only requests whose credentials FAIL are limited.
		p, err := a.Auth.Authenticate(r)
		if err != nil || p == nil {
			if !a.failIP.allow(clientHost(r)) {
				tooMany(w)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="can-equity"`)
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
		res, err := fn(r.Context(), actor, r)
		if err != nil {
			a.fail(w, err)
			return
		}
		if res.raw != nil {
			w.Header().Set("Content-Type", res.ctype)
			w.WriteHeader(res.status)
			_ = res.raw(w)
			return
		}
		writeJSON(w, res.status, res.body)
	}
}

// decode reads one strict JSON value (unknown fields are errors) and returns the raw bytes, which
// idempotency hashes.
func decode(r *http.Request, v any) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, invalid("request body is too large or unreadable")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, invalid("request body is not valid JSON for this endpoint")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("request body must be a single JSON value")
	}
	return b, nil
}

func qint(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	if n < 0 {
		n = 0
	}
	return n
}

func ok(v any) (result, error)      { return result{status: http.StatusOK, body: v}, nil }
func created(v any) (result, error) { return result{status: http.StatusCreated, body: v}, nil }
func noContent() (result, error)    { return result{status: http.StatusNoContent}, nil }
func items(v any) (result, error)   { return ok(map[string]any{"items": v}) }

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
	svc := func() *Service { return a.Svc }
	idemKey := func(r *http.Request) string { return r.Header.Get("Idempotency-Key") }
	// createdAt answers a create (or its replay): 201 first time, 200 on a replay, body = the
	// record re-read under the caller's current rights.
	createdAt := func(id string, replay bool, load func() (any, error)) (result, error) {
		v, err := load()
		if err != nil {
			return result{}, err
		}
		if replay {
			return ok(v)
		}
		return created(v)
	}

	// structure
	route("POST /v1/entities", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in EntityInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		e, err := svc().CreateEntity(ctx, ac, in)
		if err != nil {
			return result{}, err
		}
		return created(e)
	})
	route("GET /v1/entities", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListEntities(ctx, ac)
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/entities/{id}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		e, err := svc().GetEntity(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(e)
	})
	route("POST /v1/entities/{id}/share-classes", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in ShareClassInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		c, err := svc().CreateShareClass(ctx, ac, r.PathValue("id"), in)
		if err != nil {
			return result{}, err
		}
		return created(c)
	})
	route("GET /v1/entities/{id}/share-classes", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListShareClasses(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("PATCH /v1/share-classes/{id}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Authorised Qty `json:"authorised"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		c, err := svc().RaiseAuthorised(ctx, ac, r.PathValue("id"), in.Authorised)
		if err != nil {
			return result{}, err
		}
		return ok(c)
	})
	route("POST /v1/entities/{id}/plans", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in PlanInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		p, err := svc().CreatePlan(ctx, ac, r.PathValue("id"), in)
		if err != nil {
			return result{}, err
		}
		return created(p)
	})
	route("GET /v1/entities/{id}/plans", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListPlans(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/plans/{id}/pool", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		p, err := svc().Pool(ctx, ac, r.PathValue("id"), r.URL.Query().Get("as_of"))
		if err != nil {
			return result{}, err
		}
		return ok(p)
	})

	// stakeholders
	route("POST /v1/entities/{id}/stakeholders", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in StakeholderInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		st, err := svc().CreateStakeholder(ctx, ac, r.PathValue("id"), in)
		if err != nil {
			return result{}, err
		}
		return created(st)
	})
	route("GET /v1/entities/{id}/stakeholders", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListStakeholders(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/stakeholders/{id}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		st, err := svc().GetStakeholder(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(st)
	})
	route("GET /v1/stakeholders/{id}/identity", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		i, err := svc().GetIdentity(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(i)
	})
	route("PUT /v1/stakeholders/{id}/identity", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in Identity
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		if err := svc().SetIdentity(ctx, ac, r.PathValue("id"), in); err != nil {
			return result{}, err
		}
		return noContent()
	})
	route("PUT /v1/stakeholders/{id}/person", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in struct {
			PersonID string `json:"person_id"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		st, err := svc().LinkPerson(ctx, ac, r.PathValue("id"), in.PersonID)
		if err != nil {
			return result{}, err
		}
		return ok(st)
	})
	route("POST /v1/stakeholders/{id}/verify-person", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		st, err := svc().VerifyPerson(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(st)
	})
	route("GET /v1/identity/review", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		list, err := svc().IdentityReview(ctx, ac)
		if err != nil {
			return result{}, err
		}
		return items(list)
	})
	route("GET /v1/me/identity", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		return ok(svc().WhoAmI(ac))
	})
	route("POST /v1/stakeholders/{id}/attest-external", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		st, err := svc().AttestExternal(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(st)
	})
	route("PUT /v1/stakeholders/{id}/legal-hold", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Hold   bool   `json:"hold"`
			Reason string `json:"reason"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		if err := svc().SetLegalHold(ctx, ac, r.PathValue("id"), in.Hold, in.Reason); err != nil {
			return result{}, err
		}
		return noContent()
	})
	route("POST /v1/stakeholders/{id}/erasure", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		e, err := svc().RequestErasure(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(e)
	})

	// grants
	route("POST /v1/plans/{id}/grants", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in GrantInput
		body, err := decode(r, &in)
		if err != nil {
			return result{}, err
		}
		id, replay, err := svc().CreateGrant(ctx, ac, r.PathValue("id"), in, idemKey(r), body)
		if err != nil {
			return result{}, err
		}
		return createdAt(id, replay, func() (any, error) { return svc().GetGrant(ctx, ac, id) })
	})
	route("GET /v1/entities/{id}/grants", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		p, err := svc().ListGrants(ctx, ac, r.PathValue("id"), GrantFilter{Status: r.URL.Query().Get("status"), StakeholderID: r.URL.Query().Get("stakeholder_id"),
			After: qint(r, "cursor"), Limit: int(qint(r, "limit"))})
		if err != nil {
			return result{}, err
		}
		return ok(p)
	})
	route("GET /v1/grants/{id}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		g, err := svc().GetGrant(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(g)
	})
	route("PATCH /v1/grants/{id}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in GrantPatch
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		g, err := svc().UpdateGrant(ctx, ac, r.PathValue("id"), in)
		if err != nil {
			return result{}, err
		}
		return ok(g)
	})
	route("DELETE /v1/grants/{id}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		if err := svc().DeleteGrant(ctx, ac, r.PathValue("id")); err != nil {
			return result{}, err
		}
		return noContent()
	})
	grantAction := func(name string, fn func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error)) {
		route("POST /v1/grants/{id}/"+name, func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
			v, err := fn(ctx, ac, r.PathValue("id"), r)
			if err != nil {
				return result{}, err
			}
			return ok(v)
		})
	}
	grantAction("submit", func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error) {
		return svc().SubmitGrant(ctx, ac, id)
	})
	grantAction("approve", func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error) {
		return svc().ApproveGrant(ctx, ac, id)
	})
	grantAction("amend", func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error) {
		return svc().AmendGrant(ctx, ac, id)
	})
	grantAction("reject", func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if _, err := decode(r, &in); err != nil {
			return nil, err
		}
		return svc().RejectGrant(ctx, ac, id, in.Reason)
	})
	grantAction("activate", func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error) {
		var in struct {
			On string `json:"activated_on"`
		}
		if _, err := decode(r, &in); err != nil {
			return nil, err
		}
		return svc().ActivateGrant(ctx, ac, id, in.On)
	})
	grantAction("cancel", func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if _, err := decode(r, &in); err != nil {
			return nil, err
		}
		return svc().CancelGrant(ctx, ac, id, in.Reason)
	})
	grantAction("terminate", func(ctx context.Context, ac Actor, id string, r *http.Request) (any, error) {
		var in struct {
			On       string `json:"termination_date"`
			ForCause bool   `json:"for_cause"`
		}
		if _, err := decode(r, &in); err != nil {
			return nil, err
		}
		return svc().TerminateGrant(ctx, ac, id, in.On, in.ForCause)
	})
	route("POST /v1/grants/{id}/milestones", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Tranche    int    `json:"tranche_index"`
			AchievedOn string `json:"achieved_on"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		if err := svc().RecordMilestone(ctx, ac, r.PathValue("id"), in.Tranche, in.AchievedOn); err != nil {
			return result{}, err
		}
		return noContent()
	})
	route("POST /v1/grants/{id}/acceleration", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Trigger    string `json:"trigger"`
			OccurredOn string `json:"occurred_on"`
		}
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		if err := svc().RecordAcceleration(ctx, ac, r.PathValue("id"), in.Trigger, in.OccurredOn); err != nil {
			return result{}, err
		}
		return noContent()
	})
	route("GET /v1/grants/{id}/vesting", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		v, err := svc().Vesting(ctx, ac, r.PathValue("id"), r.URL.Query().Get("as_of"))
		if err != nil {
			return result{}, err
		}
		return ok(v)
	})

	// exercises
	route("POST /v1/grants/{id}/exercises", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in struct {
			Quantity Qty `json:"quantity"`
		}
		body, err := decode(r, &in)
		if err != nil {
			return result{}, err
		}
		id, replay, err := svc().RequestExercise(ctx, ac, r.PathValue("id"), in.Quantity, idemKey(r), body)
		if err != nil {
			return result{}, err
		}
		return createdAt(id, replay, func() (any, error) { return svc().GetExercise(ctx, ac, id) })
	})
	route("GET /v1/exercises/{id}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		e, err := svc().GetExercise(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return ok(e)
	})
	route("GET /v1/entities/{id}/exercises", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		p, err := svc().ListExercises(ctx, ac, r.PathValue("id"), ExerciseFilter{Status: r.URL.Query().Get("status"), After: qint(r, "cursor")})
		if err != nil {
			return result{}, err
		}
		return ok(p)
	})
	exAction := func(name string, fn func(ctx context.Context, ac Actor, id string, r *http.Request) (*Exercise, error)) {
		route("POST /v1/exercises/{id}/"+name, func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
			e, err := fn(ctx, ac, r.PathValue("id"), r)
			if err != nil {
				return result{}, err
			}
			return ok(e)
		})
	}
	exAction("approve", func(ctx context.Context, ac Actor, id string, r *http.Request) (*Exercise, error) {
		return svc().ApproveExercise(ctx, ac, id)
	})
	exAction("reject", func(ctx context.Context, ac Actor, id string, r *http.Request) (*Exercise, error) {
		return svc().RejectExercise(ctx, ac, id)
	})
	exAction("cancel", func(ctx context.Context, ac Actor, id string, r *http.Request) (*Exercise, error) {
		return svc().CancelExercise(ctx, ac, id)
	})
	exAction("issue", func(ctx context.Context, ac Actor, id string, r *http.Request) (*Exercise, error) {
		return svc().IssueShares(ctx, ac, id)
	})
	exAction("payment", func(ctx context.Context, ac Actor, id string, r *http.Request) (*Exercise, error) {
		var in PaymentInput
		if _, err := decode(r, &in); err != nil {
			return nil, err
		}
		return svc().RecordPayment(ctx, ac, id, in)
	})

	// ledger, cap table, modelling
	ledgerRoute := func(name string, fn func(ctx context.Context, ac Actor, entity string, in LedgerInput, key string, body []byte) (string, bool, error)) {
		route("POST /v1/entities/{id}/ledger/"+name, func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
			var in LedgerInput
			body, err := decode(r, &in)
			if err != nil {
				return result{}, err
			}
			id, replay, err := fn(ctx, ac, r.PathValue("id"), in, idemKey(r), body)
			if err != nil {
				return result{}, err
			}
			return createdAt(id, replay, func() (any, error) { return svc().GetLedgerEntry(ctx, ac, id) })
		})
	}
	ledgerRoute("issuances", func(ctx context.Context, ac Actor, e string, in LedgerInput, k string, b []byte) (string, bool, error) {
		return svc().IssueNewShares(ctx, ac, e, in, k, b)
	})
	ledgerRoute("transfers", func(ctx context.Context, ac Actor, e string, in LedgerInput, k string, b []byte) (string, bool, error) {
		return svc().TransferShares(ctx, ac, e, in, k, b)
	})
	ledgerRoute("cancellations", func(ctx context.Context, ac Actor, e string, in LedgerInput, k string, b []byte) (string, bool, error) {
		return svc().CancelShares(ctx, ac, e, in, k, b)
	})
	route("GET /v1/entities/{id}/ledger", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		p, err := svc().ListLedger(ctx, ac, r.PathValue("id"), r.URL.Query().Get("as_of"), qint(r, "cursor"), int(qint(r, "limit")))
		if err != nil {
			return result{}, err
		}
		return ok(p)
	})
	route("GET /v1/entities/{id}/captable", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		q := r.URL.Query()
		ct, err := svc().GetCapTable(ctx, ac, r.PathValue("id"), q.Get("as_of"), q.Get("view"), q.Get("names") == "true")
		if err != nil {
			return result{}, err
		}
		return ok(ct)
	})
	route("POST /v1/entities/{id}/whatif", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in WhatIfRequest
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		res, err := svc().WhatIf(ctx, ac, r.PathValue("id"), in)
		if err != nil {
			return result{}, err
		}
		return ok(res)
	})
	route("POST /v1/entities/{id}/convertibles", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in ConvertibleInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		c, err := svc().CreateConvertible(ctx, ac, r.PathValue("id"), in)
		if err != nil {
			return result{}, err
		}
		return created(c)
	})
	route("GET /v1/entities/{id}/convertibles", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListConvertibles(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("POST /v1/entities/{id}/valuations", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var in ValuationInput
		if _, err := decode(r, &in); err != nil {
			return result{}, err
		}
		v, err := svc().CreateValuation(ctx, ac, r.PathValue("id"), in)
		if err != nil {
			return result{}, err
		}
		return created(v)
	})
	route("GET /v1/entities/{id}/valuations", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListValuations(ctx, ac, r.PathValue("id"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	route("GET /v1/entities/{id}/exports/{name}", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		var buf bytes.Buffer
		if err := svc().ExportCSV(ctx, ac, r.PathValue("id"), r.PathValue("name"), r.URL.Query().Get("as_of"), &buf); err != nil {
			return result{}, err
		}
		return result{status: http.StatusOK, ctype: "text/csv; charset=utf-8", raw: func(w io.Writer) error { _, err := w.Write(buf.Bytes()); return err }}, nil
	})

	// self service
	route("GET /v1/me/grants", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		p, err := svc().MyGrants(ctx, ac, GrantFilter{After: qint(r, "cursor"), Limit: int(qint(r, "limit"))})
		if err != nil {
			return result{}, err
		}
		return ok(p)
	})
	route("GET /v1/me/exercises", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		p, err := svc().MyExercises(ctx, ac, qint(r, "cursor"))
		if err != nil {
			return result{}, err
		}
		return ok(p)
	})

	// retention
	route("POST /v1/retention/run", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		if !Decide(ac, ActRetention, Fact{}) {
			return result{}, ErrForbidden
		}
		rep, err := svc().RunRetention(ctx)
		if err != nil {
			return result{}, err
		}
		return ok(rep)
	})
	route("GET /v1/erasures", func(ctx context.Context, ac Actor, r *http.Request) (result, error) {
		l, err := svc().ListErasures(ctx, ac, qint(r, "after"))
		if err != nil {
			return result{}, err
		}
		return items(l)
	})
	return withDeadline(mux)
}

// withDeadline bounds every request, so a slow query or a stuck client cannot hold the single
// database connection indefinitely.
func withDeadline(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

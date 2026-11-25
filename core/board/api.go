package board

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
)

const maxBody = 64 << 10

// API is the REST JSON surface (contract: specs/029-board-entity/contracts/board-api.yaml). It only
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
	h.Set("Cache-Control", "no-store") // board data must not be cached by intermediaries
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
			w.Header().Set("WWW-Authenticate", `Bearer realm="can-board"`)
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

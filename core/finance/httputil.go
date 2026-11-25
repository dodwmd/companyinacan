package finance

import (
	"context"
	"encoding/base64"
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

	"github.com/dodwmd/companyinacan/core/obs"
)

// setSecurityHeaders applies the headers every response carries. The API serves JSON only,
// so the CSP forbids everything.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Errors  []ProblemField `json:"errors,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Code: code, Message: msg})
}

// writeStoreError maps a store error to a response. Unexpected errors are logged and
// reported without detail.
func writeStoreError(w http.ResponseWriter, log *slog.Logger, err error) {
	var ie *InvalidError
	switch {
	case errors.As(err, &ie):
		writeJSON(w, http.StatusUnprocessableEntity, apiError{Code: "invalid", Message: "the request has invalid fields", Errors: ie.Problems})
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrVersionMismatch):
		writeError(w, http.StatusPreconditionFailed, "version_mismatch", "the record changed since you read it; fetch it again and retry")
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ErrIdempotencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_mismatch", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "timeout", "the request took too long and was stopped; narrow it (add a date range or filters) and try again")
	case errors.Is(err, ErrApprovalsUnavailable):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "approvals_unavailable", "the approvals engine did not answer; try again shortly")
	case errors.Is(err, ErrSeparation):
		writeError(w, http.StatusForbidden, "separation_of_duties", "you cannot approve a record you created, edited or own")
	case errors.Is(err, ErrNotConfigured):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "not_configured", err.Error())
	case errors.Is(err, ErrNotImplemented):
		writeError(w, http.StatusNotImplemented, "not_implemented", err.Error())
	case errors.Is(err, ErrLocked):
		writeError(w, http.StatusConflict, "period_locked", err.Error())
	case errors.Is(err, ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "this token cannot do that")
	default:
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
			return
		}
		log.Error("request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// decodeJSON reads exactly one JSON value into v. Unknown fields are an error: that is the
// mass-assignment guard (a client cannot set reporter, status, version, key and so on by
// naming them), and it catches typos in automation early.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+oneLine(err.Error()))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: trailing data")
		return false
	}
	return true
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ---- cursors --------------------------------------------------------------------------

func encodeCursor(n int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("c" + strconv.FormatInt(n, 10)))
}

func decodeCursor(s string) (int64, bool) {
	if s == "" {
		return 0, true
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < 2 || b[0] != 'c' {
		return 0, false
	}
	n, err := strconv.ParseInt(string(b[1:]), 10, 64)
	return n, err == nil && n >= 0
}

// ---- ETag -----------------------------------------------------------------------------

func setETag(w http.ResponseWriter, version int64) {
	w.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}

// parseIfMatch reads an If-Match header holding one record version. ok is false when the
// header is malformed; a missing header (or "*") yields a nil version.
func parseIfMatch(r *http.Request) (v *int64, ok bool) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" || h == "*" {
		return nil, true
	}
	h = strings.TrimPrefix(h, "W/")
	h = strings.Trim(h, `"`)
	n, err := strconv.ParseInt(h, 10, 64)
	if err != nil || n < 1 {
		return nil, false
	}
	return &n, true
}

// ---- rate limiting --------------------------------------------------------------------

// rateLimiter is a fixed-window counter per key with a bounded key set.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	max     int
	windows map[string]*rateWindow
	now     func() time.Time
}

type rateWindow struct {
	start time.Time
	n     int
}

func newRateLimiter(limit int, window time.Duration, maxKeys int, now func() time.Time) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, max: maxKeys, windows: map[string]*rateWindow{}, now: now}
}

// allow counts one request for key. It returns false and the wait when the key is over limit.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	if l.limit <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.windows[key]
	if w == nil || now.Sub(w.start) >= l.window {
		if w == nil && len(l.windows) >= l.max {
			for k, v := range l.windows {
				if now.Sub(v.start) >= l.window {
					delete(l.windows, k)
				}
			}
			if len(l.windows) >= l.max { // still full: shed the oldest half rather than grow
				i := 0
				for k := range l.windows {
					if i%2 == 0 {
						delete(l.windows, k)
					}
					i++
				}
			}
		}
		w = &rateWindow{start: now}
		l.windows[key] = w
	}
	w.n++
	if w.n > l.limit {
		return false, l.window - now.Sub(w.start)
	}
	return true, 0
}

// clientIP returns the address used for rate limiting. Forwarding headers are honoured
// only when the operator says a trusted proxy sets them, and then the right-most entry
// (appended by that proxy) is used, never a client-supplied left-most one.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
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

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withMiddleware adds panic recovery and structured access logging. The access log omits
// the query string, client address and request bodies on purpose.
func withMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return obs.Instrument(accessLog(log, next))
}

// accessLog recovers panics and writes one structured line per request: method, matched route
// pattern, status and duration. Never the query string, the raw path or any body.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in handler", "panic", p)
				if rec.status == http.StatusOK {
					setSecurityHeaders(rec)
					writeError(rec, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			log.InfoContext(r.Context(), "request", "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}

// logPath keeps bearer-like path segments out of the access log: the signed invoice link is a
// credential, and anyone who can read the log would otherwise hold a working link.
func logPath(p string) string {
	// Anywhere in the path: "//public/invoices/<token>" and "/x/public/invoices/<token>" are not
	// served as links but must not put the segment in the log either.
	if strings.Contains(p, "/public/invoices/") {
		return "/public/invoices/{token}"
	}
	return p
}

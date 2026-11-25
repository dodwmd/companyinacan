// Package httpx holds the small HTTP helpers shared by the admin and module APIs: JSON and
// problem+json responses, request ids, structured access logging, panic recovery and body limits.
package httpx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// MaxBodyBytes bounds every request body.
const MaxBodyBytes = 1 << 20

// MaxLoginBodyBytes bounds the unauthenticated login body.
const MaxLoginBodyBytes = 4 << 10

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ProblemDoc is an RFC 9457 problem document.
type ProblemDoc struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Problem writes an application/problem+json error.
func Problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ProblemDoc{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail})
}

// DecodeJSON reads a JSON body into v, rejecting unknown fields and trailing data. The Content-Type
// must be application/json (this also blocks cross-site form posts).
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return DecodeJSONMax(w, r, v, MaxBodyBytes)
}

// DecodeJSONMax is DecodeJSON with a custom body limit (small for unauthenticated endpoints).
func DecodeJSONMax(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if strings.TrimSpace(strings.ToLower(ct)) != "application/json" {
		Problem(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			Problem(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			Problem(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		Problem(w, http.StatusBadRequest, "unexpected data after the JSON document")
		return false
	}
	return true
}

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID returns the id attached by the Middleware.
func RequestID(ctx context.Context) string { s, _ := ctx.Value(requestIDKey).(string); return s }

// ClientIP returns the caller's address. With trustProxy it uses the last X-Forwarded-For entry,
// which is the one appended by the trusted ingress (Traefik); never the first, which the client controls.
func ClientIP(r *http.Request, trustProxy bool) string {
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

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Middleware wraps h with request ids, panic recovery, security headers and structured logging.
// Authorization and Cookie headers are never logged.
func Middleware(log *slog.Logger, api string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var b [8]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-Id", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		rec := &recorder{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in handler", "api", api, "request_id", id, "panic", p, "stack", string(bytes.TrimSpace(debug.Stack())))
				if rec.status == 0 {
					Problem(rec, http.StatusInternalServerError, "internal error")
				}
			}
			pattern := r.Pattern
			if pattern == "" {
				pattern = "unmatched"
			}
			log.Info("request", "api", api, "request_id", id, "method", r.Method, "route", pattern, "status", rec.status,
				"bytes", rec.bytes, "duration_ms", time.Since(start).Milliseconds())
		}()
		h.ServeHTTP(rec, r)
	})
}

// BearerToken extracts the token from an Authorization: Bearer header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Health is the shared /healthz handler.
func Health(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

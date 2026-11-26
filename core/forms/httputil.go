package forms

import (
	"encoding/json"
	"html"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
)

// setSecurityHeaders applies the headers every response carries. The gateway serves JSON
// and tiny HTML pages only, so the CSP forbids everything.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// outboundClient is the default client for calls to configured URLs: bounded time and no
// redirect following (a redirect could point a call carrying a secret at another host).
func outboundClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Errors  []FieldError `json:"errors,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Code: code, Message: msg})
}

// writeHTMLPage renders a minimal static page; every dynamic string is escaped.
func writeHTMLPage(w http.ResponseWriter, status int, title string, lines ...string) {
	var b strings.Builder
	b.WriteString("<!doctype html><meta charset=utf-8><meta name=viewport content=\"width=device-width,initial-scale=1\"><title>")
	b.WriteString(html.EscapeString(title))
	b.WriteString("</title><h1>")
	b.WriteString(html.EscapeString(title))
	b.WriteString("</h1>")
	for _, l := range lines {
		b.WriteString("<p>")
		b.WriteString(html.EscapeString(l))
		b.WriteString("</p>")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(b.String()))
}

// clientIP returns the address used for rate limiting. Forwarding headers are honoured
// only when the operator says a trusted proxy sets them, and then the right-most entry
// (appended by that proxy) is used, never a client-supplied left-most one.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// Header lines, not Get: Get returns the first line, which the client may have sent.
		if vs := r.Header.Values("X-Forwarded-For"); len(vs) > 0 {
			xff := vs[len(vs)-1]
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
// the query string, client address and any submitted data on purpose.
func withMiddleware(log *slog.Logger, name string, next http.Handler) http.Handler {
	return obs.Instrument(accessLog(log, name, next))
}

// accessLog recovers panics and writes one structured line per request: method, matched route
// pattern, status and duration. Never the query string, the raw path or any body.
func accessLog(log *slog.Logger, name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in handler", "server", name, "panic", p)
				if rec.status == http.StatusOK {
					setSecurityHeaders(rec)
					writeError(rec, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			log.InfoContext(r.Context(), "request", "server", name, "method", r.Method, "route", r.Pattern, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}

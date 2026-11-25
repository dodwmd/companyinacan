package obs

import (
	"net/http"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/obs/rid"
)

// Middleware records request count, duration and in-flight gauge, propagates a safe request id
// and serves /readyz. Labels are the method (a fixed set), the route pattern the mux matched
// ("/v1/leads/{id}", never the raw path) and the status class, so cardinality is bounded by the
// routes the code registers and no identifier from a URL can reach a label.
type Middleware struct {
	reqs     *CounterVec
	dur      *HistogramVec
	inflight *Gauge
	fixed    string
	health   *Health
}

// MiddlewareOption customises NewMiddleware.
type MiddlewareOption func(*Middleware)

// FixedRoute labels every request with route (for servers with no mux, like can-static).
func FixedRoute(route string) MiddlewareOption { return func(m *Middleware) { m.fixed = route } }

// WithHealth serves h at /readyz instead of DefaultHealth.
func WithHealth(h *Health) MiddlewareOption { return func(m *Middleware) { m.health = h } }

// NewMiddleware registers the HTTP metrics on reg.
func NewMiddleware(reg *Registry, opts ...MiddlewareOption) *Middleware {
	m := &Middleware{
		reqs:     reg.Counter("can_http_requests_total", "HTTP requests by method, route pattern and status class.", "method", "route", "class"),
		dur:      reg.Histogram("can_http_request_duration_seconds", "HTTP request duration by method and route pattern.", DefBuckets, "method", "route"),
		inflight: reg.Gauge("can_http_requests_in_flight", "Requests currently being served.").With(),
		health:   DefaultHealth,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Instrument wraps h with a Middleware on the Default registry.
func Instrument(h http.Handler, opts ...MiddlewareOption) http.Handler {
	return NewMiddleware(Default, opts...).Wrap(h)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Wrap returns the instrumented handler.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An id set by an outer layer wins, so one request has one id.
		if id := rid.From(r.Context()); id == "" {
			id = rid.New(r.Header.Get("X-Request-Id"))
			w.Header().Set("X-Request-Id", id)
			r = r.WithContext(rid.With(r.Context(), id))
		}
		if r.URL.Path == "/readyz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			m.health.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		m.inflight.Inc()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			m.inflight.Dec()
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			method, route := methodLabel(r.Method), m.fixed
			if route == "" {
				route = routeLabel(r.Pattern)
			}
			m.reqs.With(method, route, statusClass(status)).Inc()
			m.dur.With(method, route).Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(sw, r)
	})
}

func methodLabel(m string) string {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return m
	}
	return "OTHER"
}

// routeLabel turns a mux pattern ("GET /v1/leads/{id}") into the path part. The mux sets
// Request.Pattern only for registered patterns, so the value set is bounded by the code.
func routeLabel(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	return pattern
}

func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	}
	return "1xx"
}

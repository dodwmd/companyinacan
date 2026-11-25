package obs

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Component and overall states reported by /readyz.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded" // a non-critical dependency is failing; the service still serves
	StatusDown     = "down"     // a critical component is failing; readiness is refused (503)
)

// Component is one line of the readiness report. It carries no error text on purpose: /readyz is
// reachable by anything that can reach the pod, so it names components and states only.
type Component struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Report is the /readyz body.
type Report struct {
	Status     string      `json:"status"`
	Components []Component `json:"components"`
}

type healthCheck struct {
	name     string
	critical bool
	fn       func(context.Context) error
}

// Health is the readiness model. /healthz stays a plain liveness answer ("the process is up and
// serving"); readiness additionally asks whether the service can do useful work.
//
// A failing critical check (database, migrations) makes the service not ready (503). A failing
// non-critical check (can-core introspection, the notification webhook) only marks the service
// degraded: it stays in rotation, because restarting or removing it would not fix the dependency.
type Health struct {
	mu       sync.Mutex
	checks   []healthCheck
	ttl      time.Duration
	timeout  time.Duration
	last     Report
	lastAt   time.Time
	inflight *flight
}

// NewHealth returns an empty model: results are cached for 2 s and each check gets 2 s.
func NewHealth() *Health { return &Health{ttl: 2 * time.Second, timeout: 2 * time.Second} }

// DefaultHealth is served by Middleware at /readyz.
var DefaultHealth = NewHealth()

// Add registers a check, replacing one with the same name.
func (h *Health) Add(name string, critical bool, fn func(context.Context) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.checks {
		if h.checks[i].name == name {
			h.checks[i] = healthCheck{name, critical, fn}
			h.lastAt = time.Time{}
			return
		}
	}
	h.checks = append(h.checks, healthCheck{name, critical, fn})
	h.lastAt = time.Time{}
}

// SetTTL sets how long a result is reused (0 disables caching). Caching bounds the cost of a
// readiness probe aimed at the database.
func (h *Health) SetTTL(d time.Duration) { h.mu.Lock(); h.ttl = d; h.mu.Unlock() }

// SetTimeout sets the per-check deadline.
func (h *Health) SetTimeout(d time.Duration) { h.mu.Lock(); h.timeout = d; h.mu.Unlock() }

// flight is one run of all checks that concurrent callers share.
type flight struct {
	done chan struct{}
	rep  Report
}

// Check returns the cached result, joins a run already in progress, or starts one.
//
// The run uses a context detached from the caller (context.WithoutCancel): the first prober
// disconnecting must not cancel the database check and cache a false "down" for everyone else. The
// caller itself still stops waiting when its own context ends, and then gets the last known result.
// No lock is held while checks run, so concurrent probes never queue behind each other.
func (h *Health) Check(ctx context.Context) Report {
	h.mu.Lock()
	if h.ttl > 0 && !h.lastAt.IsZero() && time.Since(h.lastAt) < h.ttl {
		rep := h.last
		h.mu.Unlock()
		return rep
	}
	f := h.inflight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		h.inflight = f
		checks := append([]healthCheck(nil), h.checks...)
		timeout := h.timeout
		go h.runAll(context.WithoutCancel(ctx), checks, timeout, f)
	}
	h.mu.Unlock()
	select {
	case <-f.done:
		return f.rep
	case <-ctx.Done():
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.last
	}
}

func (h *Health) runAll(ctx context.Context, checks []healthCheck, timeout time.Duration, f *flight) {
	rep := Report{Status: StatusOK, Components: make([]Component, 0, len(checks))}
	for _, c := range checks {
		st := StatusOK
		if err := runCheck(ctx, c, timeout); err != nil {
			if c.critical {
				st = StatusDown
				rep.Status = StatusDown
			} else {
				st = StatusDegraded
				if rep.Status == StatusOK {
					rep.Status = StatusDegraded
				}
			}
		}
		rep.Components = append(rep.Components, Component{c.name, st})
	}
	h.mu.Lock()
	h.last, h.lastAt, h.inflight = rep, time.Now(), nil
	h.mu.Unlock()
	f.rep = rep
	close(f.done)
}

func runCheck(ctx context.Context, c healthCheck, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- context.Canceled
			}
		}()
		done <- c.fn(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ServeHTTP answers GET /readyz: 200 when ok or degraded, 503 when down.
func (h *Health) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rep := h.Check(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if rep.Status == StatusDown {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(rep.appendJSON(nil))
}

// appendJSON encodes the report by hand: encoding/json would add its reflection machinery to every
// binary for a fixed two-level document. Names are program constants, quoted defensively anyway.
func (r Report) appendJSON(b []byte) []byte {
	b = append(b, `{"status":`...)
	b = strconv.AppendQuote(b, r.Status)
	b = append(b, `,"components":[`...)
	for i, c := range r.Components {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = strconv.AppendQuote(b, c.Name)
		b = append(b, `,"status":`...)
		b = strconv.AppendQuote(b, c.Status)
		b = append(b, '}')
	}
	return append(b, "]}\n"...)
}

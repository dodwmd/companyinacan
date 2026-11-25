package obs

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ContentType is the Prometheus text exposition media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// MetricsHandler serves reg at /metrics. With a non-empty token every request must present it as
// "Authorization: Bearer <token>"; the comparison is constant time. Failed attempts are answered
// 401 and are neither logged with the presented value nor counted against anyone, so a valid
// token is never throttled by someone else's bad guesses.
func MetricsHandler(reg *Registry, token string) http.Handler {
	want := sha256.Sum256([]byte(token))
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token != "" {
			got := sha256.Sum256([]byte(bearer(r)))
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", ContentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodGet {
			_, _ = reg.WriteTo(w)
		}
	})
	return mux
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Logger is the part of *slog.Logger the metrics listener uses; obs does not import log/slog so
// that tiny binaries stay tiny. Nil disables logging.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// MetricsServer is a running metrics listener.
type MetricsServer struct {
	srv *http.Server
	ln  net.Listener
}

// Addr is the bound address.
func (m *MetricsServer) Addr() net.Addr { return m.ln.Addr() }

// StartMetrics starts the metrics listener described by the environment and returns nil, nil when
// it is disabled (the default):
//
//	CAN_METRICS_ADDR        host:port of a dedicated listener, never the public or ingress one
//	CAN_METRICS_TOKEN       bearer token (or CAN_METRICS_TOKEN_FILE: a mounted Secret file)
//
// A listener that is not loopback-only is refused without a token, so a typo cannot publish
// metrics unauthenticated. The listener serves /metrics and nothing else, and stops with ctx.
func StartMetrics(ctx context.Context, getenv func(string) string, reg *Registry, log Logger) (*MetricsServer, error) {
	addr := getenv("CAN_METRICS_ADDR")
	if addr == "" {
		return nil, nil
	}
	token := strings.TrimSpace(getenv("CAN_METRICS_TOKEN"))
	if f := getenv("CAN_METRICS_TOKEN_FILE"); token == "" && f != "" {
		b, err := os.ReadFile(f) //nolint:gosec // operator-chosen mounted Secret path
		if err != nil {
			return nil, fmt.Errorf("CAN_METRICS_TOKEN_FILE: %w", err)
		}
		token = strings.TrimSpace(string(b))
		if fi, err := os.Stat(f); err == nil && fi.Mode().Perm()&0o077 != 0 && log != nil {
			log.Warn("CAN_METRICS_TOKEN_FILE is readable by group or others; restrict it (chmod 0400, or defaultMode: 0400 on the Secret volume)", "mode", fi.Mode().Perm().String())
		}
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("CAN_METRICS_ADDR: %w", err)
	}
	if token == "" && !loopbackHost(host) {
		return nil, errors.New("CAN_METRICS_ADDR is not loopback-only: set CAN_METRICS_TOKEN (or CAN_METRICS_TOKEN_FILE); refusing to serve metrics unauthenticated")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("CAN_METRICS_ADDR: %w", err)
	}
	srv := &http.Server{Handler: MetricsHandler(reg, token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && log != nil {
			log.Error("metrics listener stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if log != nil {
		log.Info("metrics listening", "addr", ln.Addr().String(), "auth", token != "")
	}
	return &MetricsServer{srv: srv, ln: ln}, nil
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Setup is the one call a service's main makes: it registers the process and footprint metrics on
// the Default registry and starts the metrics listener when CAN_METRICS_ADDR is set.
func Setup(ctx context.Context, getenv func(string) string, bi BuildInfo, log Logger) error {
	RegisterProcess(Default, bi, getenv)
	_, err := StartMetrics(ctx, getenv, Default, log)
	return err
}

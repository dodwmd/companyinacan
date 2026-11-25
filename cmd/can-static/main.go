// Command can-static is the tiny static file server for the in-cluster site pod.
//
// It serves a directory read-only (it never writes to disk, so the container root
// filesystem can be read-only), adds security headers, answers /healthz and serves
// precompressed .gz/.br siblings. It depends on the standard library only (its metrics come from
// core/obs, which is standard library too; set CAN_METRICS_ADDR to enable them).
//
// Usage: can-static [-dir /srv/site] [-addr :8080] [-access-log] | can-static -healthcheck
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
	"github.com/dodwmd/companyinacan/core/staticsrv"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	dir, addr   string
	accessLog   bool
	healthcheck bool
	extract     string
}

// maxExtractBytes bounds the unpacked site (a ConfigMap carries at most 1 MiB compressed).
const maxExtractBytes = 64 << 20

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("can-static", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.dir, "dir", envOr("CAN_STATIC_DIR", "/srv/site"), "directory to serve (env CAN_STATIC_DIR)")
	fs.StringVar(&c.addr, "addr", envOr("CAN_STATIC_ADDR", ":8080"), "listen address (env CAN_STATIC_ADDR)")
	fs.BoolVar(&c.accessLog, "access-log", false, "log one line per request to stderr")
	fs.BoolVar(&c.healthcheck, "healthcheck", false, "probe http://<addr>/healthz and exit 0 when healthy (for exec probes in a distroless image)")
	fs.StringVar(&c.extract, "extract", "", "unpack this site .tar.gz into -dir and exit (used by the site pod's init container)")
	return c, fs.Parse(args)
}

// lineLog is the two-method logger obs needs, without pulling log/slog into this tiny binary.
// Values are %q-quoted, so nothing a caller passes can forge a line.
type lineLog struct{ w io.Writer }

func (l lineLog) Info(msg string, args ...any)  { l.line("info", msg, args) }
func (l lineLog) Warn(msg string, args ...any)  { l.line("warn", msg, args) }
func (l lineLog) Error(msg string, args ...any) { l.line("error", msg, args) }
func (l lineLog) line(level, msg string, args []any) {
	fmt.Fprintf(l.w, "can-static: %s %q", level, msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(l.w, " %v=%q", args[i], fmt.Sprint(args[i+1]))
	}
	fmt.Fprintln(l.w)
}

func probe(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz") //nolint:noctx // short-lived probe with a client timeout
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, "can-static:", err)
		return 2
	}
	if cfg.extract != "" {
		f, err := os.Open(cfg.extract)
		if err != nil {
			fmt.Fprintln(stderr, "can-static:", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		if err := staticsrv.Extract(f, cfg.dir, maxExtractBytes); err != nil {
			fmt.Fprintln(stderr, "can-static:", err)
			return 1
		}
		fmt.Fprintf(stdout, "can-static: unpacked %s into %s\n", cfg.extract, cfg.dir)
		return 0
	}
	if cfg.healthcheck {
		if err := probe(cfg.addr); err != nil {
			fmt.Fprintln(stderr, "can-static: unhealthy:", err)
			return 1
		}
		return 0
	}
	sc := staticsrv.Config{Dir: cfg.dir}
	if cfg.accessLog {
		sc.AccessLog = stderr
	}
	// Metrics (epic E22): off unless CAN_METRICS_ADDR is set; see docs/observability.md.
	if err := obs.Setup(ctx, os.Getenv, obs.BuildInfo{Version: version}, lineLog{stderr}); err != nil {
		fmt.Fprintln(stderr, "can-static: metrics:", err)
		return 2
	}
	srv := staticsrv.NewServer(cfg.addr, obs.Instrument(staticsrv.New(sc), obs.FixedRoute("static")))
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		fmt.Fprintln(stderr, "can-static:", err)
		return 1
	}
	fmt.Fprintf(stdout, "can-static serving %s on %s\n", cfg.dir, ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		fmt.Fprintln(stderr, "can-static:", err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		fmt.Fprintln(stderr, "can-static: shutdown:", err)
		return 1
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, "can-static:", err)
		return 1
	}
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

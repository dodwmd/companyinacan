package jobs

import (
	"context"
	"flag"
	"fmt"
	"github.com/dodwmd/companyinacan/core/modclient"
	"io"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

// Version is set at build time with -ldflags "-X .../core/jobs.Version=...".
var Version = "dev"

const defaultConfigPath = "/etc/can-jobs/jobs.yaml"

// Main is the can-jobs entry point. It returns the process exit code:
// 0 ok, 1 runtime failure, 2 usage/config/secret problems.
func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("can-jobs "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to the work config file (default "+defaultConfigPath+" when present, else built-in defaults)")
	addr := fs.String("addr", "127.0.0.1:8080", "healthcheck: address of the API listener")

	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "can-jobs %s\n", Version)
		return 0
	case "serve", "check-config", "healthcheck", "verify-audit", "rotate-keys":
	default:
		fmt.Fprintf(stderr, "unknown command %q (serve, check-config, healthcheck, verify-audit, rotate-keys, version)\n", cmd)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cmd == "healthcheck" {
		return healthcheck(*addr, stderr)
	}
	path, must := *cfgPath, *cfgPath != ""
	if path == "" {
		if path = getenv("CAN_JOBS_CONFIG"); path != "" {
			must = true
		} else {
			path = defaultConfigPath
		}
	}
	cfg, err := LoadConfig(path, must)
	if err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	sec, err := SecretsFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "secrets: %v\n", err)
		return 2
	}
	if cmd == "check-config" {
		fmt.Fprintln(stdout, "config ok")
		return 0
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("service", "can-jobs", "version", Version)
	can, err := modclient.FromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "can-core: %v\n", err)
		return 2
	}
	app, err := NewApp(cfg, sec, Options{Logger: log, Can: can})
	if err != nil {
		fmt.Fprintf(stderr, "start: %v\n", err)
		return 2
	}
	switch cmd {
	case "verify-audit":
		defer func() { _ = app.Close() }()
		if err := app.Store().VerifyAudit(context.Background()); err != nil {
			fmt.Fprintf(stderr, "audit: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "audit chain ok")
		return 0
	case "rotate-keys":
		defer func() { _ = app.Close() }()
		n, err := app.Store().RotateKeys(context.Background())
		if err != nil {
			fmt.Fprintf(stderr, "rotate-keys: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "re-sealed %d values\n", n)
		return 0
	}
	ln, err := listen(cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		_ = app.Close()
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Serve(ctx, ln); err != nil {
		log.Error("exit", "error", err)
		return 1
	}
	return 0
}

// healthcheck exists because the distroless image has no curl or shell.
func healthcheck(addr string, stderr io.Writer) int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

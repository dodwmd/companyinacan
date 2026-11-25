package forms

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Version is set at build time with -ldflags "-X .../core/forms.Version=...".
var Version = "dev"

const defaultConfigPath = "/etc/can-forms/forms.yaml"

// Main is the can-forms entry point. It returns the process exit code:
// 0 ok, 1 runtime failure, 2 usage/config/secret problems.
func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("can-forms "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", firstNonEmpty(getenv("CAN_FORMS_CONFIG"), defaultConfigPath), "path to the forms config file")
	addr := fs.String("addr", "127.0.0.1:8080", "healthcheck: address of the public listener")

	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "can-forms %s\n", Version)
		return 0
	case "serve", "check-config", "healthcheck":
	default:
		fmt.Fprintf(stderr, "unknown command %q (serve, check-config, healthcheck, version)\n", cmd)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if cmd == "healthcheck" {
		return healthcheck(*addr, stderr)
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	if cmd == "check-config" {
		fmt.Fprintf(stdout, "config ok: %d form(s)\n", len(cfg.Forms))
		return 0
	}

	sec, err := SecretsFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "secrets: %v\n", err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("service", "can-forms", "version", Version)
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
	pub, err := listen(cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	api, err := listen(cfg.APIListen)
	if err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Serve(ctx, pub, api); err != nil {
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

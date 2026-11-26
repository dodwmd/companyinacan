package finance

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/obs"
	obslog "github.com/dodwmd/companyinacan/core/obs/log"
)

// Version is set at build time with -ldflags "-X .../core/finance.Version=...".
var Version = "dev"

const defaultConfigPath = "/etc/can-finance/finance.yaml"

// Main is the can-finance entry point. It returns the process exit code:
// 0 ok, 1 runtime failure, 2 usage/config/secret problems.
func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("can-finance "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to the finance config file (default "+defaultConfigPath+" when present, else built-in defaults)")
	addr := fs.String("addr", "127.0.0.1:8080", "healthcheck: address of the API listener")

	var subject, class string
	if cmd == "purge" {
		fs.StringVar(&subject, "subject", "", "purge: the subject of the erasure (party-<id>-<creation time>)")
		fs.StringVar(&class, "class", "", "purge: the data class ("+ClassPartyContact+")")
	}

	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "can-finance %s\n", Version)
		return 0
	case "serve", "check-config", "healthcheck", "purge":
	default:
		fmt.Fprintf(stderr, "unknown command %q (serve, check-config, healthcheck, purge, version)\n", cmd)
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
		if path = getenv("CAN_FINANCE_CONFIG"); path != "" {
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
	if cmd == "purge" { // the replay of one erasure, run by `can restore` (specs/056-erasure-finance-work)
		return runPurge(cfg, subject, class, stdout, stderr)
	}
	log := obslog.New(stderr, obslog.Options{Service: "can-finance", Version: Version, Level: getenv("CAN_LOG_LEVEL"), Format: getenv("CAN_LOG_FORMAT")})
	can, err := modclient.FromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "can-core: %v\n", err)
		return 2
	}
	app, err := NewApp(cfg, sec, Options{Logger: log, Can: can, CanSecret: getenv("CAN_WEBHOOK_SECRET")})
	if err != nil {
		fmt.Fprintf(stderr, "start: %v\n", err)
		return 2
	}
	ln, err := listen(cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		_ = app.Close()
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := obs.Setup(ctx, getenv, obs.BuildInfo{Version: Version}, log); err != nil {
		fmt.Fprintf(stderr, "metrics: %v\n", err)
		return 2
	}
	if err := app.Serve(ctx, ln); err != nil {
		log.Error("exit", "error", err)
		return 1
	}
	return 0
}

func runPurge(cfg *Config, subject, class string, stdout, stderr io.Writer) int {
	if subject == "" || class == "" {
		fmt.Fprintln(stderr, "purge needs -subject and -class")
		return 2
	}
	st, err := OpenStore(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(stderr, "purge: %v\n", err)
		return 1
	}
	rep, err := st.PurgeSubject(context.Background(), subject, class, cfg.RetentionYears)
	if err = errors.Join(err, st.Close()); err != nil {
		fmt.Fprintf(stderr, "purge: %v\n", err)
		return 1
	}
	if rep == nil {
		fmt.Fprintf(stdout, "nothing to purge for %s\n", subject)
		return 0
	}
	kept := 0
	for _, r := range rep.Retained {
		kept += r.Count
	}
	fmt.Fprintf(stdout, "purged %s for %s (%d drafts deleted, %d records kept by legal retention)\n", class, subject, rep.DeletedDrafts, kept)
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

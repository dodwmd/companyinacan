package equity

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/obs"
	obslog "github.com/dodwmd/companyinacan/core/obs/log"
)

// Version is set at build time with -ldflags "-X .../core/equity.Version=...".
var Version = "dev"

const defaultConfigPath = "/etc/can-equity/equity.yaml"

// Main is the can-equity entry point. Exit codes: 0 ok, 1 runtime failure, 2 usage/config/secret problems.
func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("can-equity "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", firstNonEmpty(getenv("CAN_EQUITY_CONFIG"), defaultConfigPath), "path to the equity config file")
	addr := fs.String("addr", "127.0.0.1:8081", "healthcheck: address of the API listener")
	subject := fs.String("subject", "", "purge: opaque subject id (a stakeholder id)")
	class := fs.String("class", "", "purge: data class")

	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "can-equity %s\n", Version)
		return 0
	case "serve", "check-config", "healthcheck", "rotate-keys", "purge", "retention", "verify-audit":
	default:
		fmt.Fprintf(stderr, "unknown command %q (serve, check-config, healthcheck, rotate-keys, purge, retention, verify-audit, version)\n", cmd)
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
		fmt.Fprintln(stdout, "config ok")
		return 0
	}
	rules, err := cfg.RetentionRules()
	if err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	kr, err := LoadKeyring(firstNonEmpty(getenv("CAN_EQUITY_KEY_FILE"), cfg.KeyringFile))
	if err != nil {
		fmt.Fprintf(stderr, "keys: %v\n", err)
		return 2
	}
	log := obslog.New(stderr, obslog.Options{Service: "can-equity", Version: Version, Level: getenv("CAN_LOG_LEVEL"), Format: getenv("CAN_LOG_FORMAT")})

	if cmd != "serve" {
		return maintenance(cmd, cfg, kr, rules, *subject, *class, stdout, stderr)
	}

	sec, err := SecretsFromEnv(getenv, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "secrets: %v\n", err)
		return 2
	}
	svc, err := Open(cfg.DBPath, Options{Keyring: kr, Retention: rules})
	if err != nil {
		fmt.Fprintf(stderr, "start: %v\n", err)
		return 2
	}
	defer func() { _ = svc.Close() }()

	st := &StaticTokens{}
	st.Add(sec.APIToken, Principal{Subject: "svc-can-core", Roles: []string{RoleHR, RoleFinance, RoleAdmin}})
	auth := MultiAuth{st}
	if cfg.Introspect.URL != "" {
		auth = append(auth, &IntrospectAuthenticator{URL: cfg.Introspect.URL, Token: sec.IntrospectToken})
	}
	can, err := modclient.FromEnv(getenv) // installed under can-core: personal access tokens
	if err != nil {
		fmt.Fprintf(stderr, "can-core: %v\n", err)
		return 2
	}
	if can != nil {
		auth = append(auth, &CanAuth{In: &modclient.Introspector{API: can}})
	}
	var disp *Dispatcher
	if cfg.Webhook.URL != "" {
		disp = NewDispatcher(cfg.Webhook.URL, []byte(sec.WebhookSecret), log)
	}
	ln, err := listen(cfg.Listen)
	if err != nil {
		fmt.Fprintf(stderr, "listen: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := obs.Setup(ctx, getenv, obs.BuildInfo{Version: Version}, log); err != nil {
		fmt.Fprintf(stderr, "metrics: %v\n", err)
		return 2
	}
	api := &API{Svc: svc, Auth: auth, Log: log}
	srv := &http.Server{Handler: obs.Instrument(api.Handler()), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); background(ctx, svc, disp, log) }()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("can-equity listening", "addr", ln.Addr().String(), "key_id", kr.ActiveID())
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runErr = errors.Join(runErr, srv.Shutdown(shut))
	stop()
	wg.Wait()
	if runErr != nil {
		log.Error("exit", "error", runErr)
		return 1
	}
	return 0
}

// background expires lapsed grants, announces vesting and runs the retention sweep, and delivers events.
func background(ctx context.Context, svc *Service, d *Dispatcher, log *slog.Logger) {
	due := func() {
		if n, err := svc.ApplyDue(ctx); err != nil && ctx.Err() == nil {
			log.Error("apply due vesting and expiry", "error", err)
		} else if n > 0 {
			log.Info("applied due vesting and expiry", "count", n)
		}
	}
	maint := func() {
		due()
		if rep, err := svc.RunRetention(ctx); err != nil && ctx.Err() == nil {
			log.Error("retention sweep", "error", err)
		} else if rep != nil && len(rep.Purged) > 0 {
			log.Info("retention sweep", "purged", rep.Purged)
		}
	}
	maint()
	hour := time.NewTicker(time.Hour)
	defer hour.Stop()
	sweep := time.NewTicker(time.Hour) // vesting and expiry are dated in days: an hourly sweep is plenty
	defer sweep.Stop()
	var tick <-chan time.Time
	if d != nil {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			due()
		case <-hour.C:
			maint()
		case <-tick:
			if _, err := svc.DeliverPending(ctx, d); err != nil && ctx.Err() == nil {
				log.Error("deliver events", "error", err)
			}
		}
	}
}

func maintenance(cmd string, cfg *Config, kr *Keyring, rules map[string]RetentionRule, subject, class string, stdout, stderr io.Writer) int {
	svc, err := Open(cfg.DBPath, Options{Keyring: kr, Retention: rules})
	if err != nil {
		fmt.Fprintf(stderr, "start: %v\n", err)
		return 2
	}
	defer func() { _ = svc.Close() }()
	ctx := context.Background()
	switch cmd {
	case "rotate-keys":
		n, err := svc.RotateKeys(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "rotate-keys: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "re-sealed %d value(s) under key %q\n", n, kr.ActiveID())
	case "purge": // the module's purge(subject, class) hook, also replayed from the erasure ledger on restore
		if subject == "" || class == "" {
			fmt.Fprintln(stderr, "purge needs -subject and -class")
			return 2
		}
		classes := []string{class}
		if class == "all" {
			classes = PurgeClasses
		}
		for _, c := range classes {
			if err := svc.PurgeSubject(ctx, subject, c); err != nil {
				fmt.Fprintf(stderr, "purge: %v\n", err)
				return 1
			}
		}
		fmt.Fprintf(stdout, "purged %s for %s\n", strings.Join(classes, ","), subject)
	case "retention":
		rep, err := svc.RunRetention(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "retention: %v\n", err)
			return 1
		}
		_ = json.NewEncoder(stdout).Encode(rep)
	case "verify-audit":
		if err := svc.VerifyAudit(ctx); err != nil {
			fmt.Fprintf(stderr, "audit: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "audit chain ok")
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

func listen(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, ":") {
		addr = "0.0.0.0" + addr
	}
	return net.Listen("tcp", addr)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

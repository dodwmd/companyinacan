package provision

import (
	"context"
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
)

const defaultConfigPath = "/etc/can-provision/provision.yaml"

// Main is the can-provision entry point. Exit codes: 0 ok, 1 runtime failure, 2 usage, config or
// secret problems.
func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("can-provision "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", firstNonEmpty(getenv("CAN_PROVISIONING_CONFIG"), defaultConfigPath), "path to the provisioning config file")
	addr := fs.String("addr", "127.0.0.1:8090", "healthcheck: address of the listener")
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "can-provision %s\n", Version)
		return 0
	case "serve", "check-config", "healthcheck", "verify-audit":
	default:
		fmt.Fprintf(stderr, "unknown command %q (serve, check-config, healthcheck, verify-audit, version)\n", cmd)
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
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	pol, _ := cfg.Policy()
	conns, err := BuildConnectors(cfg, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "connectors: %v\n", err)
		return 2
	}
	apiTok, dirTok := getenv("CAN_PROVISIONING_API_TOKEN"), getenv("CAN_PROVISIONING_DIRECTORY_TOKEN")
	if len(apiTok) < 24 {
		fmt.Fprintln(stderr, "secrets: CAN_PROVISIONING_API_TOKEN must be at least 24 characters")
		return 2
	}
	if len(dirTok) < 8 {
		fmt.Fprintln(stderr, "secrets: CAN_PROVISIONING_DIRECTORY_TOKEN is unset or too short (a token of the people module with people.read and people.write)")
		return 2
	}
	if cmd == "check-config" {
		fmt.Fprintln(stdout, "config ok")
		return 0
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("service", "can-provision", "version", Version)
	st, err := OpenStore(cfg.DBPath, nil)
	if err != nil {
		fmt.Fprintf(stderr, "start: %v\n", err)
		return 2
	}
	defer func() { _ = st.Close() }()
	if cmd == "verify-audit" {
		if err := st.VerifyAudit(context.Background()); err != nil {
			fmt.Fprintf(stderr, "audit: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "audit chain ok")
		return 0
	}

	dir := &HTTPDirectory{BaseURL: cfg.Directory.URL, Token: dirTok}
	can, err := modclient.FromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "can-core: %v\n", err)
		return 2
	}
	eng := &Engine{Store: st, Dir: dir, Conns: conns, Tasks: &HTTPTasks{Dir: dir}, Policy: pol, Log: log}
	auth := MultiAuth{StaticToken{Token: apiTok}}
	var canEvents http.Handler
	if can != nil {
		eng.Core, eng.Approvals = can, can // typed values only: never a nil pointer inside an interface
		auth = append(auth, &CanAuth{In: &modclient.Introspector{API: can}})
		secret := getenv("CAN_WEBHOOK_SECRET")
		if len(secret) < 16 {
			fmt.Fprintln(stderr, "secrets: CAN_WEBHOOK_SECRET is unset or shorter than 16 characters")
			return 2
		}
		canEvents = &modclient.Receiver{Secret: secret, Handle: eng.HandleEvent}
	}
	scim := &SCIM{Store: st, Dir: dir, Log: log, Cfg: cfg.SCIM, OnMembership: func(ctx context.Context, ids []string) { eng.groupMovers(ctx, ids) }}
	api := &API{Eng: eng, Auth: auth, Can: canEvents, Log: log, ConnInfo: func() []ConnectorInfo { return connectorInfo(cfg, conns) }}
	if !cfg.SCIM.Disabled {
		api.SCIM = scim
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
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var pub Publisher
		if can != nil {
			pub = can
		}
		eng.Run(ctx, pub)
	}()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("can-provision listening", "addr", ln.Addr().String(), "connectors", conns.Names(), "dry_run", pol.DryRun, "scim", !cfg.SCIM.Disabled)
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

func connectorInfo(cfg *Config, cs *ConnectorSet) []ConnectorInfo {
	var out []ConnectorInfo
	for _, cc := range cfg.Connector {
		if c, ok := cs.Get(cc.Name); ok {
			out = append(out, ConnectorInfo{Name: cc.Name, Type: cc.Type, Verified: c.Verified(), Privileged: cc.Privileged})
		}
	}
	return out
}

// groupMovers starts a mover run for people whose SCIM group membership changed and who already
// have a downstream account. Best effort: a failure is logged, and an administrator can start the
// mover through the API.
func (e *Engine) groupMovers(ctx context.Context, ids []string) {
	for _, id := range ids {
		if !partyRe.MatchString(id) {
			continue
		}
		accts, err := e.Store.activeAccounts(ctx, id)
		if err != nil || len(accts) == 0 {
			continue
		}
		key := fmt.Sprintf("scim-%d", e.now().UnixNano())
		if _, _, err := e.Start(ctx, StartRequest{Kind: KindMover, PartyID: id, Trigger: "scim", Key: key}); err != nil && !errors.Is(err, ErrConflict) {
			e.log().Warn("provision: mover for a group change not started", "party_id", id, "error", err)
		}
	}
}

// Run is the worker loop: it advances runs, delivers events and prunes old rows until ctx ends.
// Runs are woken at once by new work and otherwise polled every 5 seconds, so a crash or a
// restart picks up where the persisted state says.
func (e *Engine) Run(ctx context.Context, pub Publisher) {
	e.Wake()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	for {
		if _, err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.log().Error("provision: worker pass failed", "error", err)
		}
		if pub != nil {
			if _, err := e.DeliverPending(ctx, pub); err != nil && ctx.Err() == nil {
				e.log().Error("provision: event delivery failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-e.wake:
		case <-prune.C:
			if err := e.Store.Prune(ctx, 30*24*time.Hour); err != nil && ctx.Err() == nil {
				e.log().Error("provision: prune failed", "error", err)
			}
		}
	}
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

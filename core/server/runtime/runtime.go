// Package runtime wires can-core together: configuration from the environment, the two HTTP
// listeners (admin and module API, separate ports per SPEC §3.1), background workers and graceful
// shutdown.
package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/server/adminapi"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

// Config is can-core's configuration (all from CAN_* environment variables).
type Config struct {
	AdminAddr       string        // CAN_ADMIN_ADDR, default :8080
	ModuleAddr      string        // CAN_MODULE_ADDR, default :8090
	DBPath          string        // CAN_DB_PATH
	SecureCookies   bool          // CAN_COOKIE_SECURE, default true
	TrustProxy      bool          // CAN_TRUST_PROXY: take the client IP from X-Forwarded-For (set it behind Traefik; see deploy notes)
	TokenReview     string        // CAN_TOKEN_REVIEW: kubernetes (default) or fake (development only)
	PlatformURL     string        // CAN_PLATFORM_URL: module-API URL as seen from module namespaces
	KubeAPI         string        // CAN_KUBE_API
	SAToken         string        // CAN_SA_TOKEN_FILE
	SACA            string        // CAN_SA_CA_FILE
	MemLimitMB      int           // CAN_MEMLIMIT_MB: soft Go memory limit (idle RAM target, see plan.md)
	LogLevel        string        // CAN_LOG_LEVEL
	ShutdownTimeout time.Duration // CAN_SHUTDOWN_TIMEOUT_SECONDS

	// OnListen is called once both listeners are bound (tests use it to learn ports).
	OnListen func(admin, module net.Addr)
	// Reviewer overrides the token reviewer (tests).
	Reviewer tokenreview.Reviewer
}

// DefaultMemLimitMB is the soft memory limit applied when GOMEMLIMIT and CAN_MEMLIMIT_MB are unset.
const DefaultMemLimitMB = 64

// FromEnv reads the configuration via getenv (os.Getenv in production).
func FromEnv(getenv func(string) string) (Config, error) {
	str := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	boolean := func(k string, def bool) (bool, error) {
		switch v := strings.ToLower(getenv(k)); v {
		case "":
			return def, nil
		case "1", "true", "yes":
			return true, nil
		case "0", "false", "no":
			return false, nil
		default:
			return false, fmt.Errorf("%s: %q is not a boolean", k, v)
		}
	}
	var err error
	c := Config{
		AdminAddr: str("CAN_ADMIN_ADDR", ":8080"), ModuleAddr: str("CAN_MODULE_ADDR", ":8090"),
		DBPath: str("CAN_DB_PATH", "/var/lib/can/state.db"), TokenReview: str("CAN_TOKEN_REVIEW", "kubernetes"),
		PlatformURL: str("CAN_PLATFORM_URL", "http://can-core.can-system.svc.cluster.local:8090"),
		KubeAPI:     str("CAN_KUBE_API", "https://kubernetes.default.svc"),
		SAToken:     str("CAN_SA_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
		SACA:        str("CAN_SA_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"),
		LogLevel:    str("CAN_LOG_LEVEL", "info"), ShutdownTimeout: 15 * time.Second,
	}
	if c.SecureCookies, err = boolean("CAN_COOKIE_SECURE", true); err != nil {
		return c, err
	}
	if c.TrustProxy, err = boolean("CAN_TRUST_PROXY", false); err != nil {
		return c, err
	}
	if v := getenv("CAN_MEMLIMIT_MB"); v != "" {
		if c.MemLimitMB, err = strconv.Atoi(v); err != nil || c.MemLimitMB < 0 {
			return c, fmt.Errorf("CAN_MEMLIMIT_MB: %q is not a non-negative integer", v)
		}
	}
	if v := getenv("CAN_SHUTDOWN_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("CAN_SHUTDOWN_TIMEOUT_SECONDS: %q is not a positive integer", v)
		}
		c.ShutdownTimeout = time.Duration(n) * time.Second
	}
	if c.TokenReview != "kubernetes" && c.TokenReview != "fake" {
		return c, fmt.Errorf("CAN_TOKEN_REVIEW: %q must be kubernetes or fake", c.TokenReview)
	}
	if c.AdminAddr == c.ModuleAddr {
		return c, errors.New("CAN_ADMIN_ADDR and CAN_MODULE_ADDR must differ: the admin and module APIs are separate listeners")
	}
	return c, nil
}

// Logger builds the structured JSON logger for the configured level.
func (c Config) Logger(w *os.File) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(c.LogLevel)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}

func (c Config) reviewer(log *slog.Logger) (tokenreview.Reviewer, error) {
	if c.Reviewer != nil {
		return c.Reviewer, nil
	}
	if c.TokenReview == "fake" {
		log.Warn("CAN_TOKEN_REVIEW=fake: module tokens are NOT verified against Kubernetes; development only")
		return tokenreview.NewFake(), nil
	}
	pool := x509.NewCertPool()
	ca, err := os.ReadFile(c.SACA)
	if err != nil {
		return nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return &tokenreview.HTTP{APIServer: c.KubeAPI, Client: client, BearerToken: tokenreview.FileToken(c.SAToken)}, nil
}

// Run starts can-core and blocks until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if cfg.MemLimitMB > 0 {
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	} else if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(DefaultMemLimitMB << 20)
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer func() { _ = db.Close() }()
	rev, err := cfg.reviewer(log)
	if err != nil {
		return err
	}

	al := audit.New(db)
	reg := registry.New(db, cfg.PlatformURL)
	bus := events.NewBus(db)
	bus.SetLogger(log)
	disp := webhook.NewDispatcher(db, webhook.NewHTTPClient(10*time.Second))
	disp.Log = log

	adminSrv := &adminapi.Server{
		Auth: auth.NewService(db, al, auth.Options{Log: log, Policy: loginPolicy(cfg)}), DB: db, Audit: al, Registry: reg,
		Log: log, Secure: cfg.SecureCookies, TrustProxy: cfg.TrustProxy,
	}
	moduleSrv := &moduleapi.Server{Auth: modauth.New(rev, db), Registry: reg, Bus: bus, Log: log}

	adminLn, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		return err
	}
	moduleLn, err := net.Listen("tcp", cfg.ModuleAddr)
	if err != nil {
		_ = adminLn.Close()
		return err
	}
	if cfg.OnListen != nil {
		cfg.OnListen(adminLn.Addr(), moduleLn.Addr())
	}
	log.Info("can-core listening", "admin", adminLn.Addr().String(), "module", moduleLn.Addr().String(), "db", cfg.DBPath)

	mk := func(h http.Handler) *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	}
	servers := []struct {
		srv *http.Server
		ln  net.Listener
	}{{mk(adminSrv.Handler()), adminLn}, {mk(moduleSrv.Handler()), moduleLn}}

	workCtx, stopWork := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); disp.Run(workCtx, bus.Wake(), 30*time.Second) }()
	wg.Add(1)
	go func() { defer wg.Done(); housekeeping(workCtx, db, log) }()

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	for _, s := range servers {
		if err := s.srv.Shutdown(sctx); err != nil && runErr == nil {
			runErr = err
		}
	}
	stopWork()
	wg.Wait()
	return runErr
}

// housekeeping prunes expired sessions and old finished outbox rows (30-day TTL, SPEC §3.7).
func housekeeping(ctx context.Context, db *store.DB, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := db.DeleteExpiredSessions(ctx, time.Now()); err != nil {
				log.Error("session cleanup failed", "error", err)
			}
			if _, err := db.PruneLoginAttempts(ctx, time.Now(), auth.DefaultPolicy.Window); err != nil {
				log.Error("login attempt pruning failed", "error", err)
			}
			if _, err := db.PruneEvents(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
				log.Error("outbox pruning failed", "error", err)
			}
		}
	}
}

// loginPolicy chooses the throttling mode. With CAN_TRUST_PROXY the client address comes from the
// ingress and is real, so repeated failures refuse attempts (HardLock). Without it every client
// looks like the ingress, and refusing would let anyone lock the admin out, so failures only slow
// logins down (see auth.Service.Login).
func loginPolicy(cfg Config) auth.Policy {
	p := auth.DefaultPolicy
	p.HardLock = cfg.TrustProxy
	return p
}

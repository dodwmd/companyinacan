package work

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

var errEOF = io.EOF

// Secrets are read from the environment, never from the config file.
type Secrets struct {
	AdminToken    string // bootstrap token with every scope (CAN_WORK_ADMIN_TOKEN, >= 32 characters)
	WebhookSecret string // HMAC secret for webhook signatures (CAN_WORK_WEBHOOK_SECRET)
}

// SecretsFromEnv loads and sanity-checks the secrets.
func SecretsFromEnv(getenv func(string) string) (Secrets, error) {
	s := Secrets{AdminToken: getenv("CAN_WORK_ADMIN_TOKEN"), WebhookSecret: getenv("CAN_WORK_WEBHOOK_SECRET")}
	var errs []error
	if len(s.AdminToken) < 32 || distinctBytes(s.AdminToken) < 10 {
		errs = append(errs, errors.New("CAN_WORK_ADMIN_TOKEN is required, at least 32 characters with at least 10 different ones (for example `openssl rand -base64 32`)"))
	}
	if s.WebhookSecret != "" && len(s.WebhookSecret) < 16 {
		errs = append(errs, errors.New("CAN_WORK_WEBHOOK_SECRET must be at least 16 characters when set"))
	}
	return s, errors.Join(errs...)
}

// Options override collaborators; tests use them to inject fakes.
type Options struct {
	Auth   Authenticator // replaces the built-in token authenticators (introspection seam)
	Logger *slog.Logger
	Now    func() time.Time
	Client *http.Client
}

// App wires the store, the webhook dispatcher and the HTTP API.
type App struct {
	cfg        *Config
	store      *Store
	dispatcher *Dispatcher
	handler    http.Handler
	log        *slog.Logger
	now        func() time.Time
	closeOnce  sync.Once
	closeErr   error
}

// NewApp builds the application. It opens (and migrates) the database.
func NewApp(cfg *Config, sec Secrets, opt Options) (*App, error) {
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	if cfg.Webhook.URL != "" && sec.WebhookSecret == "" {
		return nil, errors.New("webhook.url is set but CAN_WORK_WEBHOOK_SECRET is empty")
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	store.now = now
	store.events = cfg.Webhook.URL != ""

	auth := opt.Auth
	if auth == nil {
		st := &StaticTokens{}
		st.Add(sec.AdminToken, Principal{Subject: bootstrapSubject, Kind: "static", Permissions: []string{PermAdmin}})
		auth = Chain{st, TokenAuth{Store: store}}
	}
	app := &App{cfg: cfg, store: store, log: log, now: now}
	if store.events {
		app.dispatcher = &Dispatcher{store: store, URL: cfg.Webhook.URL, Secret: []byte(sec.WebhookSecret), Client: opt.Client, Log: log, Now: now}
	}
	a := &api{
		store: store, auth: auth, log: log, maxBody: cfg.MaxBodyBytes, trustProxy: cfg.TrustProxy,
		limiter:  newRateLimiter(cfg.RateLimit.PerMinute, time.Minute, cfg.RateLimit.MaxTracked, now),
		penalty:  300 * time.Millisecond,
		failures: newRateLimiter(cfg.RateLimit.AuthFailuresPerMin, time.Minute, cfg.RateLimit.MaxTracked, now),
	}
	app.handler = withMiddleware(log, a.handler())
	return app, nil
}

// Handler serves the API.
func (a *App) Handler() http.Handler { return a.handler }

// Store exposes the store (tests and tooling).
func (a *App) Store() *Store { return a.store }

// Purge applies retention and returns how many issues were deleted.
func (a *App) Purge(ctx context.Context) (int64, error) {
	return a.store.Purge(ctx, a.cfg.RetentionDays, a.cfg.AuditRetentionDays)
}

// Close closes the database. It is safe to call repeatedly.
func (a *App) Close() error {
	a.closeOnce.Do(func() { a.closeErr = a.store.Close() })
	return a.closeErr
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// Serve runs the API until ctx is cancelled or the server fails, then shuts down
// gracefully: stop accepting, let in-flight requests finish, close the database.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	srv := newServer(a.handler)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	bg, stopBg := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.purgeLoop(bg) }()
	if a.dispatcher != nil {
		wg.Add(1)
		go func() { defer wg.Done(); a.dispatcher.Run(bg) }()
	}
	a.log.Info("can-work listening", "addr", ln.Addr().String(), "webhook", a.dispatcher != nil)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.log.Info("shutting down")
	err := errors.Join(runErr, srv.Shutdown(shutCtx))
	stopBg()
	wg.Wait()
	if err = errors.Join(err, a.Close()); err != nil {
		return err
	}
	a.log.Info("shutdown complete")
	return nil
}

func (a *App) purgeLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := a.Purge(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("retention purge", "error", err)
		} else if n > 0 {
			a.log.Info("retention purge", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// listen opens a TCP listener for an address like ":8080".
func listen(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, ":") {
		addr = "0.0.0.0" + addr
	}
	return net.Listen("tcp", addr)
}

func distinctBytes(s string) int {
	var seen [256]bool
	n := 0
	for i := 0; i < len(s); i++ {
		if !seen[s[i]] {
			seen[s[i]] = true
			n++
		}
	}
	return n
}

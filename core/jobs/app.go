package jobs

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"

	"github.com/dodwmd/companyinacan/core/forms"
)

// Secrets are read from the environment (and the mounted keyring), never from the config file.
type Secrets struct {
	AdminToken    string // bootstrap admin token (CAN_JOBS_ADMIN_TOKEN, >= 32 characters)
	HashKey       string // HMAC key for the candidate email dedupe hash (CAN_JOBS_HASH_KEY, >= 32 characters)
	WebhookSecret string // HMAC secret for webhook signatures (CAN_JOBS_WEBHOOK_SECRET)
	FormSecret    string // signs the apply form's timing tokens (CAN_JOBS_FORM_SECRET); random per start when empty
	CaptchaSecret string // CAN_JOBS_CAPTCHA_SECRET, when apply.captcha_provider is set
	// CanSecret verifies the webhooks can-core delivers to this module (CAN_WEBHOOK_SECRET).
	CanSecret string
}

// SecretsFromEnv loads and sanity-checks the secrets.
func SecretsFromEnv(getenv func(string) string) (Secrets, error) {
	s := Secrets{AdminToken: getenv("CAN_JOBS_ADMIN_TOKEN"), HashKey: getenv("CAN_JOBS_HASH_KEY"), WebhookSecret: getenv("CAN_JOBS_WEBHOOK_SECRET"),
		FormSecret: getenv("CAN_JOBS_FORM_SECRET"), CaptchaSecret: getenv("CAN_JOBS_CAPTCHA_SECRET"), CanSecret: getenv("CAN_WEBHOOK_SECRET")}
	var errs []error
	if len(s.AdminToken) < 32 || distinctBytes(s.AdminToken) < 10 {
		errs = append(errs, errors.New("CAN_JOBS_ADMIN_TOKEN is required, at least 32 characters with at least 10 different ones (for example `openssl rand -base64 32`)"))
	}
	if len(s.HashKey) < 32 || distinctBytes(s.HashKey) < 10 {
		errs = append(errs, errors.New("CAN_JOBS_HASH_KEY is required, at least 32 characters with at least 10 different ones; changing it later breaks duplicate detection for existing candidates"))
	}
	if s.WebhookSecret != "" && len(s.WebhookSecret) < 16 {
		errs = append(errs, errors.New("CAN_JOBS_WEBHOOK_SECRET must be at least 16 characters when set"))
	}
	if s.FormSecret != "" && len(s.FormSecret) < 16 {
		errs = append(errs, errors.New("CAN_JOBS_FORM_SECRET must be at least 16 characters when set"))
	}
	return s, errors.Join(errs...)
}

// Options override collaborators; tests and later modules use them to inject implementations.
type Options struct {
	// Can is the can-core client of an installed module (CAN_API_URL): personal access tokens are
	// introspected through it and events are published through it instead of a webhook URL.
	Can         *modclient.Client
	Auth        Authenticator // replaces the built-in token authenticators (introspection seam)
	Logger      *slog.Logger
	Now         func() time.Time
	Client      *http.Client
	Keyring     *Keyring
	Resumes     ResumeStore
	Scanner     Scanner
	Syndicators []Syndicator
	Approver    Approver
}

// App wires the store, the webhook dispatcher and the HTTP surfaces.
type App struct {
	cfg        *Config
	store      *Store
	dispatcher *Dispatcher
	handler    http.Handler
	ui         *UI // nil unless ui.enabled
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
		return nil, errors.New("webhook.url is set but CAN_JOBS_WEBHOOK_SECRET is empty")
	}
	if cfg.Company.Name == "" || cfg.Company.BaseURL == "" {
		return nil, errors.New("company.name and company.base_url are required: the careers pages and feeds need absolute URLs")
	}
	kr := opt.Keyring
	if kr == nil {
		var err error
		if kr, err = LoadKeyring(cfg.KeyringPath); err != nil {
			return nil, err
		}
	}
	res := opt.Resumes
	if res == nil {
		var err error
		if res, err = NewFSResumeStore(cfg.ResumeDir); err != nil {
			return nil, err
		}
	}
	approver := opt.Approver
	if approver == nil && opt.Can != nil {
		approver = CoreApprover{Client: opt.Can} // requisitions are approved by can-core's engine
	}
	store, err := OpenStore(cfg.DBPath, StoreOptions{Keyring: kr, HashKey: []byte(sec.HashKey), Config: cfg, Now: now, Syndicate: opt.Syndicators,
		Approver: approver, Resumes: res, Scanner: opt.Scanner, EventsOn: cfg.Webhook.URL != "" || opt.Can != nil})
	if err != nil {
		return nil, err
	}
	auth := opt.Auth
	if auth == nil {
		st := &StaticTokens{}
		st.Add(sec.AdminToken, Principal{Subject: bootstrapSubject, Kind: "static", Role: RoleAdmin})
		auth = Chain{st, TokenAuth{Store: store}}
		if opt.Can != nil {
			auth = append(auth.(Chain), &CanAuth{In: &modclient.Introspector{API: opt.Can}})
		}
	}
	app := &App{cfg: cfg, store: store, log: log, now: now}
	if store.events {
		app.dispatcher = &Dispatcher{store: store, URL: cfg.Webhook.URL, Secret: []byte(sec.WebhookSecret), Client: opt.Client, Log: log, Now: now, Can: opt.Can}
	}
	timeout := time.Duration(cfg.DBTimeoutSeconds) * time.Second
	a := &api{
		store: store, auth: auth, log: log, maxBody: cfg.MaxBodyBytes, trustProxy: cfg.TrustProxy, dbTimeout: timeout,
		limiter:  newRateLimiter(cfg.RateLimit.PerMinute, time.Minute, cfg.RateLimit.MaxTracked, now),
		penalty:  300 * time.Millisecond,
		failures: newRateLimiter(cfg.RateLimit.AuthFailuresPerMin, time.Minute, cfg.RateLimit.MaxTracked, now),
	}
	key := []byte(sec.FormSecret)
	if len(key) == 0 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	pub := &publicHandler{store: store, cfg: cfg, site: cfg.Site(), log: log, now: now, dbTimeout: timeout,
		signer:  forms.NewTokenSigner(key, time.Duration(cfg.Apply.TokenTTLMinutes)*time.Minute, now),
		limiter: newRateLimiter(cfg.RateLimit.ApplyPerHour, time.Hour, cfg.RateLimit.MaxTracked, now)}
	if cfg.Apply.CaptchaProvider != "" {
		if pub.captcha, err = forms.NewCaptchaVerifier(cfg.Apply.CaptchaProvider, sec.CaptchaSecret, opt.Client); err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if err := store.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unhealthy", "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	a.routes(mux)
	pub.routes(mux)
	if opt.Can != nil {
		// can-core's signed webhooks (approval decisions). Authenticated by the HMAC signature, not by a token.
		mux.Handle("POST /v1/can/events", &modclient.Receiver{Secret: sec.CanSecret, Handle: store.CanEventHandler(), Now: now})
	}
	var uiHandler http.Handler
	if cfg.UI.Enabled {
		app.ui = &UI{Store: store, Auth: auth, Log: log, Site: cfg.Site(), Secure: !cfg.UI.InsecureCookies, TrustProxy: cfg.TrustProxy, Now: now, Timeout: timeout,
			IdleTimeout: time.Duration(cfg.UI.IdleMinutes) * time.Minute, AbsoluteTimeout: time.Duration(cfg.UI.AbsoluteHours) * time.Hour,
			NoTokenLogin: !cfg.tokenLogin()}
		if opt.Can != nil {
			app.ui.Sessions = opt.Can
		}
		uiHandler = app.ui.Handler()
	}
	app.handler = withMiddleware(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			setSecurityHeaders(w)
		}
		// The recruiter web UI owns every path the API, the careers pages, the feeds and the
		// health check do not: it is served on the same listener when ui.enabled is set.
		if uiHandler != nil && !reservedPath(r.URL.Path) {
			uiHandler.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	return app, nil
}

// reservedPath reports whether a path belongs to the API or the public pages rather than the UI.
func reservedPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "/v1/"), p == "/careers", strings.HasPrefix(p, "/careers/"), strings.HasPrefix(p, "/feeds/"), p == "/healthz", p == "/robots.txt":
		return true
	}
	return false
}

// Handler serves the API and the public pages.
func (a *App) Handler() http.Handler { return a.handler }

// Store exposes the store (tests and tooling).
func (a *App) Store() *Store { return a.store }

// Close closes the database. It is safe to call repeatedly.
func (a *App) Close() error {
	a.closeOnce.Do(func() { a.closeErr = a.store.Close() })
	return a.closeErr
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second, // resume uploads
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// Serve runs until ctx is cancelled or the server fails, then shuts down gracefully.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	srv := newServer(a.handler)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	bg, stopBg := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.sweepLoop(bg) }()
	if a.dispatcher != nil {
		wg.Add(1)
		go func() { defer wg.Done(); a.dispatcher.Run(bg) }()
	}
	a.log.Info("can-jobs listening", "addr", ln.Addr().String(), "webhook", a.dispatcher != nil)

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

func (a *App) sweepLoop(ctx context.Context) {
	t := time.NewTicker(a.store.sweepInterval())
	defer t.Stop()
	for {
		rep, err := a.store.RunRetention(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			a.log.Error("retention sweep", "error", err)
		case rep != nil && (rep.CandidatesPurged+rep.JobsExpired+rep.JobsDeleted+rep.EEODeleted) > 0:
			a.log.Info("retention sweep", "candidates_purged", rep.CandidatesPurged, "jobs_expired", rep.JobsExpired, "jobs_deleted", rep.JobsDeleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

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

package finance

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

var errEOF = io.EOF

// Secrets are read from the environment, never from the config file.
type Secrets struct {
	AdminToken     string // bootstrap token with every scope (CAN_FINANCE_ADMIN_TOKEN, >= 32 characters)
	WebhookSecret  string // HMAC secret for outgoing event signatures (CAN_FINANCE_WEBHOOK_SECRET)
	LinkSecret     string // HMAC secret for public invoice links (CAN_FINANCE_LINK_SECRET, >= 32 characters)
	ProviderSecret string // shared secret of the fake payment and e-sign providers (CAN_FINANCE_PROVIDER_SECRET)
	SealKey        string // 64 hex characters sealing bank account numbers (CAN_FINANCE_SEAL_KEY); optional, bank accounts need it
}

// SecretsFromEnv loads and sanity-checks the secrets.
func SecretsFromEnv(getenv func(string) string) (Secrets, error) {
	s := Secrets{AdminToken: getenv("CAN_FINANCE_ADMIN_TOKEN"), WebhookSecret: getenv("CAN_FINANCE_WEBHOOK_SECRET"),
		LinkSecret: getenv("CAN_FINANCE_LINK_SECRET"), ProviderSecret: getenv("CAN_FINANCE_PROVIDER_SECRET"), SealKey: getenv("CAN_FINANCE_SEAL_KEY")}
	var errs []error
	if s.SealKey != "" {
		if _, err := NewSealer(s.SealKey); err != nil {
			errs = append(errs, errors.New("CAN_FINANCE_SEAL_KEY: "+err.Error()))
		}
	}
	if len(s.AdminToken) < 32 || distinctBytes(s.AdminToken) < 10 {
		errs = append(errs, errors.New("CAN_FINANCE_ADMIN_TOKEN is required, at least 32 characters with at least 10 different ones (for example `openssl rand -base64 32`)"))
	}
	if len(s.LinkSecret) < 32 || distinctBytes(s.LinkSecret) < 10 {
		errs = append(errs, errors.New("CAN_FINANCE_LINK_SECRET is required, at least 32 characters with at least 10 different ones: it signs the public invoice links"))
	}
	if s.WebhookSecret != "" && len(s.WebhookSecret) < 16 {
		errs = append(errs, errors.New("CAN_FINANCE_WEBHOOK_SECRET must be at least 16 characters when set"))
	}
	if s.ProviderSecret != "" && len(s.ProviderSecret) < 16 {
		errs = append(errs, errors.New("CAN_FINANCE_PROVIDER_SECRET must be at least 16 characters when set"))
	}
	return s, errors.Join(errs...)
}

// Options override collaborators; tests use them to inject fakes.
type Options struct {
	// Can is the can-core client of an installed module (CAN_API_URL): personal access tokens are
	// introspected through it and events are published through it instead of a webhook URL.
	Can *modclient.Client
	// CanSecret verifies the webhooks can-core delivers (CAN_WEBHOOK_SECRET).
	CanSecret string
	Auth      Authenticator // replaces the built-in token authenticators (introspection seam)
	Approver  Approver      // replaces the finance.approve rule (reporting-line approvals)
	Payments  PaymentProvider
	ESign     ESigner
	Mailer    Mailer // replaces can-core's email.send for signing links
	Logger    *slog.Logger
	Now       func() time.Time
	Client    *http.Client
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
		return nil, errors.New("webhook.url is set but CAN_FINANCE_WEBHOOK_SECRET is empty")
	}
	if len(sec.LinkSecret) < 32 {
		return nil, errors.New("CAN_FINANCE_LINK_SECRET must be at least 32 characters")
	}
	pay, esign := opt.Payments, opt.ESign
	if cfg.PaymentProvider == "fake" || cfg.ESignProvider == "fake" {
		if len(sec.ProviderSecret) < 16 {
			return nil, errors.New("payment_provider or esign_provider is fake but CAN_FINANCE_PROVIDER_SECRET is shorter than 16 characters")
		}
		log.Warn("fake payment/e-sign providers are enabled: for development only, nothing real is charged or signed")
	}
	if pay == nil && cfg.PaymentProvider == "fake" {
		pay = &FakePayments{Secret: []byte(sec.ProviderSecret), Now: now}
	}
	if esign == nil && cfg.ESignProvider == "fake" {
		esign = &FakeESign{Secret: []byte(sec.ProviderSecret), Now: now}
	}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	store.now = now
	if sec.SealKey != "" {
		sl, err := NewSealer(sec.SealKey)
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		store.sealer = sl
	}
	store.events = cfg.Webhook.URL != "" || opt.Can != nil
	store.base = cfg.BaseCurrency
	store.payInstr = cfg.PaymentInstructions
	store.sign.log, store.sign.baseURL, store.sign.company = log, cfg.SignBaseURL, cfg.Seller.Name
	if u, err := url.Parse(cfg.SignBaseURL); err == nil && store.sign.company == "" {
		store.sign.company = u.Host // a mail that names nobody looks like phishing
	}
	if cfg.SigningRecordIP != "" {
		store.sign.ipMode = cfg.SigningRecordIP
	}
	store.sign.mailer = opt.Mailer
	if store.sign.mailer == nil && opt.Can != nil {
		store.sign.mailer = opt.Can
	}
	if opt.Can != nil {
		store.core = opt.Can
		store.approver = &CoreApprover{store: store} // invoices are approved through can-core's engine
	}
	if opt.Approver != nil {
		store.approver = opt.Approver
	}

	auth := opt.Auth
	if auth == nil {
		st := &StaticTokens{}
		st.Add(sec.AdminToken, Principal{Subject: bootstrapSubject, Kind: "static", Permissions: []string{PermAdmin}})
		auth = Chain{st, TokenAuth{Store: store}}
		if opt.Can != nil {
			auth = append(auth.(Chain), &CanAuth{In: &modclient.Introspector{API: opt.Can}})
		}
	}
	app := &App{cfg: cfg, store: store, log: log, now: now}
	if store.events {
		app.dispatcher = &Dispatcher{store: store, URL: cfg.Webhook.URL, Secret: []byte(sec.WebhookSecret), Client: opt.Client, Log: log, Now: now, Can: opt.Can}
	}
	a := &api{
		store: store, auth: auth, log: log, maxBody: cfg.MaxBodyBytes, trustProxy: cfg.TrustProxy, now: now,
		cfg: cfg, linkSecret: []byte(sec.LinkSecret), payments: pay, esign: esign,
		seller:   Seller{Name: cfg.Seller.Name, Address: cfg.Seller.Address, TaxID: cfg.Seller.TaxID},
		limiter:  newRateLimiter(cfg.RateLimit.PerMinute, time.Minute, cfg.RateLimit.MaxTracked, now),
		penalty:  300 * time.Millisecond,
		failures: newRateLimiter(cfg.RateLimit.AuthFailuresPerMin, time.Minute, cfg.RateLimit.MaxTracked, now),
	}
	h := a.handler()
	if opt.Can != nil {
		// can-core's signed webhooks (approval decisions). Authenticated by the HMAC signature, not by a token.
		mux := http.NewServeMux()
		mux.Handle("POST /v1/can/events", &modclient.Receiver{Secret: opt.CanSecret, Handle: store.CanEventHandler(), Now: now})
		// The business overview (specs/053-business-overview).
		mux.Handle(modclient.SummaryPath, modclient.SummaryHandler(opt.Can, log, summaryBuilder(store, now)))
		mux.Handle("/", h)
		h = mux
	}
	if cfg.UI.Enabled {
		// The web UI owns the root of the listener; the API, the signed invoice link and the
		// health check keep their paths (specs/031-finance-ui).
		ui := &UI{Store: store, Auth: auth, Log: log, Secure: !cfg.UI.InsecureCookies, TrustProxy: cfg.TrustProxy, Now: now, Cfg: cfg, Seller: a.seller,
			LinkSecret: a.linkSecret, ESign: esign, NotificationsURL: cfg.UI.NotificationsURL,
			NoTokenLogin: !cfg.tokenLogin(),
			IdleTimeout:  time.Duration(cfg.UI.IdleMinutes) * time.Minute, AbsoluteTimeout: time.Duration(cfg.UI.AbsoluteHours) * time.Hour}
		if opt.Can != nil {
			ui.Sessions = opt.Can
		}
		top := http.NewServeMux()
		top.Handle("/v1/", h)
		top.Handle("GET /healthz", h)
		top.Handle("GET /public/invoices/{token}", h)
		top.Handle("/", ui.Handler())
		h = top
	}
	// The signing pages are served beside everything else: the link alone authenticates them.
	signMux := http.NewServeMux()
	signMux.Handle(signPathPrefix, newSignSite(a))
	signMux.Handle("/", h)
	app.handler = withMiddleware(log, signMux)
	return app, nil
}

// Handler serves the API.
func (a *App) Handler() http.Handler { return a.handler }

// Store exposes the store (tests and tooling).
func (a *App) Store() *Store { return a.store }

// Purge applies retention.
func (a *App) Purge(ctx context.Context) (PurgeReport, error) {
	return a.store.Purge(ctx, a.cfg.RetentionYears, a.cfg.AuditRetentionDays)
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
	a.log.Info("can-finance listening", "addr", ln.Addr().String(), "webhook", a.dispatcher != nil)

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

// purgeLoop runs the housekeeping: retention, due recurring invoices and contract reminders.
func (a *App) purgeLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if rep, err := a.Purge(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("retention purge", "error", err)
		} else if rep != (PurgeReport{}) {
			a.log.Info("retention purge", "report", rep)
		}
		today := dateOf(a.now())
		if ids, err := a.store.RunRecurring(ctx, today); err != nil && ctx.Err() == nil {
			a.log.Error("recurring invoices", "error", err)
		} else if len(ids) > 0 {
			a.log.Info("recurring invoices drafted", "count", len(ids))
		}
		if n, err := a.store.SweepContracts(ctx, today); err != nil && ctx.Err() == nil {
			a.log.Error("contract sweep", "error", err)
		} else if n > 0 {
			a.log.Info("contract renewal reminders", "count", n)
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

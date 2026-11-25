package forms

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Secrets are read from the environment, never from the config file.
type Secrets struct {
	TokenKey      []byte // HMAC key for timing tokens (CAN_FORMS_TOKEN_KEY, >= 32 bytes)
	APIToken      string // bearer token with leads.read + leads.write (CAN_FORMS_API_TOKEN)
	APIReadToken  string // optional read-only token (CAN_FORMS_API_READ_TOKEN)
	WebhookSecret string // HMAC secret for webhook signatures (CAN_FORMS_WEBHOOK_SECRET)
	CaptchaSecret string // Turnstile/hCaptcha secret (CAN_FORMS_CAPTCHA_SECRET)
	EmailToken    string // bearer token for email.send (CAN_FORMS_EMAIL_TOKEN)
}

// SecretsFromEnv loads and sanity-checks the secrets.
func SecretsFromEnv(getenv func(string) string) (Secrets, error) {
	s := Secrets{
		TokenKey:      []byte(getenv("CAN_FORMS_TOKEN_KEY")),
		APIToken:      getenv("CAN_FORMS_API_TOKEN"),
		APIReadToken:  getenv("CAN_FORMS_API_READ_TOKEN"),
		WebhookSecret: getenv("CAN_FORMS_WEBHOOK_SECRET"),
		CaptchaSecret: getenv("CAN_FORMS_CAPTCHA_SECRET"),
		EmailToken:    getenv("CAN_FORMS_EMAIL_TOKEN"),
	}
	var errs []error
	if len(s.TokenKey) < 32 {
		errs = append(errs, errors.New("CAN_FORMS_TOKEN_KEY is required and must be at least 32 bytes"))
	}
	if len(s.APIToken) < 16 {
		errs = append(errs, errors.New("CAN_FORMS_API_TOKEN is required and must be at least 16 characters"))
	}
	if s.APIReadToken != "" && len(s.APIReadToken) < 16 {
		errs = append(errs, errors.New("CAN_FORMS_API_READ_TOKEN must be at least 16 characters when set"))
	}
	return s, errors.Join(errs...)
}

// Options override collaborators; tests use them to inject fakes.
type Options struct {
	Mailer  Mailer
	Captcha CaptchaVerifier
	Auth    Authenticator
	Logger  *slog.Logger
	Now     func() time.Time
	Client  *http.Client
}

// App wires the store, spam guards, notifier and the two HTTP surfaces.
type App struct {
	cfg        *Config
	store      *Store
	dispatcher *Dispatcher
	public     http.Handler
	api        http.Handler
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

	captcha := opt.Captcha
	if captcha == nil {
		v, err := NewCaptchaVerifier(cfg.Captcha.Provider, sec.CaptchaSecret, opt.Client)
		if err != nil {
			return nil, fmt.Errorf("%w (set CAN_FORMS_CAPTCHA_SECRET)", err)
		}
		captcha = v
	}

	mailer := opt.Mailer
	if mailer == nil && cfg.Email.URL != "" {
		mailer = &HTTPMailer{URL: cfg.Email.URL, Token: sec.EmailToken, From: cfg.Email.From, Client: opt.Client}
	}
	if mailer == nil {
		for _, f := range cfg.Forms {
			if len(f.NotifyEmail) > 0 {
				log.Warn("notify_email is set but email.url is not; no notification emails will be sent", "form", f.ID)
			}
		}
	}

	var hook *WebhookSender
	if cfg.Webhook.URL != "" {
		if sec.WebhookSecret == "" {
			return nil, errors.New("webhook.url is set but CAN_FORMS_WEBHOOK_SECRET is empty")
		}
		hook = &WebhookSender{URL: cfg.Webhook.URL, Secret: []byte(sec.WebhookSecret), MaxAttempts: cfg.Webhook.MaxAttempts, Backoff: time.Second, Client: opt.Client, Now: now}
		if hook.Client == nil {
			hook.Client = &http.Client{
				Timeout:       time.Duration(cfg.Webhook.TimeoutSeconds) * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}
		}
	}

	auth := opt.Auth
	if auth == nil {
		st := &StaticTokens{}
		st.Add(sec.APIToken, Principal{Subject: "static-admin", Permissions: []string{PermLeadsRead, PermLeadsWrite}})
		st.Add(sec.APIReadToken, Principal{Subject: "static-reader", Permissions: []string{PermLeadsRead}})
		auth = st
	}

	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	store.now = now

	d := NewDispatcher(store, mailer, hook, log)
	d.now = now
	d.Start(2)

	pub := &publicHandler{
		cfg:      cfg,
		store:    store,
		signer:   NewTokenSigner(sec.TokenKey, 24*time.Hour, now),
		limiter:  NewRateLimiter(cfg.RateLimit.PerMinute, cfg.RateLimit.MaxTrackedIPs, now),
		captcha:  captcha,
		notifier: d,
		log:      log,
		now:      now,
	}
	api := &leadsAPI{store: store, auth: auth, log: log}
	return &App{
		cfg: cfg, store: store, dispatcher: d, log: log, now: now,
		public: withMiddleware(log, "public", pub),
		api:    withMiddleware(log, "api", api.handler()),
	}, nil
}

// PublicHandler serves the form endpoints.
func (a *App) PublicHandler() http.Handler { return a.public }

// APIHandler serves the leads capability.
func (a *App) APIHandler() http.Handler { return a.api }

// Purge deletes leads past their retention and returns how many.
func (a *App) Purge(ctx context.Context) (int64, error) {
	return a.store.PurgeExpired(ctx, a.now())
}

// Close drains notifications and closes the database. It is safe to call repeatedly.
func (a *App) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		err := a.dispatcher.Shutdown(ctx)
		a.closeErr = errors.Join(err, a.store.Close())
	})
	return a.closeErr
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// Serve runs both servers until ctx is cancelled or one fails, then shuts down gracefully:
// stop accepting, let in-flight requests finish, flush pending notifications, close the DB.
func (a *App) Serve(ctx context.Context, pub, api net.Listener) error {
	pubSrv, apiSrv := newServer(a.public), newServer(a.api)
	errc := make(chan error, 2)
	for _, s := range []struct {
		srv *http.Server
		ln  net.Listener
	}{{pubSrv, pub}, {apiSrv, api}} {
		go func() { errc <- s.srv.Serve(s.ln) }()
	}
	purgeDone := make(chan struct{})
	go func() {
		defer close(purgeDone)
		a.purgeLoop(ctx)
	}()
	a.log.Info("can-forms listening", "public", pub.Addr().String(), "api", api.Addr().String(), "forms", len(a.cfg.Forms))

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
	err := errors.Join(runErr, pubSrv.Shutdown(shutCtx), apiSrv.Shutdown(shutCtx))
	<-purgeDone
	err = errors.Join(err, a.Close(shutCtx))
	if err != nil {
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

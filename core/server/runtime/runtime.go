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
	"maps"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/obs"
	obslog "github.com/dodwmd/companyinacan/core/obs/log"
	"github.com/dodwmd/companyinacan/core/obs/obssql"
	"github.com/dodwmd/companyinacan/core/server/adminapi"
	"github.com/dodwmd/companyinacan/core/server/adminui"
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/identity"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/restoreops"
	"github.com/dodwmd/companyinacan/core/server/siteedit"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
	"github.com/dodwmd/companyinacan/core/server/users"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

// Config is can-core's configuration (all from CAN_* environment variables).
type Config struct {
	AdminAddr          string            // CAN_ADMIN_ADDR, default :8080
	ModuleAddr         string            // CAN_MODULE_ADDR, default :8090
	DBPath             string            // CAN_DB_PATH
	SecureCookies      bool              // CAN_COOKIE_SECURE, default true
	TrustProxy         bool              // CAN_TRUST_PROXY: take the client IP from X-Forwarded-For (set it behind Traefik; see deploy notes)
	TokenReview        string            // CAN_TOKEN_REVIEW: kubernetes (default) or fake (development only)
	PlatformURL        string            // CAN_PLATFORM_URL: module-API URL as seen from module namespaces
	KubeAPI            string            // CAN_KUBE_API
	SAToken            string            // CAN_SA_TOKEN_FILE
	SACA               string            // CAN_SA_CA_FILE
	MemLimitMB         int               // CAN_MEMLIMIT_MB: soft Go memory limit (idle RAM target, see plan.md)
	LogLevel           string            // CAN_LOG_LEVEL
	CompanyName        string            // CAN_COMPANY_NAME: shown on the admin dashboard
	CompanyDomain      string            // CAN_COMPANY_DOMAIN: shown on the admin dashboard
	ModuleUIURLs       map[string]string // CAN_MODULE_UI_URLS: id=base URL of a module web UI, comma-separated (E11c); beats the manifest
	ModuleUIURLsRaw    string            // the unparsed value, for start-up warnings
	PushEndpointAllow  []string          // CAN_PUSH_ENDPOINT_ALLOW: comma-separated host suffixes Web Push endpoints must match; empty = any public https host
	VAPIDKeyFile       string            // CAN_VAPID_KEY_FILE: Web Push application server key from a mounted Secret; unset = generated on first use and kept in the state database
	DirectoryTokenFile string            // CAN_DIRECTORY_TOKEN_FILE: bearer credential for the `directory` capability (reporting-line approval chains); unset = manager steps use their fallback
	LeadsTokenFile     string            // CAN_LEADS_TOKEN_FILE: bearer credential for the `leads` capability (admin UI leads view); unset = view shows "not available"
	Installer          string            // CAN_INSTALLER: kubernetes (deploy modules) or none (default; no deploys)
	CompanyFile        string            // CAN_COMPANY_FILE: the applied company.yaml (mounted ConfigMap)
	ModulesDir         string            // CAN_MODULES_DIR: module directories baked into the image
	NodeIP             string            // CAN_NODE_IP: the node address (downward API status.hostIP)
	ReconcileInterval  time.Duration     // CAN_RECONCILE_INTERVAL_SECONDS, default 30
	PostgresAdminURL   string            // CAN_POSTGRES_ADMIN_URL: admin connection used to create module databases
	PostgresHost       string            // CAN_POSTGRES_HOST: Postgres Service as seen by modules
	ShutdownTimeout    time.Duration     // CAN_SHUTDOWN_TIMEOUT_SECONDS
	PublicIPs          []string          // CAN_PUBLIC_IPS: the server's public addresses, listed in the setup wizard's DNS step
	SSOSecretDir       string            // CAN_SSO_SECRET_DIR: directory with the SSO client secret as <name>/<key> files, a mounted Secret (default /etc/can/sso)
	DevStack           bool              // CAN_DEV_STACK: the Docker Compose development mode (devstack.go); refused by release builds
	DevModuleTokenDir  string            // CAN_DEV_MODULE_TOKEN_DIR: <module id>/token files, for CAN_DEV_STACK only
	DevWebhookSecret   string            // CAN_DEV_WEBHOOK_SECRET: signs events to every module, for CAN_DEV_STACK only

	// Backup dumps (epic E08b, dumps.go): CAN_DUMPS_DIR, CAN_DUMP_AT (UTC), CAN_DUMP_START_DELAY_SECONDS,
	// CAN_DUMP_REQUEST_FILE.
	DumpsDir             string
	DumpHour, DumpMinute int
	DumpStartDelay       time.Duration
	DumpRequestFile      string

	// Restore mode (specs/042-restore): CAN_RESTORE names a restore to latch, CAN_RESTORE_OVERWRITE
	// the one that may replace an existing state database.
	RestoreID, RestoreOverwrite string

	Version, Commit string // build identity for can_build_info
	// Env reads settings Run needs after FromEnv (CAN_METRICS_*, CAN_RAM_DECLARED_*); nil means os.Getenv.
	Env func(string) string

	// OnListen is called once both listeners are bound (tests use it to learn ports).
	OnListen func(admin, module net.Addr)
	// Reviewer overrides the token reviewer (tests).
	Reviewer tokenreview.Reviewer
	// ModuleInstaller overrides the installer (tests).
	ModuleInstaller reconcile.ModuleInstaller
	// SiteDir (CAN_SITE_DIR) is a built site a can-static serves, which the website editor replaces
	// where there is no cluster (Docker Compose). SiteBaseURL (CAN_SITE_BASE_URL) is its public origin.
	SiteDir, SiteBaseURL string
	// SitePublisher overrides how the website is published (tests).
	SitePublisher siteedit.Publisher
	// CompanyStore overrides where the admin UI stores company.yaml (tests); by default the ConfigMap
	// can-system/company with CAN_INSTALLER=kubernetes, otherwise the file CAN_COMPANY_FILE.
	CompanyStore modmgr.CompanyStore
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
		CompanyName: getenv("CAN_COMPANY_NAME"), CompanyDomain: getenv("CAN_COMPANY_DOMAIN"), LeadsTokenFile: getenv("CAN_LEADS_TOKEN_FILE"), DirectoryTokenFile: getenv("CAN_DIRECTORY_TOKEN_FILE"), VAPIDKeyFile: getenv("CAN_VAPID_KEY_FILE"), PushEndpointAllow: splitList(getenv("CAN_PUSH_ENDPOINT_ALLOW")), ModuleUIURLs: parseModuleUIURLs(getenv("CAN_MODULE_UI_URLS")), ModuleUIURLsRaw: getenv("CAN_MODULE_UI_URLS"),
		Installer: str("CAN_INSTALLER", "none"), CompanyFile: str("CAN_COMPANY_FILE", "/etc/can/company/company.yaml"), ModulesDir: str("CAN_MODULES_DIR", "/modules"),
		NodeIP: getenv("CAN_NODE_IP"), PostgresAdminURL: getenv("CAN_POSTGRES_ADMIN_URL"), PostgresHost: str("CAN_POSTGRES_HOST", "postgres.can-data.svc.cluster.local"),
		ReconcileInterval: 30 * time.Second, SSOSecretDir: str("CAN_SSO_SECRET_DIR", "/etc/can/sso"),
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
	if v := getenv("CAN_RECONCILE_INTERVAL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("CAN_RECONCILE_INTERVAL_SECONDS: %q is not a positive integer", v)
		}
		c.ReconcileInterval = time.Duration(n) * time.Second
	}
	if c.Installer != "kubernetes" && c.Installer != "none" {
		return c, fmt.Errorf("CAN_INSTALLER: %q must be kubernetes or none", c.Installer)
	}
	if c.TokenReview != "kubernetes" && c.TokenReview != "fake" {
		return c, fmt.Errorf("CAN_TOKEN_REVIEW: %q must be kubernetes or fake", c.TokenReview)
	}
	for _, ip := range strings.Split(getenv("CAN_PUBLIC_IPS"), ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			if net.ParseIP(ip) == nil {
				return c, fmt.Errorf("CAN_PUBLIC_IPS: %q is not an IP address", ip)
			}
			c.PublicIPs = append(c.PublicIPs, ip)
		}
	}
	if c.DevStack, err = boolean(envDevStack, false); err != nil {
		return c, err
	}
	c.DevModuleTokenDir, c.DevWebhookSecret, c.Env = getenv(envDevTokenDir), getenv(envDevHook), getenv
	if err := c.checkDevStack(); err != nil {
		return c, err
	}
	if c.AdminAddr == c.ModuleAddr {
		return c, errors.New("CAN_ADMIN_ADDR and CAN_MODULE_ADDR must differ: the admin and module APIs are separate listeners")
	}
	if err := c.dumpEnv(getenv); err != nil {
		return c, err
	}
	c.RestoreID, c.RestoreOverwrite = getenv("CAN_RESTORE"), getenv("CAN_RESTORE_OVERWRITE")
	c.SiteDir, c.SiteBaseURL = getenv("CAN_SITE_DIR"), getenv("CAN_SITE_BASE_URL")
	return c, nil
}

// Logger builds the structured JSON logger for the configured level.
// Secret-bearing keys and values are redacted and records carry request_id (core/obs/log).
func (c Config) Logger(w *os.File) *slog.Logger {
	return obslog.New(w, obslog.Options{Service: "can-core", Version: c.Version, Level: c.LogLevel, Format: c.env("CAN_LOG_FORMAT")})
}

func (c Config) env(k string) string {
	if c.Env != nil {
		return c.Env(k)
	}
	return os.Getenv(k)
}

func (c Config) reviewer(log *slog.Logger) (tokenreview.Reviewer, error) {
	if c.Reviewer != nil {
		return c.Reviewer, nil
	}
	if c.DevStack {
		return devReviewer(c.DevModuleTokenDir, log)
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
	return tokenreview.NewCached(&tokenreview.HTTP{APIServer: c.KubeAPI, Client: client, BearerToken: tokenreview.FileToken(c.SAToken)}, tokenreview.CacheOptions{}), nil
}

// Run starts can-core and blocks until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	if err := cfg.checkDevStack(); err != nil { // again: a Config need not come from FromEnv
		return err
	}
	if cfg.MemLimitMB > 0 {
		debug.SetMemoryLimit(int64(cfg.MemLimitMB) << 20)
	} else if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(DefaultMemLimitMB << 20)
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}
	if !cfg.TrustProxy {
		log.Warn("CAN_TRUST_PROXY is off: sign-in has no lockout or rate limit, only delays, so someone who knows a password can keep guessing authentication codes. Behind Traefik set CAN_TRUST_PROXY=true (specs/005-can-core-mvp/deployment.md)")
	}
	// Restore mode imports the state dump before the database is opened.
	restoring, err := restoreops.Boot(ctx, cfg.DBPath, cfg.DumpsDir, cfg.RestoreID, cfg.RestoreID != "" && cfg.RestoreOverwrite == cfg.RestoreID)
	if err != nil {
		return fmt.Errorf("restore mode: %w", err)
	}
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer func() { _ = db.Close() }()
	// Observability (epic E22): process and footprint metrics, the optional token-protected metrics
	// listener (never the admin or module port), readiness checks and statement timings.
	store.SetObserver(obs.NewDBObserver(obs.Default))
	defer store.SetObserver(nil)
	if err := obs.Setup(ctx, cfg.env, obs.BuildInfo{Version: cfg.Version, Commit: cfg.Commit}, log); err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	obssql.WatchDefault(db.DB)
	rm := newReconcileMetrics(obs.Default, obs.DefaultHealth)
	pm := newPlatformMetrics(obs.Default)
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

	// Shared platform services (epic E11a).
	rb := rbac.New(db, al, rbac.Options{})
	// email.send (epic E03d): also the e-mail channel of notifications.
	mail, err := cfg.emailService(db, al, log)
	if err != nil {
		return err
	}
	if err := cfg.devModules(ctx, db, rev, log); err != nil {
		return err
	}
	ntf := notify.New(db, notify.Options{Bus: bus, Roles: rb, Log: log, Mailer: email.NotifyMailer{S: mail}, Webhook: notify.HTTPWebhook{}, VAPIDKeyFile: cfg.VAPIDKeyFile, PushEndpointAllow: cfg.PushEndpointAllow, PushContact: pushContact(cfg.CompanyDomain)})
	bus.SubscribeAll(ntf.HandleEvent)
	lines := &approvals.DirectoryReportingLine{Resolver: reg, Token: adminui.FileToken(cfg.DirectoryTokenFile)}
	eng := approvals.New(db, al, approvals.Deps{Roles: rb, Perms: rb, Lines: lines, Notifier: ntf, Bus: bus, Log: log})

	pats := pat.New(db, al)
	if d, err := strconv.Atoi(os.Getenv("CAN_PAT_MAX_TTL_DAYS")); err == nil && d >= 1 && d <= 365 {
		pats.MaxTTL = time.Duration(d) * 24 * time.Hour // default 90 days
	}
	pats.Holds = func(ctx context.Context, subject, perm string) (bool, error) { return rb.Check(ctx, subject, perm, "") }

	// Single sign-on (epic E11b): configured by the sso: block of company.yaml; absent means off.
	ssoSvc := &sso.Service{DB: db, RBAC: rb, Audit: al, Log: log, Secure: cfg.SecureCookies, HardLock: cfg.TrustProxy, Source: sso.FileSource(cfg.CompanyFile), Secrets: cfg.ssoSecrets(log)}
	adminSrv := &adminapi.Server{
		RBAC: rb, Approvals: eng, Notify: ntf, PATs: pats, SSO: ssoSvc,
		Auth: auth.NewService(db, al, auth.Options{Log: log, Policy: loginPolicy(cfg), LocalPolicy: ssoSvc.AllowLocalLogin}), DB: db, Audit: al, Registry: reg,
		Log: log, Secure: cfg.SecureCookies, TrustProxy: cfg.TrustProxy,
	}
	ssoSvc.Auth = adminSrv.Auth
	// Single sign-on into module UIs (E11c): launch tickets, minted by the admin UI and API,
	// redeemed over the module API.
	tickets := &ticket.Service{DB: db, Audit: al, Holds: pats.Holds,
		Launcher: ticket.Launcher{Domain: cfg.CompanyDomain, Overrides: cfg.ModuleUIURLs, AllowHTTP: !cfg.SecureCookies}}
	adminSrv.Tickets = tickets
	uiWarnings := moduleUIWarnings(cfg.ModuleUIURLsRaw, cfg.CompanyDomain, !cfg.SecureCookies)
	if installed, err := db.ListModules(ctx); err != nil {
		log.Warn("apps: cannot check CAN_MODULE_UI_URLS against the installed modules", "error", err)
	} else {
		uiWarnings = append(uiWarnings, unknownModuleUIWarnings(cfg.ModuleUIURLs, installed)...)
	}
	for _, w := range uiWarnings {
		log.Warn("apps: " + w)
	}
	leads := &adminui.HTTPLeads{Resolver: reg, Token: cfg.leadsToken(reg, log)}
	// Website leads to the modules company.yaml names (specs/069-lead-adapters).
	var companyStore atomic.Pointer[modmgr.CompanyStore]
	adapters := cfg.newLeadAdapter(db, al, leads, &companyStore, log)
	disp.Adapter, bus.Extra = adapters.Deliver, adapters.Targets
	uiSrv := &adminui.Server{
		RBAC: rb, Approvals: eng, Notify: ntf, PATs: pats, SSO: ssoSvc, Tickets: tickets,
		Auth: adminSrv.Auth, DB: db, Audit: al, Registry: reg,
		Leads:   leads,
		Company: adminui.Company{Name: cfg.CompanyName, Domain: cfg.CompanyDomain},
		Log:     log, Secure: cfg.SecureCookies, TrustProxy: cfg.TrustProxy,
	}
	idSvc := &identity.Service{DB: db, Audit: al, Log: log, Providers: func(ctx context.Context, name string) ([]string, error) {
		res, err := reg.Resolve(ctx, name)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, p := range res.Providers {
			ids = append(ids, p.ModuleID)
		}
		return ids, nil
	}}
	adminSrv.Identity = idSvc
	if !restoring { // it acts on the restored data, which may still hold what is to be erased
		if err := idSvc.Reconcile(ctx); err != nil {
			log.Warn("identity: reconciling offboarding facts failed", "error", err)
		}
	}
	bus.Subscribe("person.offboarded.v1", idSvc.HandleOffboarded)
	moduleSrv := &moduleapi.Server{Auth: modauth.New(rev, db), Registry: reg, Bus: bus, Log: log, RBAC: rb, Approvals: eng, Notify: ntf, PATs: pats, Identity: idSvc, Tickets: tickets, Email: mail}
	uiSrv.Email = mail
	// People's access (E11e): one service for the admin UI and API. Set-up links go by e-mail when a
	// relay is configured, to the address of the admin UI taken from configuration.
	people := &users.Service{DB: db, Audit: al, Auth: adminSrv.Auth, RBAC: rb, PATs: pats, SSO: ssoSvc, Identity: idSvc, Mail: users.EmailSender{S: mail}, Log: log}
	if cfg.CompanyDomain != "" {
		people.BaseURL = "https://admin." + cfg.CompanyDomain
	}
	uiSrv.Users, adminSrv.Users = people, people

	var dumper *dumps.Dumper
	if cfg.DumpsDir != "" {
		if dumper, err = cfg.newDumper(db, log); err != nil {
			return fmt.Errorf("backup dumps: %w", err)
		}
		moduleSrv.Dumps = dumper
		dumper.RestoreMode, moduleSrv.Restoring = restoring, restoring
	}

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
	}{{mk(uiSrv.Handler(adminSrv.Handler())), adminLn}, {mk(moduleSrv.Handler()), moduleLn}}

	if restoring {
		// No background worker is started: nothing is sent, dumped or reconciled.
		servers[0].srv.Handler = restoringHandler(servers[0].srv.Handler, adminAllowed)
		servers[1].srv.Handler = restoringHandler(servers[1].srv.Handler, moduleAllowed)
		return serveRestoring(ctx, cfg.ShutdownTimeout, log, servers[0].srv, servers[0].ln, servers[1].srv, servers[1].ln)
	}

	workCtx, stopWork := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); mail.Run(workCtx) }()
	wg.Add(1)
	go func() { defer wg.Done(); housekeeping(workCtx, db, log) }()
	wg.Add(1)
	go func() { defer wg.Done(); pm.run(workCtx, db, al, log) }()
	inst, err := cfg.newInstaller(log)
	if err != nil {
		stopWork()
		return fmt.Errorf("installer: %w", err)
	}
	if inst != nil {
		if cfg.Installer == "kubernetes" && cfg.NodeIP == "" && cfg.ModuleInstaller == nil {
			stopWork()
			return errors.New("CAN_NODE_IP is required with CAN_INSTALLER=kubernetes")
		}
		rec := newReconciler(db, reg, al, inst)
		rec.FixedWebhookSecret = cfg.DevWebhookSecret // empty outside the development stack (checkDevStack)
		if cfg.DevStack {
			rec.Keep, rec.RequireApproval = isDevModule, false
		}
		svc, cs, run, merr := cfg.moduleManagement(db, al, rec, log)
		if merr != nil {
			stopWork()
			return fmt.Errorf("module management: %w", merr)
		}
		uiSrv.Mod, adminSrv.Mod = svc, svc
		if uiSrv.Site, merr = cfg.siteEditor(db, al, cs); merr != nil {
			stopWork()
			return fmt.Errorf("website editor: %w", merr)
		}
		companyStore.Store(&cs)
		svc.OnPurged = func(id string) error { return db.DeleteLeadLinks(workCtx, id) }
		if dumper != nil {
			svc.OnPurged = func(id string) error { return errors.Join(dumper.Forget(id), db.DeleteLeadLinks(workCtx, id)) }
			svc.LastDump = func() (backup.DumpRun, error) { return backup.ReadDumpRun(cfg.DumpsDir) }
		}
		if rerr := svc.Recover(workCtx); rerr != nil {
			log.Error("could not close interrupted applies", "error", rerr)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			loopEvery(workCtx, cfg, log, func(ctx context.Context) error {
				err := reconcileStored(ctx, cfg, cs, run, svc, log)
				rm.observe(err, time.Now())
				return err
			})
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); disp.Run(workCtx, bus.Wake(), 30*time.Second) }()
	if dumper != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dumper.Loop(workCtx, cfg.DumpHour, cfg.DumpMinute, cfg.DumpStartDelay, cfg.DumpRequestFile)
		}()
	}
	col, err := cfg.erasureCollector(db, log)
	if err != nil {
		stopWork()
		return fmt.Errorf("erasure collection: %w", err)
	}
	if col != nil {
		wg.Add(1)
		go func() { defer wg.Done(); col.Loop(workCtx, erasureInterval(cfg)) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); platformTimers(workCtx, eng, ntf, log) }()

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

// moduleManagement builds the module-management service (admin UI catalogue, changes, wizard,
// status) and the company store and runner the periodic reconcile shares with it.
func (c Config) moduleManagement(db *store.DB, al *audit.Log, rec *reconcile.Reconciler, log *slog.Logger) (*modmgr.Service, modmgr.CompanyStore, *modmgr.Runner, error) {
	run := modmgr.NewRunner(rec)
	svc := &modmgr.Service{DB: db, Audit: al, ModulesDir: c.ModulesDir, Runner: run, Disk: modmgr.DirDisk{Name: "can-core data", Path: dirOf(c.DBPath)}}
	if len(c.PublicIPs) > 0 {
		var v4, v6 []string
		for _, ip := range c.PublicIPs {
			if p := net.ParseIP(ip); p != nil && p.To4() != nil {
				v4 = append(v4, ip)
			} else {
				v6 = append(v6, ip)
			}
		}
		svc.Net = modmgr.StaticNet{V4: v4, V6: v6}
	}
	var cs modmgr.CompanyStore = &modmgr.FileStore{Path: c.CompanyFile}
	if c.CompanyStore != nil {
		cs = c.CompanyStore
	} else if c.Installer == "kubernetes" && c.ModuleInstaller == nil {
		api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
		if err != nil {
			return nil, nil, nil, err
		}
		cs = &modmgr.KubeStore{API: api}
		svc.Secrets = modmgr.KubeSecrets{API: api}
	}
	svc.Company = cs
	if svc.Secrets == nil {
		log.Warn("module management: no Secret writer in this mode; the setup wizard cannot store email relay credentials")
	}
	return svc, cs, run, nil
}

// siteEditor builds the website editor of the admin UI over the company store, nil when this
// installation has no website it could publish to.
func (c Config) siteEditor(db *store.DB, al *audit.Log, cs modmgr.CompanyStore) (*siteedit.Service, error) {
	p := c.SitePublisher
	switch {
	case p != nil:
	case c.Installer == "kubernetes" && c.ModuleInstaller == nil:
		api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
		if err != nil {
			return nil, err
		}
		p = siteedit.KubePublisher{API: api}
	case c.SiteDir != "":
		p = siteedit.DirPublisher{Dir: c.SiteDir}
	default:
		return nil, nil
	}
	base := c.SiteBaseURL
	if base == "" && c.CompanyDomain != "" {
		base = "https://" + c.CompanyDomain
	}
	return &siteedit.Service{DB: db, Audit: al, Company: cs, Publisher: p, ModulesDir: c.ModulesDir, BaseURL: base}, nil
}

// ssoSecrets reads the SSO client secret from a Secret mounted into the pod at CAN_SSO_SECRET_DIR
// (deploy/can-core mounts the optional Secret sso-client). can-core's Kubernetes permissions
// deliberately do not include reading Secrets of can-system, so none are added for this.
func (c Config) ssoSecrets(_ *slog.Logger) sso.SecretReader {
	return sso.DirSecrets{Dir: c.SSOSecretDir}
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
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
			if _, err := db.PruneEventDedupe(ctx, time.Now().Add(-7*24*time.Hour)); err != nil {
				log.Error("publish de-duplication pruning failed", "error", err)
			}
			if _, err := db.PruneAPITokens(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
				log.Error("token pruning failed", "error", err)
			}
			if err := db.PurgeLoginTickets(ctx, time.Now().Add(-time.Hour)); err != nil {
				log.Error("launch ticket pruning failed", "error", err)
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

// pushContact is the VAPID `sub` claim: an https URL on the company's own domain when it is known
// (so a push service operator can reach whoever runs this server), otherwise the project's URL.
func pushContact(domain string) string {
	if domain != "" && !strings.ContainsAny(domain, " /\\:@?#") {
		return "https://" + domain
	}
	return ""
}

// parseModuleUIURLs reads "id=url,id=url". Entries that are not a lowercase module id and a
// non-empty value are dropped; the launcher validates each URL itself.
func parseModuleUIURLs(v string) map[string]string {
	out := map[string]string{}
	for _, p := range splitList(v) {
		id, raw, ok := strings.Cut(p, "=")
		id, raw = strings.TrimSpace(id), strings.TrimSpace(raw)
		if !ok || raw == "" || id == "" || id != strings.ToLower(id) || strings.ContainsAny(id, "/ ") || id[0] < 'a' || id[0] > 'z' {
			continue
		}
		out[id] = raw
	}
	return out
}

// unknownModuleUIWarnings names the CAN_MODULE_UI_URLS entries for module ids this server has no
// module record of: a typo there would otherwise only show as an app missing from Apps.
func unknownModuleUIWarnings(overrides map[string]string, installed []store.Module) []string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(overrides)) {
		if !slices.ContainsFunc(installed, func(m store.Module) bool { return m.ID == id }) {
			out = append(out, "CAN_MODULE_UI_URLS names "+strconv.Quote(id)+", which is not an installed module (check the spelling; it takes effect if that module is installed)")
		}
	}
	return out
}

// moduleUIWarnings says what in the configuration will keep module UIs out of the Apps launcher
// (E11c), so an operator hears about it at start-up and not from an empty page. Configured values
// are never repeated: a URL can carry a password.
func moduleUIWarnings(raw, domain string, allowHTTP bool) []string {
	var out []string
	switch {
	case domain == "":
		out = append(out, "CAN_COMPANY_DOMAIN is not set: no module web UI can be offered in Apps")
	case !strings.Contains(domain, "."):
		out = append(out, "CAN_COMPANY_DOMAIN is a single label: a launch cookie cannot be shared between hosts, so Apps works only when the admin UI and a module are on one host")
	}
	l := ticket.Launcher{Domain: domain, AllowHTTP: allowHTTP}
	for i, entry := range splitList(raw) {
		parsed := parseModuleUIURLs(entry)
		if len(parsed) == 0 {
			out = append(out, "CAN_MODULE_UI_URLS entry "+strconv.Itoa(i+1)+" is not <module id>=<URL> and is ignored")
			continue
		}
		for id, u := range parsed {
			if domain != "" && !l.Acceptable(u) {
				out = append(out, "CAN_MODULE_UI_URLS: the address for "+id+" is not acceptable (https on the company domain, not the admin host, no credentials, query or fragment); the module is not offered in Apps")
			}
		}
	}
	return out
}

// splitList splits a comma-separated setting, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

package files

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/obs"
	obslog "github.com/dodwmd/companyinacan/core/obs/log"
)

// Version is set at build time with -ldflags "-X .../core/files.Version=...".
var Version = "dev"

const defaultConfigPath = "/etc/can-files/files.yaml"

// Secrets come from the environment, never from the config file.
type Secrets struct {
	APIToken   string // CAN_FILES_API_TOKEN: the service token (can-core, other modules, operators)
	LinkSecret string // CAN_FILES_LINK_SECRET: signs share links
	// RestoreToken (CAN_FILES_RESTORE_TOKEN, optional) is the restore-only credential: the purge
	// hook with it ignores legal holds and retention dates.
	RestoreToken string
	S3KeyID      string // CAN_FILES_S3_ACCESS_KEY_ID, only with backend.type s3
	S3Secret     string // CAN_FILES_S3_SECRET_ACCESS_KEY
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

// SecretsFromEnv loads and sanity-checks the secrets for a configuration.
func SecretsFromEnv(getenv func(string) string, cfg *Config) (Secrets, error) {
	s := Secrets{APIToken: getenv("CAN_FILES_API_TOKEN"), LinkSecret: getenv("CAN_FILES_LINK_SECRET"), RestoreToken: getenv("CAN_FILES_RESTORE_TOKEN"),
		S3KeyID: getenv("CAN_FILES_S3_ACCESS_KEY_ID"), S3Secret: getenv("CAN_FILES_S3_SECRET_ACCESS_KEY")}
	var errs []error
	if len(s.APIToken) < 32 || distinctBytes(s.APIToken) < 10 {
		errs = append(errs, errors.New("CAN_FILES_API_TOKEN is required, at least 32 characters with at least 10 different ones (for example `openssl rand -base64 32`)"))
	}
	if len(s.LinkSecret) < 32 || distinctBytes(s.LinkSecret) < 10 {
		errs = append(errs, errors.New("CAN_FILES_LINK_SECRET is required, at least 32 characters with at least 10 different ones; changing it invalidates every share link"))
	}
	if s.RestoreToken != "" && (len(s.RestoreToken) < 32 || distinctBytes(s.RestoreToken) < 10 || s.RestoreToken == s.APIToken) {
		errs = append(errs, errors.New("CAN_FILES_RESTORE_TOKEN, when set, is at least 32 characters with at least 10 different ones and differs from the API token"))
	}
	if cfg.Backend.Type == "s3" && (s.S3KeyID == "" || s.S3Secret == "") {
		errs = append(errs, errors.New("CAN_FILES_S3_ACCESS_KEY_ID and CAN_FILES_S3_SECRET_ACCESS_KEY are required with backend.type s3"))
	}
	return s, errors.Join(errs...)
}

// BuildBackend makes the blob backend the configuration names.
func BuildBackend(cfg *Config, sec Secrets) (Backend, error) {
	if cfg.Backend.Type == "s3" {
		s3 := cfg.Backend.S3
		return NewS3Backend(S3Options{Endpoint: s3.Endpoint, Bucket: s3.Bucket, Region: s3.Region, Prefix: s3.Prefix, KeyID: sec.S3KeyID, Secret: sec.S3Secret, AllowInsecure: s3.AllowInsecureHTTP})
	}
	return NewLocalBackend(cfg.Backend.Local.Dir)
}

// BuildScanner makes the virus scanner the configuration names (none by default).
func BuildScanner(cfg *Config) Scanner {
	if cfg.Scanner.Type == "clamd" {
		d, _ := time.ParseDuration(cfg.Scanner.Timeout)
		return &ClamdScanner{Address: cfg.Scanner.Address, Timeout: d}
	}
	return NoopScanner{}
}

// SetStaging points the store at its staging directory (0700, on the data volume) and removes
// what a crash left in it.
func (s *Store) SetStaging(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	s.stageDir = dir
	return nil
}

// Main is the can-files entry point. Exit codes: 0 ok, 1 runtime failure, 2 usage, config or
// secret problems.
func Main(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("can-files "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to the config file (default "+defaultConfigPath+" when present, else built-in defaults)")
	addr := fs.String("addr", "127.0.0.1:8095", "healthcheck: address of the listener")
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "can-files %s\n", Version)
		return 0
	case "serve", "check-config", "healthcheck", "verify-audit", "rotate-keys", "sweep":
	default:
		fmt.Fprintf(stderr, "unknown command %q (serve, check-config, healthcheck, verify-audit, rotate-keys, sweep, version)\n", cmd)
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
		if path = getenv("CAN_FILES_CONFIG"); path != "" {
			must = true
		} else {
			path = defaultConfigPath
		}
	}
	cfg, err := LoadConfig(path, must)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	sec, err := SecretsFromEnv(getenv, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "secrets: %v\n", err)
		return 2
	}
	if cmd == "check-config" {
		fmt.Fprintln(stdout, "config ok")
		return 0
	}
	log := obslog.New(stderr, obslog.Options{Service: "can-files", Version: Version, Level: getenv("CAN_LOG_LEVEL"), Format: getenv("CAN_LOG_FORMAT")})
	kr, err := LoadKeyring(cfg.KeyringPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 2
	}
	be, err := BuildBackend(cfg, sec)
	if err != nil {
		fmt.Fprintf(stderr, "backend: %v\n", err)
		return 2
	}
	can, err := modclient.FromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "can-core: %v\n", err)
		return 2
	}
	st, err := OpenStore(cfg.DBPath, StoreOptions{Keyring: kr, Backend: be, Scanner: BuildScanner(cfg), LinkSecret: []byte(sec.LinkSecret), Config: cfg, EventsOn: can != nil})
	if err != nil {
		fmt.Fprintf(stderr, "start: %v\n", err)
		return 2
	}
	defer func() { _ = st.Close() }()
	if err := st.SetStaging(cfg.StagingDir); err != nil {
		fmt.Fprintf(stderr, "start: staging directory: %v\n", err)
		return 2
	}
	switch cmd {
	case "verify-audit":
		if err := st.VerifyAudit(context.Background()); err != nil {
			fmt.Fprintf(stderr, "audit: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "audit chain ok")
		return 0
	case "rotate-keys":
		n, err := st.RotateKeys(context.Background())
		if err != nil {
			fmt.Fprintf(stderr, "rotate-keys: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "re-sealed %d values\n", n)
		return 0
	case "sweep":
		rep, err := st.RunRetention(context.Background())
		if err != nil {
			fmt.Fprintf(stderr, "sweep: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "purged %d, skipped (hold) %d, skipped (retention lock) %d, orphan blobs removed %d\n", rep.Purged, rep.SkippedHold, rep.SkippedLocked, rep.Orphans)
		return 0
	}

	auth := MultiAuth{StaticToken{Token: sec.APIToken}, StaticRestoreToken{Token: sec.RestoreToken}}
	if can != nil {
		auth = append(auth, &CanAuth{In: &modclient.Introspector{API: can}})
	}
	api := &API{Store: st, Auth: auth, Log: log, TrustProxy: cfg.TrustProxy}
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
	obs.DefaultHealth.Add("blobstore", true, be.Ping) // /readyz: the backend must answer too
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second,
		// Bodies have their own deadlines (uploads, downloads): see API.body and API.serve.
		ReadTimeout: 0, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		st.sweepLoop(ctx, log)
	}()
	if can != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			(&Dispatcher{Store: st, Pub: can, Log: log}).Run(ctx)
		}()
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("can-files listening", "addr", ln.Addr().String(), "backend", cfg.Backend.Type, "scanner", scannerName(cfg), "events", can != nil)
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

func scannerName(cfg *Config) string {
	if cfg.Scanner.Type == "" {
		return "none"
	}
	return cfg.Scanner.Type
}

type logger interface{ Error(msg string, args ...any) }

// sweepLoop runs the retention sweep shortly after start and then on its interval.
func (s *Store) sweepLoop(ctx context.Context, log logger) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.RunRetention(ctx); err != nil && ctx.Err() == nil {
			log.Error("retention sweep failed", "error", err)
		}
		t.Reset(time.Duration(s.cfg.Retention.SweepIntervalHours) * time.Hour)
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

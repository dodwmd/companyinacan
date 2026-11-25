// Package corecli implements the can-core command line: serve, audit verify|head|export,
// admin bootstrap and version. cmd/can-core is a thin main around Execute.
package corecli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/runtime"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Set by -ldflags in the Makefile.
var (
	version = "dev"
	commit  = "none"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func fail(format string, a ...any) error { return &exitError{exitFailure, fmt.Sprintf(format, a...)} }

const defaultDB = "/var/lib/can/state.db"

// Execute runs the command line and returns the exit code.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	root := &cobra.Command{Use: "can-core", Short: "Company in a Can control plane", SilenceUsage: true, SilenceErrors: true}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetIn(stdin)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &exitError{exitUsage, err.Error()} })
	root.AddCommand(serveCmd(getenv), auditCmd(getenv), adminCmd(stdin, getenv), &cobra.Command{
		Use: "version", Short: "Print the version", Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "can-core %s (%s)\n", version, commit)
		},
	})
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(stderr, "error:", ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(stderr, "error:", err)
	if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown flag") || strings.Contains(err.Error(), "accepts") {
		return exitUsage
	}
	return exitFailure
}

func dbFlag(cmd *cobra.Command, getenv func(string) string) *string {
	def := defaultDB
	if v := getenv("CAN_DB_PATH"); v != "" {
		def = v
	}
	return cmd.Flags().String("db", def, "path to the can-core SQLite state database (default $CAN_DB_PATH)")
}

func serveCmd(getenv func(string) string) *cobra.Command {
	return &cobra.Command{
		Use: "serve", Short: "Run can-core (admin API, module API, webhook dispatcher)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := runtime.FromEnv(getenv)
			if err != nil {
				return fail("%v", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runtime.Run(ctx, cfg, cfg.Logger(os.Stderr))
		},
	}
}

// openExisting opens a database that must already exist (verification must never create one).
func openExisting(ctx context.Context, path string) (*store.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fail("state database %s: %v", path, err)
	}
	return store.Open(ctx, path)
}

func auditCmd(getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Inspect the tamper-evident audit log"}

	verify := &cobra.Command{Use: "verify", Short: "Verify the audit hash chain; exits 1 if it was modified, reordered or truncated", Args: cobra.NoArgs}
	vdb := dbFlag(verify, getenv)
	head := verify.Flags().String("head", "", "externally recorded head (seq:hash from `audit head`) the log must still contain; detects truncation")
	asJSON := verify.Flags().String("output", "text", "text or json")
	verify.RunE = func(c *cobra.Command, _ []string) error {
		ctx := c.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		db, err := openExisting(ctx, *vdb)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		opt := audit.VerifyOptions{}
		if *head != "" {
			h, err := audit.ParseHead(*head)
			if err != nil {
				return &exitError{exitUsage, err.Error()}
			}
			opt.Expected = &h
		}
		res, err := audit.New(db).Verify(ctx, opt)
		if err != nil {
			return fail("%v", err)
		}
		w := c.OutOrStdout()
		if *asJSON == "json" {
			_ = json.NewEncoder(w).Encode(res)
		} else if res.OK() {
			fmt.Fprintf(w, "audit log OK: %d entries, head %s\n", res.Entries, res.Head)
		} else {
			fmt.Fprintf(w, "audit log TAMPERED: %s at record %d: %s\n", res.Violation.Kind, res.Violation.Seq, res.Violation.Message)
		}
		if !res.OK() {
			return &exitError{exitFailure, ""}
		}
		return nil
	}

	headCmd := &cobra.Command{Use: "head", Short: "Print the newest entry as seq:hash (record it somewhere safe to detect truncation later)", Args: cobra.NoArgs}
	hdb := dbFlag(headCmd, getenv)
	headCmd.RunE = func(c *cobra.Command, _ []string) error {
		db, err := openExisting(c.Context(), *hdb)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		h, err := audit.New(db).Head(c.Context())
		if err != nil {
			return fail("%v", err)
		}
		if h.Seq == 0 {
			return fail("the audit log is empty")
		}
		fmt.Fprintln(c.OutOrStdout(), h)
		return nil
	}

	export := &cobra.Command{Use: "export", Short: "Write the audit log as JSON lines", Args: cobra.NoArgs}
	edb := dbFlag(export, getenv)
	export.RunE = func(c *cobra.Command, _ []string) error {
		db, err := openExisting(c.Context(), *edb)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		return audit.New(db).Export(c.Context(), c.OutOrStdout())
	}
	cmd.AddCommand(verify, headCmd, export)
	return cmd
}

func adminCmd(stdin io.Reader, getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Administrator account"}
	boot := &cobra.Command{
		Use: "bootstrap", Short: "Create the admin account and print its TOTP secret once", Args: cobra.NoArgs,
		Long: "Creates the single admin account. The password comes from $CAN_ADMIN_PASSWORD or the first line of stdin\n" +
			"(never a flag, so it cannot leak into process listings). TOTP is mandatory: the secret and otpauth URI are printed once.",
	}
	db := dbFlag(boot, getenv)
	user := boot.Flags().String("username", "admin", "admin username")
	issuer := boot.Flags().String("issuer", "Company in a Can", "issuer label shown in the authenticator app")
	boot.RunE = func(c *cobra.Command, _ []string) error {
		pw := getenv("CAN_ADMIN_PASSWORD")
		if pw == "" {
			line, err := bufio.NewReader(stdin).ReadString('\n')
			if err != nil && line == "" {
				return fail("no password: set CAN_ADMIN_PASSWORD or pipe it on stdin")
			}
			pw = strings.TrimRight(line, "\r\n")
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return fail("%v", err)
		}
		d, err := store.Open(c.Context(), *db)
		if err != nil {
			return fail("%v", err)
		}
		defer func() { _ = d.Close() }()
		secret := auth.NewTOTPSecret()
		if _, err := d.CreateAdmin(c.Context(), *user, hash, secret); err != nil {
			if errors.Is(err, store.ErrExists) {
				return fail("admin %q already exists", *user)
			}
			return fail("%v", err)
		}
		if _, err := audit.New(d).Append(c.Context(), audit.Entry{Actor: "cli", Action: "admin.bootstrap", Target: "admin"}); err != nil {
			return fail("%v", err)
		}
		w := c.OutOrStdout()
		fmt.Fprintf(w, "Admin %q created.\nTOTP secret (shown once): %s\nProvisioning URI: %s\n", *user, secret, auth.TOTPURI(*issuer, *user, secret))
		return nil
	}
	cmd.AddCommand(boot)
	return cmd
}

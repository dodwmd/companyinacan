package corecli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/diagnose"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// diagnoseCmd is `can-core admin diagnose`: the in-pod counterpart of `can diagnose`. It opens the
// state database read-only (no migration, no creation, no writes) and collects counts, states and
// the audit chain verdict. It never selects a secret column (hmac_secret, password hashes, TOTP
// secrets, session tokens) and never reads a row of people data.
func diagnoseCmd(getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{
		Use: "diagnose", Short: "Write a support bundle: versions, database and module state, redacted config, log tail", Args: cobra.NoArgs,
		Long: "Writes a tar.gz with an allowlist of files (see `can diagnose --help` for the guarantees). The database is\n" +
			"opened read-only. Run it inside the can-core pod (kubectl exec) and copy the file out with kubectl cp.",
	}
	db := dbFlag(cmd, getenv)
	out := cmd.Flags().String("out", "", "bundle path (default ./can-core-diagnose-<time>.tar.gz); an existing file is never overwritten")
	company := cmd.Flags().String("company-file", firstSet(getenv("CAN_COMPANY_FILE"), "/etc/can/company/company.yaml"), "applied company.yaml (default $CAN_COMPANY_FILE)")
	logFile := cmd.Flags().String("log-file", "", "a log file whose last lines to include")
	lines := cmd.Flags().Int("lines", 200, "log lines to keep")
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		ctx := c.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if *out == "" {
			*out = "can-core-diagnose-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
		}
		if _, err := os.Stat(*db); err != nil {
			return fail("state database %s: %v", *db, err)
		}
		sdb, err := sql.Open("sqlite", "file:"+*db+"?mode=ro&_pragma=busy_timeout(3000)")
		if err != nil {
			return fail("%v", err)
		}
		defer func() { _ = sdb.Close() }()
		sdb.SetMaxOpenConns(1)

		b := diagnose.New()
		var problems []string
		add := func(name string, v any) {
			if err := b.AddJSON(name, v); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			}
		}
		add("versions.json", map[string]string{"can-core": version, "commit": commit, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
		if raw, err := os.ReadFile(*company); err == nil { //nolint:gosec // operator-chosen path
			if len(raw) > 256<<10 {
				raw = raw[:256<<10]
			}
			if err := b.AddText("config.yaml", string(raw)); err != nil {
				problems = append(problems, "config.yaml: "+err.Error())
			}
		}
		st, mods, rec := collectState(ctx, sdb, *db)
		add("state.json", st)
		add("modules.json", mods)
		if rec != nil {
			add("reconcile.json", rec)
		}
		if *logFile != "" {
			if f, err := os.Open(*logFile); err == nil { //nolint:gosec // operator-chosen path
				if err := b.AddText("logs.txt", diagnose.TailLines(f, *lines, 1024)); err != nil {
					problems = append(problems, "logs.txt: "+err.Error())
				}
				_ = f.Close()
			}
		}
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-chosen output path
		if err != nil {
			return fail("%v", err)
		}
		if err := b.Encode(f); err != nil {
			_ = f.Close()
			_ = os.Remove(*out)
			return fail("writing the bundle: %v", err)
		}
		if err := f.Close(); err != nil {
			return fail("%v", err)
		}
		fmt.Fprintf(c.OutOrStdout(), "wrote %s. Review it before sharing; secrets and email addresses are removed, names and phone numbers in free text may remain.\n", *out)
		for _, p := range problems {
			fmt.Fprintln(c.OutOrStdout(), "note:", p)
		}
		return nil
	}
	return cmd
}

func firstSet(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type moduleInfo struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	State     string `json:"state"`
	LastError string `json:"last_error,omitempty"`
}

// collectState gathers counts and states. Every query is a COUNT or selects non-secret columns, and
// a failure of one is recorded as an "error" note instead of aborting the bundle.
func collectState(ctx context.Context, sdb *sql.DB, path string) (map[string]any, []moduleInfo, map[string]any) {
	st := map[string]any{}
	size := func(p string) int64 {
		if fi, err := os.Stat(p); err == nil {
			return fi.Size()
		}
		return 0
	}
	st["db_bytes"], st["db_wal_bytes"] = size(path), size(path+"-wal")
	count := func(key, q string) {
		var n int64
		if err := sdb.QueryRowContext(ctx, q).Scan(&n); err != nil {
			st[key] = "unavailable"
			return
		}
		st[key] = n
	}
	var v sql.NullInt64
	if err := sdb.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err == nil {
		st["schema_version"] = v.Int64
	} else {
		st["schema_version"] = "unavailable"
	}
	count("deliveries_pending", `SELECT COUNT(*) FROM deliveries WHERE status='pending'`)
	count("deliveries_dead", `SELECT COUNT(*) FROM deliveries WHERE status='dead'`)
	count("approvals_pending", `SELECT COUNT(*) FROM approval_requests WHERE status='pending'`)
	count("events_stored", `SELECT COUNT(*) FROM events`)
	count("modules", `SELECT COUNT(*) FROM modules`)

	al := audit.New(&store.DB{DB: sdb, Now: time.Now})
	if res, err := al.Verify(ctx, audit.VerifyOptions{}); err != nil {
		st["audit_chain_ok"] = "unavailable"
	} else {
		st["audit_entries"] = res.Entries
		st["audit_chain_ok"] = res.OK()
		if res.Violation != nil {
			st["audit_violation"] = map[string]any{"kind": res.Violation.Kind, "seq": res.Violation.Seq}
		}
	}

	mods := []moduleInfo{}
	if rows, err := sdb.QueryContext(ctx, `SELECT id, version, state, last_error FROM modules ORDER BY id`); err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var m moduleInfo
			if rows.Scan(&m.ID, &m.Version, &m.State, &m.LastError) == nil {
				m.LastError = modmgr.Sanitise(m.LastError)
				mods = append(mods, m)
			}
		}
	}

	var rec map[string]any
	var at int64
	var ok, actions, failed int
	var msg string
	if err := sdb.QueryRowContext(ctx, `SELECT at, ok, actions, failed, error FROM reconcile_status WHERE id=1`).Scan(&at, &ok, &actions, &failed, &msg); err == nil {
		rec = map[string]any{"at": time.Unix(at, 0).UTC().Format(time.RFC3339), "ok": ok == 1, "actions": actions, "failed": failed, "error": modmgr.Sanitise(msg)}
	}
	return st, mods, rec
}

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/validate"
)

// backupSeams are what tests replace: the command runner (restic, systemctl,
// the backup script), the object store and the restore cluster adapters.
var backupSeams = struct {
	runner backup.Runner
	store  func(k *backup.Kit) backup.ObjectStore
	// restore returns the provisioner and cluster adapter for `can restore`.
	// can-core (epic E03) supplies the real ones; until then they report "not implemented".
	restore func(k *backup.Kit) (backup.Provisioner, backup.Cluster)
}{
	runner: backup.ExecRunner{},
	store: func(k *backup.Kit) backup.ObjectStore {
		return &backup.S3Store{Endpoint: k.Bucket.Endpoint, Bucket: k.Bucket.Name, Region: k.Bucket.Region,
			AccessKeyID: k.Bucket.AccessKeyID, SecretAccessKey: k.Bucket.SecretAccessKey}
	},
	restore: func(*backup.Kit) (backup.Provisioner, backup.Cluster) {
		u := unavailable{}
		return u, u
	},
}

// unavailable implements Provisioner and Cluster for steps that need can-core.
type unavailable struct{}

func (unavailable) nope(what string) error {
	return provider.NotImplemented("restore step "+what+" (needs can-core)", "E03")
}
func (u unavailable) Provision(context.Context, *backup.Kit) error { return u.nope("provision") }
func (u unavailable) ImportState(context.Context, string) error    { return u.nope("import-state") }
func (u unavailable) Apply(context.Context) error                  { return u.nope("apply") }
func (u unavailable) RestoreModules(context.Context) ([]string, error) {
	return nil, u.nope("module restore hooks")
}
func (u unavailable) ScaleDown(context.Context, []string) error { return u.nope("scale-down") }
func (u unavailable) PVCDirs(context.Context) ([]string, error) { return nil, u.nope("pvc listing") }
func (u unavailable) Purge(context.Context, string, string) error {
	return u.nope("purge hooks")
}
func (u unavailable) ReapplyTombstones(context.Context, []backup.Entry) error {
	return u.nope("tombstone re-apply")
}
func (u unavailable) ScaleUp(context.Context) error { return u.nope("scale-up") }

// mapErr turns a not-implemented error into exit code 3.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, provider.ErrNotImplemented) {
		return &exitError{ExitNotImplemented, err.Error()}
	}
	return failure("%v", err)
}

type repoFlags struct {
	repo, passwordFile, envFile, dumpsDir, hostStateDir string
}

func (f *repoFlags) add(cmd *cobra.Command) {
	d := backup.DefaultSettings()
	cmd.Flags().StringVar(&f.repo, "repo", os.Getenv("RESTIC_REPOSITORY"), "restic repository (default $RESTIC_REPOSITORY)")
	cmd.Flags().StringVar(&f.passwordFile, "password-file", filepath.Join(d.ConfigDir, backup.PasswordFileName), "file holding the restic password")
	cmd.Flags().StringVar(&f.envFile, "env-file", filepath.Join(d.ConfigDir, backup.EnvFileName), "KEY=VALUE file with the bucket credentials")
	cmd.Flags().StringVar(&f.dumpsDir, "dumps-dir", d.DumpsDir, "dumps directory")
	cmd.Flags().StringVar(&f.hostStateDir, "host-state-dir", d.HostStateDir, "root-owned host state directory (manifest, marker and ledger copies)")
}

// restic builds the wrapper, or nil when no repository is configured.
func (f *repoFlags) restic() (*backup.Restic, error) {
	if f.repo == "" {
		return nil, nil
	}
	env, err := backup.LoadEnvFile(f.envFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &backup.Restic{R: backupSeams.runner, Repo: backup.Repo{Repository: f.repo, PasswordFile: f.passwordFile, Env: env}}, nil
}

func newBackupCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up, verify and manage the recovery kit (run on the host)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return usage("backup needs a subcommand: run, status, verify, kit, install, ledger-add, ledger-sync")
		},
	}
	cmd.AddCommand(newBackupRunCmd(), newBackupStatusCmd(g), newBackupVerifyCmd(g), newBackupKitCmd(),
		newBackupInstallCmd(g), newLedgerAddCmd(), newLedgerSyncCmd())
	return cmd
}

func newBackupRunCmd() *cobra.Command {
	script := backup.DefaultSettings().ScriptPath
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the installed backup script now (same as the systemd timer)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := backupSeams.runner.Run(cmd.Context(), backup.Cmd{Name: script})
			_, _ = cmd.OutOrStdout().Write(out)
			var ce *backup.CmdError
			if errors.As(err, &ce) {
				return failure("backup failed with exit code %d (see docs/backup-restore.md): %s", ce.Code, strings.TrimSpace(ce.Stderr))
			}
			return mapErr(err)
		},
	}
	cmd.Flags().StringVar(&script, "script", script, "installed backup script")
	return cmd
}

func newBackupStatusCmd(g *globalOpts) *cobra.Command {
	var f repoFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the completion marker, latest snapshot and problems",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := f.restic()
			if err != nil {
				return failure("%v", err)
			}
			var api backup.ResticAPI
			if r != nil {
				api = r
			}
			rep := backup.Status(cmd.Context(), api, f.dumpsDir, lifecycle.now(), backup.DefaultSettings().MaxMarkerAge)
			if g.json() {
				_ = json.NewEncoder(cmd.OutOrStdout()).Encode(rep)
			} else {
				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "backups enabled: %v\nsnapshots: %d\n", rep.Enabled, rep.Snapshots)
				if rep.Latest != "" {
					fmt.Fprintf(w, "latest snapshot: %s at %s\n", rep.Latest, rep.LatestAt.Format("2006-01-02 15:04 MST"))
				}
				if rep.RunID != "" {
					fmt.Fprintf(w, "last dump run: %s finished %s\n", rep.RunID, rep.MarkerAt.Format("2006-01-02 15:04 MST"))
				}
				for _, p := range rep.Problems {
					fmt.Fprintln(w, "PROBLEM:", p)
				}
			}
			if len(rep.Problems) > 0 {
				return silent(ExitFailure)
			}
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newBackupVerifyCmd(g *globalOpts) *cobra.Command {
	var f repoFlags
	var sample int
	var readData bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "restic check, then restore a sample into a temp dir and compare hashes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := f.restic()
			if err != nil {
				return failure("%v", err)
			}
			if r == nil {
				return failure("backups are off: no repository configured (--repo)")
			}
			rep, err := backup.Verify(cmd.Context(), r, backup.VerifyOptions{DumpsDir: f.dumpsDir, HostStateDir: f.hostStateDir, Sample: sample, ReadData: readData})
			if g.json() {
				_ = json.NewEncoder(cmd.OutOrStdout()).Encode(rep)
			} else if rep != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "snapshot: %s\ncheck ok: %v\nsampled: %s\n", rep.Snapshot, rep.CheckOK, strings.Join(rep.Sampled, ", "))
			}
			if err != nil {
				return failure("%v", err)
			}
			if !g.json() {
				fmt.Fprintln(cmd.OutOrStdout(), "verify OK")
			}
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().IntVar(&sample, "sample", 3, "number of dump files to restore and hash")
	cmd.Flags().BoolVar(&readData, "read-data", false, "also read every pack (slow)")
	return cmd
}

// newPassphraseSource is a seam so tests can script the typed passphrase.
var newPassphraseSource = passphraseSource

// passphraseSource returns a func that supplies the passphrase from a file or
// the controlling terminal, or nil when neither is available.
func passphraseSource(file string) func(string) (string, error) {
	if file != "" {
		return func(string) (string, error) {
			b, err := os.ReadFile(file) //nolint:gosec // operator-chosen path
			return strings.TrimRight(string(b), "\r\n"), err
		}
	}
	return func(prompt string) (string, error) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return "", errors.New("no terminal available: pass --passphrase-file")
		}
		defer func() { _ = tty.Close() }()
		fmt.Fprint(tty, prompt)
		b, err := term.ReadPassword(int(tty.Fd())) //nolint:gosec // fd fits int
		fmt.Fprintln(tty)
		return string(b), err
	}
}

func newBackupKitCmd() *cobra.Command {
	var out, passFile, dir, endpoint, bucket, region, credFile, pwFile string
	cmd := &cobra.Command{
		Use:   "kit",
		Short: "Generate the recovery kit; print it once, or write it as an encrypted file",
		Long: `Generate a recovery kit (restic password, bucket credentials, provider info).
Without --out the kit is shown once after a confirmation prompt. With --out it is
written encrypted (argon2id + XChaCha20-Poly1305) to a new file outside any git
working tree. The kit is never written to the company repository.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			co, err := loadCompanyOnly(cmd, dir)
			if err != nil {
				return err
			}
			env, err := backup.LoadEnvFile(credFile)
			if err != nil {
				return failure("credentials file: %v", err)
			}
			var key, secret string
			for _, e := range env {
				k, v, _ := strings.Cut(e, "=")
				switch k {
				case "AWS_ACCESS_KEY_ID":
					key = v
				case "AWS_SECRET_ACCESS_KEY":
					secret = v
				}
			}
			// Never mint a second password for a repository that already has one.
			existing := ""
			if b, rerr := os.ReadFile(pwFile); rerr == nil { //nolint:gosec // operator-chosen path
				existing = strings.TrimSpace(string(b))
				fmt.Fprintf(cmd.ErrOrStderr(), "reusing the restic password in %s\n", pwFile)
			} else if !errors.Is(rerr, os.ErrNotExist) {
				return failure("%v", rerr)
			}
			kit, err := backup.NewKit(backup.KitInput{
				Password: existing, CompanyName: co.Name, Domain: co.Domain, Endpoint: endpoint, Bucket: bucket, Region: region,
				AccessKeyID: key, SecretAccessKey: secret, ProviderType: co.Provider.Type, ProviderOptions: co.Provider.Options,
			})
			if err != nil {
				return failure("%v", err)
			}
			if existing == "" {
				if err := ensureNoRepo(cmd, kit); err != nil {
					return err
				}
			}
			if out != "" {
				ask := newPassphraseSource(passFile)
				pass, err := ask("New kit passphrase: ")
				if err != nil {
					return failure("%v", err)
				}
				if passFile == "" { // typed: confirm it, a typo would make the kit unreadable
					again, err := ask("Confirm passphrase: ")
					if err != nil {
						return failure("%v", err)
					}
					if again != pass {
						return failure("passphrases do not match; nothing was written")
					}
				}
				if err := backup.WriteKitFile(out, kit, pass); err != nil {
					return failure("%v", err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote encrypted recovery kit to %s; store it and its passphrase outside the server\n", out)
				return nil
			}
			if err := backup.PrintOnce(cmd.InOrStdin(), cmd.OutOrStdout(), kit); err != nil {
				return failure("%v: the kit was not acknowledged; re-run, or use --out", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write an encrypted kit file here instead of printing")
	cmd.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the passphrase for --out (default: prompt)")
	cmd.Flags().StringVar(&dir, "dir", ".", "company directory (company.yaml)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "S3 endpoint URL, e.g. https://<account>.r2.cloudflarestorage.com")
	cmd.Flags().StringVar(&bucket, "bucket", "", "bucket name")
	cmd.Flags().StringVar(&region, "region", "auto", "bucket region")
	cmd.Flags().StringVar(&pwFile, "password-file", filepath.Join(backup.DefaultSettings().ConfigDir, backup.PasswordFileName), "existing restic password file: reused when present")
	cmd.Flags().StringVar(&credFile, "credentials-file", "", "KEY=VALUE file with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	_ = cmd.MarkFlagRequired("endpoint")
	_ = cmd.MarkFlagRequired("bucket")
	_ = cmd.MarkFlagRequired("credentials-file")
	return cmd
}

// ensureNoRepo refuses to create a kit with a fresh password when a restic
// repository already exists at the target: that password would not open it.
func ensureNoRepo(cmd *cobra.Command, kit *backup.Kit) error {
	pw, cleanup, err := kit.PasswordFile(os.TempDir())
	if err != nil {
		return failure("%v", err)
	}
	defer cleanup()
	r := &backup.Restic{R: backupSeams.runner, Repo: kit.Repo(pw)}
	_, err = r.EnsureNoRepo(cmd.Context())
	var ce *backup.CmdError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return failure("a restic repository may already exist at %s (%v); refusing to generate a new password for it. Reuse the existing password via --password-file, or choose another bucket/prefix", kit.Restic.Repository, ce)
	default:
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not check for an existing repository (%v)\n", err)
		return nil
	}
}

func loadCompanyOnly(cmd *cobra.Command, dir string) (*config.Company, error) {
	doc, ps := config.Load(filepath.Join(dir, "company.yaml"))
	if problem.HasErrors(ps) {
		return nil, failure("%s", ps[0].String())
	}
	co, err := config.DecodeCompany(doc)
	if err != nil {
		return nil, failure("%v", err)
	}
	return co, nil
}

func newBackupInstallCmd(g *globalOpts) *cobra.Command {
	var root, dir, modulesDir, kitPath, passFile string
	var enable, plan bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the backup script, systemd service and timer (and credentials from --kit) on this host",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			co, err := loadCompanyOnly(cmd, dir)
			if err != nil {
				return err
			}
			mods := map[string]*config.Module{}
			md := resolveModulesDir(modulesDir, dir)
			for id, e := range co.Modules {
				if !e.IsEnabled() {
					continue
				}
				p, ok := validate.ManifestPath(md, id)
				if !ok {
					return failure("module %q has no manifest in %s", id, md)
				}
				doc, ps := config.Load(p)
				if problem.HasErrors(ps) {
					return failure("%s", ps[0].String())
				}
				if mods[id], err = config.DecodeModule(doc); err != nil {
					return failure("%s: %v", p, err)
				}
			}
			s := backup.DefaultSettings()
			var kit *backup.Kit
			if kitPath != "" {
				if kit, err = readKit(cmd, kitPath, passFile); err != nil {
					return err
				}
				s.Repository = kit.Restic.Repository
			}
			p, err := backup.BuildPlan(co, mods, s)
			if err != nil {
				return failure("%v", err)
			}
			for _, w := range p.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			if plan {
				fmt.Fprint(cmd.OutOrStdout(), backup.GenerateScript(p, s))
				return nil
			}
			if kit == nil {
				return failure("--kit is required: the repository and credentials come from the recovery kit")
			}
			at := func(p string) string { return filepath.Join(root, p) }
			files := []struct {
				path string
				body string
				mode os.FileMode
			}{
				{at(s.ScriptPath), backup.GenerateScript(p, s), 0o700},
				{at("/etc/systemd/system/can-backup.service"), backup.GenerateService(s), 0o644},
				{at("/etc/systemd/system/can-backup.timer"), backup.GenerateTimer(s), 0o644},
				{at(s.ConfigDir + "/" + backup.PasswordFileName), kit.Restic.Password, 0o600},
				{at(s.ConfigDir + "/" + backup.EnvFileName), backup.FormatEnvFile(kit.Repo("").Env), 0o600},
				{at("/etc/systemd/system/can-backup-failure.service"), backup.GenerateFailureService(), 0o644},
			}
			for _, f := range files {
				if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil { //nolint:gosec // system directories
					return failure("%v", err)
				}
				if err := os.WriteFile(f.path, []byte(f.body), f.mode); err != nil {
					return failure("%v", err)
				}
				if err := os.Chmod(f.path, f.mode); err != nil {
					return failure("%v", err)
				}
			}
			r := &backup.Restic{R: backupSeams.runner, Repo: kit.Repo(at(s.ConfigDir + "/" + backup.PasswordFileName))}
			created, err := r.EnsureInit(cmd.Context())
			if err != nil {
				return failure("restic repository check/init: %v", err)
			}
			if created {
				fmt.Fprintln(cmd.OutOrStdout(), "initialised a new restic repository")
			}
			if enable {
				for _, a := range [][]string{{"daemon-reload"}, {"enable", "--now", "can-backup.timer"}} {
					if _, err := backupSeams.runner.Run(cmd.Context(), backup.Cmd{Name: "systemctl", Args: a}); err != nil {
						return failure("systemctl %s: %v", strings.Join(a, " "), err)
					}
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "backup installed")
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", "/", "filesystem root to install under (for testing/chroot)")
	cmd.Flags().StringVar(&dir, "dir", ".", "company directory (company.yaml)")
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "modules directory (default: ./modules)")
	cmd.Flags().StringVar(&kitPath, "kit", "", "recovery kit file, or - for stdin")
	cmd.Flags().StringVar(&passFile, "passphrase-file", "", "passphrase file for an encrypted kit")
	cmd.Flags().BoolVar(&enable, "enable", false, "systemctl daemon-reload and enable --now the timer")
	cmd.Flags().BoolVar(&plan, "plan", false, "print the generated backup script and change nothing")
	_ = g
	return cmd
}

func readKit(cmd *cobra.Command, path, passFile string) (*backup.Kit, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(path) //nolint:gosec // operator-chosen path
	}
	if err != nil {
		return nil, failure("reading kit: %v", err)
	}
	kit, err := backup.ParseKit(data, passphraseSource(passFile))
	if err != nil {
		return nil, failure("%v", err)
	}
	return kit, nil
}

func newLedgerAddCmd() *cobra.Command {
	var ledger, subject, class string
	cmd := &cobra.Command{
		Use:   "ledger-add",
		Short: "Record an erasure tombstone in the local ledger (subject id and data class only; no PII)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l := &backup.Ledger{Path: ledger}
			e, added, err := l.Append(subject, class)
			if err != nil {
				return failure("%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "entry %d (%s, %s, %s) added=%v\n", e.Seq, e.Subject, e.Class, e.Date, added)
			return nil
		},
	}
	cmd.Flags().StringVar(&ledger, "ledger", backup.DefaultSettings().LedgerSource, "ledger file")
	cmd.Flags().StringVar(&subject, "subject", "", "opaque subject id")
	cmd.Flags().StringVar(&class, "class", "", "data class")
	_ = cmd.MarkFlagRequired("subject")
	_ = cmd.MarkFlagRequired("class")
	return cmd
}

func newLedgerSyncCmd() *cobra.Command {
	var f repoFlags
	var ledger string
	var insecure bool
	cmd := &cobra.Command{
		Use:   "ledger-sync",
		Short: "Merge the local erasure ledger into the encrypted object in the bucket (called by the backup script)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, pw, err := f.store(insecure)
			if err != nil {
				return failure("%v", err)
			}
			n, err := backup.SyncLedger(cmd.Context(), st, &backup.Ledger{Path: ledger}, pw)
			if err != nil {
				return failure("%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "erasure ledger synced (%d entries)\n", n)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&ledger, "ledger", backup.DefaultSettings().LedgerSource, "local ledger file")
	cmd.Flags().BoolVar(&insecure, "allow-insecure-endpoint", false, "allow an http:// endpoint (testing only)")
	return cmd
}

// store builds the bucket client from --repo (s3:https://host/bucket/prefix) and the env file.
func (f *repoFlags) store(insecure bool) (backup.ObjectStore, string, error) {
	rest, ok := strings.CutPrefix(f.repo, "s3:")
	if !ok {
		return nil, "", errors.New("--repo must be an s3: repository")
	}
	u, err := url.Parse(rest)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("bad repository %q", f.repo)
	}
	bucket, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	env, err := backup.LoadEnvFile(f.envFile)
	if err != nil {
		return nil, "", err
	}
	s := &backup.S3Store{Endpoint: u.Scheme + "://" + u.Host, Bucket: bucket, Region: "auto", AllowInsecure: insecure}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "AWS_ACCESS_KEY_ID":
			s.AccessKeyID = v
		case "AWS_SECRET_ACCESS_KEY":
			s.SecretAccessKey = v
		case "AWS_DEFAULT_REGION":
			s.Region = v
		}
	}
	pw, err := os.ReadFile(f.passwordFile) //nolint:gosec // operator-chosen path
	if err != nil {
		return nil, "", err
	}
	return s, strings.TrimSpace(string(pw)), nil
}

func newRestoreCmd(g *globalOpts) *cobra.Command {
	var kitPath, passFile, dir, snapshot string
	var plan, allowNoLedger, force bool
	d := backup.DefaultSettings()
	var root, dumps, hostState, pvcRoot, staging, statePath string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a company from its recovery kit (NOT FUNCTIONAL until the can-core adapters exist; --plan works)",
		Long: `NOT FUNCTIONAL YET: steps 1-6 need the can-core and provider adapters (epic E03), which are
not part of this release. A real run stops at step 1 with exit code 3. Only --plan works.

Restore onto a blank host from the recovery kit: (1) provision, k3s, can-core in
restore mode; (2) dumps directory first, can-core imports its state; (3) restore apply from
the restored company.yaml; (4) module restore hooks; (5) PVC data; (6) erasure tombstone
ledger re-applied, then workloads scale up.

Progress is recorded in --state; after a failure run the same command again to resume.
Use --plan to print the steps without touching anything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if kitPath == "" {
				return usage("--kit <file|-> is required")
			}
			kit, err := readKit(cmd, kitPath, passFile)
			if err != nil {
				return err
			}
			if statePath == "" {
				statePath = filepath.Join(dir, ".can", "restore.json")
			}
			prov, cl := backupSeams.restore(kit)
			pw, cleanup, err := kit.PasswordFile(os.TempDir())
			if err != nil {
				return failure("%v", err)
			}
			defer cleanup()
			rs := &backup.Restic{R: backupSeams.runner, Repo: kit.Repo(pw)}
			o := &backup.Restore{
				Kit: kit, Provisioner: prov, Restic: rs, Cluster: cl,
				Ledger: backup.BucketLedger{Store: backupSeams.store(kit), Password: kit.Restic.Password},
				Root:   root, DumpsDir: dumps, HostStateDir: hostState, Force: force, PVCRoot: pvcRoot, StagingDir: staging, CompanyDir: dir,
				StatePath: statePath, Snapshot: snapshot, AllowNoLedger: allowNoLedger, Out: cmd.ErrOrStderr(),
			}
			if plan {
				steps := o.Plan()
				if g.json() {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(steps)
				}
				for _, s := range steps {
					mark := " "
					if s.Done {
						mark = "x"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "[%s] %d. %s\n      %s\n", mark, s.N, s.Title, s.Detail)
				}
				return nil
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			if err := o.Run(ctx); err != nil {
				return mapErr(err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "restore complete")
			return nil
		},
	}
	cmd.Flags().StringVar(&kitPath, "kit", "", "recovery kit file, or - for stdin")
	cmd.Flags().StringVar(&passFile, "passphrase-file", "", "passphrase file for an encrypted kit")
	cmd.Flags().StringVar(&dir, "dir", ".", "directory to write company.yaml back into (the new company repo)")
	cmd.Flags().BoolVar(&plan, "plan", false, "print the six steps and what is already done; change nothing")
	cmd.Flags().BoolVar(&allowNoLedger, "allow-no-ledger", false, "go live even if the bucket holds no erasure ledger")
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "restic snapshot id (default: latest, pinned on first run)")
	cmd.Flags().StringVar(&root, "root", "/", "host root dumps are restored under")
	cmd.Flags().StringVar(&dumps, "dumps-dir", d.DumpsDir, "dumps directory")
	cmd.Flags().StringVar(&hostState, "host-state-dir", d.HostStateDir, "host state directory")
	cmd.Flags().BoolVar(&force, "force", false, "restore over a non-blank host and accept a ledger behind the snapshot's copy (overwrites data)")
	cmd.Flags().StringVar(&pvcRoot, "pvc-root", d.PVCRoot, "k3s local-path storage directory")
	cmd.Flags().StringVar(&staging, "staging", "/var/lib/can/restore-staging", "scratch directory for the PVC restore (must be absolute, at least 3 levels deep, empty or created by can restore)")
	cmd.Flags().StringVar(&statePath, "state", "", "restore progress file (default <dir>/.can/restore.json)")
	return cmd
}

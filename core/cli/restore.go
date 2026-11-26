package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/launch"
)

type restoreFlags struct {
	kit, passFile, dir, snapshot, repo                                             string
	root, dumps, hostState, pvcRoot, staging, state                                string
	kubeconfig, kubeContext, kubectl, imagesFrom, releaseManifest, coreImage, acme string
	confirm                                                                        string
	plan, allowNoLedger, acceptBehind, forceOlder, withoutSecrets                  bool
	acceptUnrestored, acceptUnerased, missingBlobs                                 []string
	acceptCompany                                                                  string
}

const k3sKubeconfig = "/etc/rancher/k3s/k3s.yaml"

func newRestoreCmd(g *globalOpts) *cobra.Command {
	var f restoreFlags
	d := backup.DefaultSettings()
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a company from its recovery kit onto a blank server (run on that server, as root)",
		Long: `Run on the new server, as root, after k3s is installed (the same bootstrap ` + "`can up`" + ` uses) with
restic 0.17 or newer on it. It restores the newest consistent snapshot: dumps and Secrets, then
can-core in restore mode (admin login and TOTP preserved), the modules with workloads at 0, volume
files, module restore hooks, the erasure ledger, and only then scales the company up.

Nothing is sent and nothing is public until the last step. A server that is not blank is refused
unless you name the company with --confirm-overwrite. Progress is recorded in --state: after a
failure fix the cause and run the same command again. --plan prints the steps and changes nothing.

It does not create the server, publish the site, route the admin UI or install the backup timer:
run ` + "`can up`" + ` afterwards. docs/backup-restore.md lists what has not been verified.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runRestore(cmd, g, &f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.kit, "kit", "", "recovery kit file, or - for stdin")
	fl.StringVar(&f.passFile, "passphrase-file", "", "passphrase file for an encrypted kit")
	fl.StringVar(&f.dir, "dir", ".", "directory to write company.yaml back into (the new company repo)")
	fl.BoolVar(&f.plan, "plan", false, "print the steps and what is already done; change nothing")
	fl.StringVar(&f.snapshot, "snapshot", "", "restic snapshot id (default: latest, pinned on first run)")
	fl.StringVar(&f.confirm, "confirm-overwrite", "", "the company name: restore over a server that is not blank (its data is lost)")
	fl.BoolVar(&f.allowNoLedger, "allow-no-ledger", false, "go live even if the bucket holds no erasure ledger (only if no erasure was ever requested)")
	fl.BoolVar(&f.acceptBehind, "accept-ledger-behind", false, "apply the snapshot's ledger copy although the bucket ledger lacks entries it has (erasures may be lost)")
	fl.BoolVar(&f.forceOlder, "force-older", false, "skip a newer snapshot that fails its consistency check and use an older one")
	fl.BoolVar(&f.withoutSecrets, "without-secrets", false, "restore a snapshot without a Secrets export: modules get new keys and sealed data is unreadable")
	fl.StringArrayVar(&f.acceptUnrestored, "accept-unrestored", nil, "module id or namespace/claim volume that may stay unrestored (repeatable)")
	fl.StringArrayVar(&f.acceptUnerased, "accept-unerased", nil, "module:class whose tombstones cannot be replayed and that you will erase by hand (repeatable)")
	fl.StringArrayVar(&f.missingBlobs, "allow-missing-blobs", nil, "module (files, jobs) to restore although files its database names are missing; only right for files destroyed after the backup's dump (repeatable)")
	fl.StringVar(&f.acceptCompany, "accept-company", "", "restore a snapshot whose company.yaml has this name although the kit names another (a company renamed since the kit was made)")
	fl.StringVar(&f.repo, "repo", "", "restore from this repository instead of the kit's bucket: a local directory holding a copy of it (the ledger is read from ../ledger)")
	fl.StringVar(&f.imagesFrom, "images-from", "", "directory written by make images (verified and imported into k3s)")
	fl.StringVar(&f.releaseManifest, "release-manifest", "", "release image manifest (digest-pinned)")
	fl.StringVar(&f.coreImage, "core-image", "", "can-core image already present on this node (skips --images-from and --release-manifest)")
	fl.StringVar(&f.acme, "acme-email", "", "contact address for Let's Encrypt")
	fl.StringVar(&f.kubectl, "kubectl", "kubectl", "kubectl binary")
	fl.StringVar(&f.kubeconfig, "kubeconfig", k3sKubeconfig, "kubeconfig of this server's k3s")
	fl.StringVar(&f.kubeContext, "context", "", "kubeconfig context")
	fl.StringVar(&f.root, "root", "/", "prefix of every host path (testing)")
	fl.StringVar(&f.dumps, "dumps-dir", d.DumpsDir, "dumps directory")
	fl.StringVar(&f.hostState, "host-state-dir", d.HostStateDir, "host state directory inside the snapshot")
	fl.StringVar(&f.pvcRoot, "pvc-root", d.PVCRoot, "k3s local-path storage directory")
	fl.StringVar(&f.staging, "staging", "/var/lib/can/restore-staging", "scratch directory holding the restored snapshot (needs room for it; absolute, at least 3 levels deep, empty or created by can restore)")
	fl.StringVar(&f.state, "state", "", "progress file (default <root>/var/lib/can/restore-state.json)")
	return cmd
}

func runRestore(cmd *cobra.Command, g *globalOpts, f *restoreFlags) error {
	if f.kit == "" {
		return usage("--kit <file|-> is required")
	}
	kit, err := readKit(cmd, f.kit, f.passFile)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(f.dir)
	if err != nil {
		return failure("%v", err)
	}
	if f.state == "" {
		f.state = filepath.Join(f.root, "var/lib/can/restore-state.json")
	}
	o := &backup.Restore{
		Kit: kit, Root: f.root, DumpsDir: f.dumps, HostStateDir: f.hostState, PVCRoot: f.pvcRoot, StagingDir: f.staging,
		CompanyDir: dir, DirChosen: cmd.Flags().Changed("dir"), Source: f.repo, AcceptCompany: f.acceptCompany, ConfigDir: backup.DefaultSettings().ConfigDir, LedgerPath: backup.DefaultSettings().LedgerSource,
		StatePath: f.state, Snapshot: f.snapshot, ConfirmOverwrite: f.confirm, AllowNoLedger: f.allowNoLedger,
		AcceptLedgerBehind: f.acceptBehind, ForceOlder: f.forceOlder, WithoutSecrets: f.withoutSecrets,
		AcceptUnrestored: f.acceptUnrestored, AcceptUnerased: f.acceptUnerased, AllowMissingBlobs: f.missingBlobs, Out: cmd.ErrOrStderr(),
	}
	if f.plan {
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
	// It rewrites /var/lib/can, k3s volume directories and file ownership.
	if backupSeams.euid() != 0 {
		return failure("can restore changes this server and must run as root, on the server being restored")
	}
	images := launch.ImageSet{}
	if f.coreImage == "" {
		if images, err = launch.ResolveImages(launch.ImageOptions{FromDir: f.imagesFrom, ReleaseManifest: f.releaseManifest}); err != nil {
			return failure("%v", err)
		}
	}
	cluster := &launch.RestoreCluster{
		Kube:   launch.ExecKube{Bin: f.kubectl, Kubeconfig: f.kubeconfig, Context: f.kubeContext},
		Images: images, Loader: launch.LocalImageLoader{}, CoreImage: f.coreImage, ACMEEmail: f.acme, Out: cmd.ErrOrStderr(),
	}
	o.Cluster = backupSeams.cluster(cluster)

	store := backupSeams.store(kit)
	if f.repo != "" {
		if !backup.LocalRepository(f.repo) {
			return usage("--repo takes a local directory (an absolute path)")
		}
		store = backup.FileStoreFor(f.repo)
	}
	pw, cleanup, err := kit.PasswordFile(os.TempDir())
	if err != nil {
		return failure("%v", err)
	}
	defer cleanup()
	repo := kit.Repo(pw)
	if f.repo != "" {
		repo.Repository, repo.Env = f.repo, nil
	}
	o.Restic = &backup.Restic{R: backupSeams.runner, Repo: repo}
	o.Ledger = backup.BucketLedger{Store: store, Password: kit.Restic.Password}
	if filepath.Clean(f.root) == "/" {
		o.StopTimer = func(ctx context.Context) error {
			if _, err := exec.LookPath("systemctl"); err != nil {
				return nil // no systemd, no timer
			}
			if _, err := os.Stat("/etc/systemd/system/can-backup.timer"); errors.Is(err, os.ErrNotExist) {
				return nil
			}
			_, err := backupSeams.runner.Run(ctx, backup.Cmd{Name: "systemctl", Args: []string{"disable", "--now", "can-backup.timer"}})
			if _, serr := os.Stat("/etc/systemd/system/" + backup.ErasureTimerName); serr == nil {
				_, serr = backupSeams.runner.Run(ctx, backup.Cmd{Name: "systemctl", Args: []string{"disable", "--now", backup.ErasureTimerName}})
				err = errors.Join(err, serr)
			}
			return err
		}
	}
	if os.Geteuid() == 0 {
		o.Chown = func(dir string, uid int) error {
			return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Lchown(p, uid, uid)
			})
		}
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	if err := o.Run(ctx); err != nil {
		return mapErr(err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), `restore complete.
THIS SERVER IS NOT BEING BACKED UP YET.
Next, from a machine that reaches this server: run `+"`can up`"+` in %s (site, admin and module routes, DNS),
with the --backup-* flags to install the backup timer again (the password is already on this server).
Check that the old server is really gone: two servers backing up to one repository, or sending mail, is not supported.
Erasures: only those in the erasure ledger were replayed (people, equity, board, entity, files, finance, forms, jobs and work record theirs;
other modules do not). See the warning above and repeat any erasure it may have missed.
`, f.dir)
	if len(f.acceptUnrestored) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "NOT restored (%s): it is not in later backups, and the snapshots holding it are deleted after %d days.\n",
			strings.Join(f.acceptUnrestored, ", "), backup.DefaultSettings().RetentionDays)
	}
	return nil
}

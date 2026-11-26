package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/backup"
)

// `can backup snapshots --kit` answers the question to ask before a server is destroyed: does the
// recovery kit, and nothing else, open the backups? It uses the repository, password and bucket
// credentials in the kit only (never the installed ones or the environment), takes no lock and
// writes nothing to the bucket.

func newBackupSnapshotsCmd(g *globalOpts) *cobra.Command {
	var kitPath, passFile, restic string
	cmd := &cobra.Command{
		Use:   "snapshots",
		Short: "List the snapshots the recovery kit alone can open, and read the erasure ledger with it (changes nothing)",
		Long: "Opens the repository and the erasure ledger with what is in the kit and nothing else: not the files under\n" +
			"/etc/can, and no RESTIC_ or AWS_ variable of this shell. Run it before `can down`. It exits 1 if the kit\n" +
			"does not open them or there is no snapshot a restore would use. It does not prove that the data restores.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if kitPath == "" {
				return usage("--kit <file|-> is required")
			}
			kit, err := readKit(cmd, kitPath, passFile)
			if err != nil {
				return err
			}
			pw, cleanup, err := kit.PasswordFile(os.TempDir())
			if err != nil {
				return failure("%v", err)
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			r := &backup.Restic{R: backupSeams.runner, Repo: kit.Repo(pw), Bin: restic, KitOnly: true}
			snaps, err := r.Snapshots(ctx)
			if err != nil {
				return failure("the kit does NOT open the repository: %v", err)
			}
			entries, err := backup.FetchLedger(ctx, backupSeams.store(kit), kit.Restic.Password)
			ledger := fmt.Sprintf("%d entries", len(entries))
			switch {
			case errors.Is(err, backup.ErrNoLedger):
				ledger = "none in the bucket (right only if no erasure was ever recorded)"
			case err != nil:
				return failure("the kit does NOT read the erasure ledger: %v", err)
			}
			usable := 0
			for _, s := range snaps {
				if slices.Contains(s.Tags, "can") && !slices.Contains(s.Tags, "inconsistent") {
					usable++
				}
			}
			out := cmd.OutOrStdout()
			if g.json() {
				if err := json.NewEncoder(out).Encode(map[string]any{"snapshots": snaps, "usable": usable, "ledger": ledger}); err != nil {
					return failure("%v", err)
				}
			} else {
				for _, s := range snaps {
					fmt.Fprintf(out, "%-10s %s  %v\n", s.ShortID, s.Time.UTC().Format("2006-01-02 15:04 MST"), s.Tags)
				}
				fmt.Fprintf(out, "snapshots: %d, usable by a restore: %d\nerasure ledger: %s\n", len(snaps), usable, ledger)
			}
			if usable == 0 {
				return failure("the kit opens the repository, but it holds no snapshot a restore would use")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kitPath, "kit", "", "recovery kit file, or - for stdin")
	cmd.Flags().StringVar(&passFile, "passphrase-file", "", "passphrase file for an encrypted kit")
	cmd.Flags().StringVar(&restic, "restic", "restic", "restic binary (0.17 or newer)")
	return cmd
}

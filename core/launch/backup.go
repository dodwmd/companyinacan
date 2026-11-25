package launch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/provider"
)

// BackupInstallVersion is the generation of what the backup step installs on the host (script,
// units, tools). Raise it whenever an existing install must be redone by the next `can up`.
const BackupInstallVersion = 2

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// backup installs the backup script, unit and timer on the server (the same files `can backup
// install` writes locally) and returns the recovery kit to show. The restic password and bucket
// credentials reach the server only as files written over the Runner's stdin, never in a script
// or an argument, and never in .can/state.json.
//
// Resume: if the timer is installed but the kit was never shown (the run stopped in between), the
// kit is rebuilt from the password already on the host, so the operator is not handed a kit that
// disagrees with the repository.
func (r *runner) backup(ctx context.Context) (bool, string, error) {
	b := r.o.Backup
	if r.d.Host == nil {
		return false, "", errors.New("backup was requested but there is no server to install the timer on")
	}
	st, err := r.d.Store.Load()
	if err != nil {
		return false, "", err
	}
	s := b.Settings
	if err := r.warnBackupLimits(ctx, b.Mods, b.Settings); err != nil {
		return false, "", err
	}
	installed := st.Launch != nil && st.Launch.BackupInstalled
	shown := st.Launch != nil && st.Launch.KitShown
	pwPath := path.Join(s.ConfigDir, backup.PasswordFileName)
	// A password already on the host (an installed timer, or a run that stopped after writing it,
	// possibly after `restic init`) is the repository's: reuse it. If it cannot be read, or an
	// installed server has none, stop: a new password could not open the existing repository.
	out, err := r.d.Host.Run(ctx, fmt.Sprintf("if [ -e %[1]s ]; then cat %[1]s; fi\n", pwPath))
	if err != nil {
		return false, "", fmt.Errorf("read the restic password from the server (nothing was changed): %w", err)
	}
	in := b.Input
	in.Password = strings.TrimSpace(out)
	if !validPassword(in.Password) && (installed || in.Password != "") {
		return false, "", fmt.Errorf("the restic password file %s on the server is missing or not a password: put the one from your recovery kit back; refusing to create a new one, which could not open the repository", pwPath)
	}
	kit, err := backup.NewKit(in)
	if err != nil {
		return false, "", err
	}
	// Does the operator's kit still match what the server will use?
	changed := false
	if installed {
		old, err := r.d.Host.Run(ctx, fmt.Sprintf("if [ -e %[1]s ]; then grep -m1 '^export RESTIC_REPOSITORY=' %[1]s; fi\n", s.ScriptPath))
		if err != nil {
			return false, "", fmt.Errorf("read the installed repository from the server: %w", err)
		}
		repo := strings.Trim(strings.TrimPrefix(strings.TrimSpace(old), "export RESTIC_REPOSITORY="), "'")
		changed = strings.HasPrefix(strings.TrimSpace(old), "export RESTIC_REPOSITORY=") && repo != kit.Restic.Repository
		if st.Launch.KitFingerprint != "" && st.Launch.KitFingerprint != kitFingerprint(kit) {
			changed = true
		}
	}
	if changed && shown {
		shown = false
		if err := r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).KitShown = false; return nil }); err != nil {
			return false, "", err
		}
		r.res.Warnings = append(r.res.Warnings, "the restic password or repository differs from the recovery kit you hold: store the NEW kit; the old one only opens the old repository")
	}
	if installed && !changed && st.Launch.BackupVersion >= BackupInstallVersion {
		if err := r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).KitFingerprint = kitFingerprint(kit); return nil }); err != nil {
			return false, "", err
		}
		if shown {
			return true, "timer installed and the kit was already shown", nil
		}
		r.noteKit(kit)
		return true, "timer already installed; kit rebuilt from the server's password", nil
	}
	// Redoing an install must not hand out an unchanged, already shown kit a second time.
	r.noteKit(kit)
	if shown {
		r.res.Kit = nil
	}
	s.Repository = kit.Restic.Repository
	plan, err := backup.BuildPlan(r.o.Company, b.Mods, s)
	if err != nil {
		return false, "", err
	}
	envPath := path.Join(s.ConfigDir, backup.EnvFileName)
	// The dumps directory belongs to can-core's user (the can-dumps volume); servers bootstrapped
	// before E08b have it root-owned.
	if _, err := r.d.Host.Run(ctx, fmt.Sprintf("install -d -m 0700 %s\ninstall -d -m 0755 %s /etc/systemd/system\ninstall -d -o 65532 -g 65532 -m 0700 %s\n", shq(s.ConfigDir), shq(path.Dir(s.ScriptPath)), shq(s.DumpsDir))); err != nil {
		return false, "", err
	}
	files := []struct {
		path, body, mode string
	}{
		{s.ScriptPath, backup.GenerateScript(plan, s), "0700"},
		{"/etc/systemd/system/can-backup.service", backup.GenerateService(s), "0644"},
		{"/etc/systemd/system/can-backup.timer", backup.GenerateTimer(s), "0644"},
		{"/etc/systemd/system/can-backup-failure.service", backup.GenerateFailureService(), "0644"},
		{pwPath, kit.Restic.Password, "0600"},
		{envPath, backup.HostEnvFile(kit), "0600"},
	}
	for _, f := range files {
		if err := r.d.Host.Copy(ctx, f.path, []byte(f.body), f.mode); err != nil {
			return false, "", fmt.Errorf("install %s: %w", f.path, err)
		}
	}
	tools, err := r.d.Host.Run(ctx, backup.HostToolsScript("/usr/local/bin/restic"))
	if err != nil || strings.Contains(tools, "CAN_NO_RESTIC") {
		return false, "", fmt.Errorf("restic %s (or sqlite3) is not installed on the server and could not be installed: %v %s", backup.ResticVersion, err, strings.TrimSpace(tools))
	}
	script := fmt.Sprintf(`set -eu
set -a; . %[1]s; set +a
export RESTIC_REPOSITORY=%[2]s RESTIC_PASSWORD_FILE=%[3]s
restic cat config >/dev/null 2>&1 || restic init
systemctl daemon-reload
systemctl enable --now can-backup.timer
echo CAN_BACKUP_OK
`, shq(envPath), shq(kit.Restic.Repository), shq(pwPath))
	out, err = r.d.Host.Run(ctx, script)
	if err != nil || !strings.Contains(out, "CAN_BACKUP_OK") {
		return false, "", fmt.Errorf("enable the backup timer: %v: %s", err, strings.TrimSpace(out))
	}
	if err := r.d.Store.Mutate(func(s *provider.State) error {
		launchOf(s).BackupInstalled, launchOf(s).BackupVersion = true, BackupInstallVersion
		launchOf(s).KitFingerprint = kitFingerprint(kit)
		return nil
	}); err != nil {
		return false, "", err
	}
	return false, "", nil
}

// kitFingerprint identifies the repository and password a kit opens, without holding either
// (the password is 32 random bytes; its hash does not help guess it).
func kitFingerprint(k *backup.Kit) string {
	h := sha256.Sum256([]byte("can-kit\x00" + k.Restic.Repository + "\x00" + k.Restic.Password))
	return hex.EncodeToString(h[:])
}

// noteKit hands the kit to the result and teaches the redactor its secrets, so a later error
// message cannot echo them; the summary prints the kit through its own, unredacted writer.
func (r *runner) noteKit(k *backup.Kit) {
	r.res.Kit = k
	if r.d.Redactor != nil {
		for _, s := range []string{k.Restic.Password, k.Bucket.SecretAccessKey} {
			if s != "" {
				r.d.Redactor.Add(s)
			}
		}
	}
}

func validPassword(s string) bool {
	return len(s) >= 16 && len(s) <= 256 && !strings.ContainsAny(s, " \t\r\n")
}

// RestoreCaveat is shown by every `can up` with backups on.
const RestoreCaveat = "RESTORE IS NOT FUNCTIONAL YET: `can restore` stops at its first step. Snapshots are taken, but recovering from them is manual work with restic (docs/backup-restore.md)"

// warnBackupLimits says, on every run of a company with backups, what the backup does not do,
// and what the server's last dump run reported.
func (r *runner) warnBackupLimits(ctx context.Context, mods map[string]*config.Module, s backup.Settings) error {
	// Modules without a readable manifest cannot be judged here (the install itself requires
	// the enabled ones).
	known := *r.o.Company
	known.Modules = map[string]config.ModuleEntry{}
	for id, e := range r.o.Company.Modules {
		if mods[id] != nil {
			known.Modules[id] = e
		}
	}
	plan, err := backup.BuildPlan(&known, mods, s)
	if err != nil {
		return err
	}
	r.res.Warnings = append(r.res.Warnings, RestoreCaveat)
	r.res.Warnings = append(r.res.Warnings, plan.Warnings...)
	if r.res.BundleAdded {
		r.res.Warnings = append(r.res.Warnings, `module "forms" keeps SQLite on a PVC but has no backup hook: raw database files are backed up and may be inconsistent`)
	}
	// Modules the server still records (removed from company.yaml, say) show up in its last run.
	out, err := r.d.Host.Run(ctx, "cat "+path.Join(s.DumpsDir, backup.DumpRunName)+" 2>/dev/null || true\n")
	if err != nil {
		r.res.Warnings = append(r.res.Warnings, "could not read the server's last dump run: run `can backup status` on the server")
		return nil
	}
	run, err := backup.ParseDumpRun([]byte(out))
	if err != nil {
		return nil // no run yet
	}
	if !run.OK {
		r.res.Warnings = append(r.res.Warnings, "the server's last dump run FAILED, so nothing new is being backed up: "+run.Error)
	}
	for _, m := range run.Skipped {
		r.res.Warnings = append(r.res.Warnings, m.Message())
	}
	return nil
}

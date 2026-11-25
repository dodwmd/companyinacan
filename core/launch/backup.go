package launch

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/provider"
)

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
	if st.Launch != nil && st.Launch.BackupInstalled {
		if st.Launch.KitShown {
			return true, "timer installed and the kit was already shown", nil
		}
		out, err := r.d.Host.Run(ctx, "cat "+path.Join(s.ConfigDir, backup.PasswordFileName)+"\n")
		if err != nil {
			return false, "", fmt.Errorf("read the restic password from the server: %w", err)
		}
		in := b.Input
		in.Password = strings.TrimSpace(out)
		kit, err := backup.NewKit(in)
		if err != nil {
			return false, "", err
		}
		r.noteKit(kit)
		return true, "timer already installed; kit rebuilt from the server's password", nil
	}

	// A password file already on the host means an earlier run stopped after writing the
	// credentials (possibly after `restic init`): reuse it, a new password would lock the
	// repository that already exists.
	in := b.Input
	in.Password = ""
	if out, err := r.d.Host.Run(ctx, "cat "+path.Join(s.ConfigDir, backup.PasswordFileName)+" 2>/dev/null || true\n"); err == nil {
		if pw := strings.TrimSpace(out); validPassword(pw) {
			in.Password = pw
		}
	}
	kit, err := backup.NewKit(in)
	if err != nil {
		return false, "", err
	}
	r.noteKit(kit)
	s.Repository = kit.Restic.Repository
	plan, err := backup.BuildPlan(r.o.Company, b.Mods, s)
	if err != nil {
		return false, "", err
	}
	pwPath := path.Join(s.ConfigDir, backup.PasswordFileName)
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
		{envPath, backup.FormatEnvFile(kit.Repo("").Env), "0600"},
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
	out, err := r.d.Host.Run(ctx, script)
	if err != nil || !strings.Contains(out, "CAN_BACKUP_OK") {
		return false, "", fmt.Errorf("enable the backup timer: %v: %s", err, strings.TrimSpace(out))
	}
	if err := r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).BackupInstalled = true; return nil }); err != nil {
		return false, "", err
	}
	return false, "", nil
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

package launch

import (
	"context"
	"strings"
	"testing"
)

// The laptop's state.json says the backup is installed, but the server is a restored (or
// replaced) one without the timer: `can up` installs it, with the password already there, and
// does not hand out the unchanged kit again.
func TestUpInstallsTheTimerOnAServerThatLacksIt(t *testing.T) {
	for _, version := range []int{BackupInstallVersion, BackupInstallVersion - 1} {
		k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
		o, d, _ := newStack(t, k, &fakeLoader{})
		h := &hostFake{}
		d.Host = h
		o.Backup = backupInstall(t, o)
		if _, err := Run(context.Background(), o, d); err != nil {
			t.Fatal(err)
		}
		if err := MarkKitShown(d.Store); err != nil {
			t.Fatal(err)
		}
		pw, env := h.copied["/etc/can/backup.d/restic.password"], h.copied[hostEnv]
		// a new server: only what the restore writes back is there
		h2 := &hostFake{out: map[string]string{
			"cat /etc/can/backup.d/restic.password": string(pw),
			"cat " + hostEnv:                        string(env),
			"is-enabled --quiet can-backup.timer":   "CAN_TIMER_MISSING\n",
		}}
		d.Host = h2
		installedState(t, d, version, true)
		res, err := Run(context.Background(), o, d)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"/usr/local/sbin/can-backup", "/etc/systemd/system/can-backup.timer", "/etc/systemd/system/can-backup.service"} {
			if h2.copied[f] == nil {
				t.Errorf("v%d: %s was not installed", version, f)
			}
		}
		if !strings.Contains(strings.Join(h2.ran, "\n"), "systemctl enable --now can-backup.timer") {
			t.Errorf("v%d: the timer was not enabled", version)
		}
		if string(h2.copied["/etc/can/backup.d/restic.password"]) != string(pw) {
			t.Errorf("v%d: the repository password changed", version)
		}
		if res.Kit != nil {
			t.Errorf("v%d: the unchanged kit was handed out again", version)
		}
	}
}

// Without the --backup-* flags the missing timer is at least said, loudly.
func TestUpWithoutBackupFlagsWarnsThatTheServerHasNoTimer(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	d.Host = &hostFake{out: map[string]string{"is-enabled --quiet can-backup.timer": "CAN_TIMER_MISSING\n"}}
	installedState(t, d, BackupInstallVersion, true)
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "THIS SERVER IS NOT BEING BACKED UP") {
		t.Errorf("warnings: %v", res.Warnings)
	}
}

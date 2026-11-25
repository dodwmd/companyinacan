package launch

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/provider"
)

const ownDumps = "install -d -o 65532 -g 65532 -m 0700 '/var/lib/can/dumps'"

// QA 7: every run repairs the dumps directory's owner, with or without --backup-*.
func TestEveryUpReconcilesTheDumpsDirectory(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{}
	d.Host = h
	for i := range 2 {
		h.ran = nil
		if _, err := Run(context.Background(), o, d); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(h.ran, "\n"), ownDumps) {
			t.Fatalf("run %d did not set the dumps directory's owner: %v", i+1, h.ran)
		}
	}
}

// QA 7: an install made by an older `can` is redone (same password, no second kit).
func TestOlderBackupInstallIsRedone(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{out: map[string]string{"cat /etc/can/backup.d/restic.password": "hostpw-0123456789abcdef\n"}}
	d.Host = h
	o.Backup = backupInstall(t, o)
	if err := d.Store.Mutate(func(s *provider.State) error {
		s.Launch = &provider.LaunchState{BackupInstalled: true, KitShown: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.copied["/usr/local/sbin/can-backup"]; !ok {
		t.Fatal("the old backup script was not replaced")
	}
	if string(h.copied["/etc/can/backup.d/restic.password"]) != "hostpw-0123456789abcdef" {
		t.Fatal("the repository password changed")
	}
	if res.Kit != nil {
		t.Fatal("a kit that was shown must not be shown again")
	}
	st, _ := d.Store.Load()
	if st.Launch.BackupVersion != BackupInstallVersion {
		t.Fatalf("version %d", st.Launch.BackupVersion)
	}
	h.copied = nil
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if len(h.copied) != 0 {
		t.Fatal("a current install must be left alone")
	}
}

// QA 7: without the backup flags an outdated install cannot be redone, and the operator is told.
func TestOutdatedInstallWithoutFlagsWarns(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	d.Host = &hostFake{}
	if err := d.Store.Mutate(func(s *provider.State) error {
		s.Launch = &provider.LaunchState{BackupInstalled: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "--backup-") {
		t.Fatalf("no warning: %v", res.Warnings)
	}
}

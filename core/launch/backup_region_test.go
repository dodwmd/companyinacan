package launch

import (
	"context"
	"strings"
	"testing"
)

// A re-run without --backup-region keeps the server's region instead of rewriting it to "auto".
func TestReRunWithoutRegionKeepsTheServersRegion(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{out: map[string]string{
		"cat /etc/can/backup.d/restic.password": "hostpw-0123456789abcdef\n",
		"cat " + hostEnv:                        "AWS_ACCESS_KEY_ID='AKIAOLD'\nAWS_SECRET_ACCESS_KEY='old'\nAWS_DEFAULT_REGION='eu-west-3'\n",
	}}
	d.Host = h
	o.Backup = backupInstall(t, o)
	o.Backup.Input.Region = ""
	installedState(t, d, BackupInstallVersion, true)
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if got := string(h.copied[hostEnv]); !strings.Contains(got, "AWS_DEFAULT_REGION='eu-west-3'") {
		t.Fatalf("region rewritten: %s", got)
	}
}

// Rotated credentials that do not open the repository never replace the working restic.env.
func TestRotatedCredentialsAreVerifiedBeforeTheyReplaceTheOldOnes(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{out: map[string]string{
		"cat /etc/can/backup.d/restic.password": "hostpw-0123456789abcdef\n",
		"cat " + hostEnv:                        "AWS_ACCESS_KEY_ID='AKIAOLD'\nAWS_SECRET_ACCESS_KEY='old'\n",
		"restic cat config":                     "CAN_CREDS_BAD 1\n",
	}}
	d.Host = h
	o.Backup = backupInstall(t, o)
	installedState(t, d, BackupInstallVersion, true)
	_, err := Run(context.Background(), o, d)
	if err == nil || !strings.Contains(err.Error(), "restic.env was NOT changed") {
		t.Fatalf("err = %v", err)
	}
	if b, ok := h.copied[hostEnv]; ok {
		t.Fatalf("restic.env was overwritten: %s", b)
	}
}

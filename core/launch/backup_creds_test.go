package launch

import (
	"context"
	"strings"
	"testing"
)

const hostEnv = "/etc/can/backup.d/restic.env"

// E08b QA3 5: rotated bucket credentials reach the server on a re-run, and the kit that holds
// them is handed out again.
func TestRotatedBucketCredentialsAreWritten(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, out := newStack(t, k, &fakeLoader{})
	h := &hostFake{}
	d.Host = h
	o.Backup = backupInstall(t, o)
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if err := MarkKitShown(d.Store); err != nil {
		t.Fatal(err)
	}
	pw := string(h.copied["/etc/can/backup.d/restic.password"])
	h.out = map[string]string{"cat /etc/can/backup.d/restic.password": pw}
	o.Backup.Input.SecretAccessKey = "rotated-secret-0123456789abcdef"
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h.copied[hostEnv]), "rotated-secret-0123456789abcdef") {
		t.Fatal("the server still has the old credentials")
	}
	if res.Kit == nil || res.Kit.Bucket.SecretAccessKey != "rotated-secret-0123456789abcdef" {
		t.Fatal("the new kit was not handed out")
	}
	if strings.Contains(strings.Join(h.ran, "\n"), "rotated-secret") || strings.Contains(out.String(), "rotated-secret") {
		t.Fatal("the secret leaked into a command or the progress output")
	}
	// unchanged: nothing written, no kit
	if err := MarkKitShown(d.Store); err != nil {
		t.Fatal(err)
	}
	before := string(h.copied[hostEnv])
	delete(h.copied, "/usr/local/sbin/can-backup")
	res, err = Run(context.Background(), o, d)
	if err != nil || res.Kit != nil || string(h.copied[hostEnv]) != before || h.copied["/usr/local/sbin/can-backup"] != nil {
		t.Fatalf("%v kit=%v", err, res.Kit != nil)
	}
}

// E08b QA3 5: also for an install made before kit fingerprints existed.
func TestRotatedCredentialsOnAnOlderStateAreWritten(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	d.Host = &hostFake{out: map[string]string{
		"cat /etc/can/backup.d/restic.password": "hostpw-0123456789abcdef\n",
		"cat " + hostEnv:                        "AWS_ACCESS_KEY_ID='AKIAOLD'\nAWS_SECRET_ACCESS_KEY='old'\n",
	}}
	o.Backup = backupInstall(t, o)
	installedState(t, d, BackupInstallVersion, true)
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	h := d.Host.(*hostFake)
	if !strings.Contains(string(h.copied[hostEnv]), "AKIAEXAMPLE") || res.Kit == nil {
		t.Fatalf("env=%q kit=%v", h.copied[hostEnv], res.Kit != nil)
	}
}

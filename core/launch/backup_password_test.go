package launch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/provider"
)

const pwRead = "cat /etc/can/backup.d/restic.password"

type failingRead struct{ *hostFake }

func (f failingRead) Run(ctx context.Context, script string) (string, error) {
	if strings.Contains(script, pwRead) {
		return "", errors.New("ssh: connection reset")
	}
	return f.hostFake.Run(ctx, script)
}

func installedState(t *testing.T, d Deps, version int, shown bool) {
	t.Helper()
	if err := d.Store.Mutate(func(s *provider.State) error {
		s.Launch = &provider.LaunchState{BackupInstalled: true, BackupVersion: version, KitShown: shown}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// QA2 3: a password that cannot be read must never be replaced by a new one.
func TestUnreadablePasswordStopsTheRun(t *testing.T) {
	for name, installed := range map[string]bool{"first install": false, "older install": true} {
		t.Run(name, func(t *testing.T) {
			k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
			o, d, _ := newStack(t, k, &fakeLoader{})
			h := &hostFake{}
			d.Host = failingRead{h}
			o.Backup = backupInstall(t, o)
			if installed {
				installedState(t, d, 1, true)
			}
			_, err := Run(context.Background(), o, d)
			if err == nil || !strings.Contains(err.Error(), "password") {
				t.Fatalf("got %v", err)
			}
			if _, ok := h.copied["/etc/can/backup.d/restic.password"]; ok {
				t.Fatal("a password file was written")
			}
		})
	}
}

// QA2 3: an installed server must still have a valid password; none is minted over it.
func TestInstalledServerWithoutAPasswordIsAnError(t *testing.T) {
	for _, content := range []string{"", "short", "two words here and more text"} {
		k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
		o, d, _ := newStack(t, k, &fakeLoader{})
		h := &hostFake{out: map[string]string{pwRead: content}}
		d.Host = h
		o.Backup = backupInstall(t, o)
		installedState(t, d, 1, true)
		if _, err := Run(context.Background(), o, d); err == nil || !strings.Contains(err.Error(), "recovery kit") {
			t.Fatalf("%q: got %v", content, err)
		}
		if len(h.copied) != 0 {
			t.Fatalf("%q: files written: %v", content, keys(h.copied))
		}
	}
}

// QA2 3: the kit is handed out again when the password or the repository differs from the one
// the operator holds.
func TestKitIsShownAgainWhenPasswordOrRepositoryChanges(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{out: map[string]string{pwRead: "hostpw-0123456789abcdef\n"}}
	d.Host = h
	o.Backup = backupInstall(t, o)
	res, err := Run(context.Background(), o, d)
	if err != nil || res.Kit == nil {
		t.Fatalf("%v", err)
	}
	if err := MarkKitShown(d.Store); err != nil {
		t.Fatal(err)
	}
	run := func(what string, wantKit bool) {
		t.Helper()
		res, err := Run(context.Background(), o, d)
		if err != nil {
			t.Fatal(err)
		}
		if (res.Kit != nil) != wantKit {
			t.Fatalf("%s: kit shown=%v, want %v", what, res.Kit != nil, wantKit)
		}
		if wantKit {
			if st, _ := d.Store.Load(); st.Launch.KitShown {
				t.Fatalf("%s: the new kit is not yet shown", what)
			}
			if err := MarkKitShown(d.Store); err != nil {
				t.Fatal(err)
			}
		}
	}
	run("unchanged", false)
	h.out[pwRead] = "another-0123456789abcdef\n"
	run("password replaced on the server", true)
	run("unchanged again", false)
	o.Backup.Input.Bucket = "other-bucket"
	h.copied = nil
	run("repository changed", true)
	if !strings.Contains(string(h.copied["/usr/local/sbin/can-backup"]), "other-bucket") {
		t.Fatal("the new repository was not installed")
	}
}

// QA2 3: an install made before fingerprints is compared by the repository in its script.
func TestOlderInstallWithAnotherRepositoryShowsTheKitAgain(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	d.Host = &hostFake{out: map[string]string{pwRead: "hostpw-0123456789abcdef\n",
		"RESTIC_REPOSITORY=' /usr/local/sbin": "export RESTIC_REPOSITORY='s3:https://old.example/b/restic'\n"}}
	o.Backup = backupInstall(t, o)
	installedState(t, d, 1, true)
	res, err := Run(context.Background(), o, d)
	if err != nil || res.Kit == nil {
		t.Fatalf("kit=%v err=%v", res.Kit != nil, err)
	}
}

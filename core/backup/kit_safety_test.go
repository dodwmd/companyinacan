package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKitOptionAllowList(t *testing.T) {
	k, err := NewKit(KitInput{
		CompanyName: "A", Domain: "a.example", Endpoint: "https://e.invalid", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s",
		ProviderType:    "hetzner",
		ProviderOptions: map[string]any{"location": "fsn1", "api_key": "LEAK", "credentials": "LEAK", "hcloud_token_file": "/x", "new_option": "LEAK"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Provider.Options) != 1 || k.Provider.Options["location"] != "fsn1" {
		t.Fatalf("options = %v", k.Provider.Options)
	}
}

func TestKitReusesGivenPassword(t *testing.T) {
	k, err := NewKit(KitInput{CompanyName: "A", Domain: "a.example", Endpoint: "https://e.invalid", Bucket: "b",
		AccessKeyID: "k", SecretAccessKey: "s", ProviderType: "ssh", Password: "existing-password-0123456789"})
	if err != nil || k.Restic.Password != "existing-password-0123456789" {
		t.Fatalf("%v %v", err, k)
	}
}

func TestKitRequiresHTTPSEndpoint(t *testing.T) {
	k := testKit(t)
	k.Bucket.Endpoint = "http://s3.example.invalid"
	if k.Validate() == nil {
		t.Fatal("plain http endpoint accepted")
	}
}

func TestInsideGitRepoFollowsSymlinks(t *testing.T) {
	repo := t.TempDir()
	_ = os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	outside := t.TempDir()
	link := filepath.Join(outside, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if !insideGitRepo(filepath.Join(link, "kit.enc")) {
		t.Error("symlink into a repository not detected")
	}
	if !insideGitRepo(filepath.Join(link, "not", "yet", "there", "kit.enc")) {
		t.Error("non-existent tail under a symlinked repo not detected")
	}
	if insideGitRepo(filepath.Join(outside, "kit.enc")) {
		t.Error("false positive")
	}
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x"), 0o600)
	if !insideGitRepo(filepath.Join(wt, "kit")) {
		t.Error(".git file not detected")
	}
}

func TestPasswordFileDirIsPrivate(t *testing.T) {
	k := testKit(t)
	p, cleanup, err := k.PasswordFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Dir(p))
	if st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(p)); err == nil {
		t.Error("dir not removed")
	}
}

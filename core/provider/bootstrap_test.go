package provider

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fullOpts() BootstrapOptions {
	return BootstrapOptions{
		K3sVersion:   "v1.33.5+k3s1",
		NodeName:     "acme",
		SSHPort:      2222,
		OperatorCIDR: "198.51.100.9",
		SwapMB:       2048,
		ImageTarballs: []ImageTarball{
			{Name: "can-core.tar.zst", URL: "https://example.invalid/can-core.tar.zst", SHA256: strings.Repeat("ab", 32)},
			{Name: "traefik.tar", URL: "https://example.invalid/traefik.tar", SHA256: strings.Repeat("cd", 32)},
		},
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from generated output (run go test ./core/provider -update to refresh):\n--- got ---\n%s", name, got)
	}
}

func TestBootstrapGolden(t *testing.T) {
	def, err := Bootstrap(BootstrapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "bootstrap_default.golden.sh", def)
	full, err := Bootstrap(fullOpts())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "bootstrap_full.golden.sh", full)
}

func TestBootstrapIsDeterministic(t *testing.T) {
	a, _ := Bootstrap(fullOpts())
	b, _ := Bootstrap(fullOpts())
	if a != b {
		t.Fatal("generator is not deterministic")
	}
}

// The structural assertions guard the properties SPEC.md section 3.1 requires,
// independently of the golden file (which only detects change).
func TestBootstrapStructure(t *testing.T) {
	s, err := Bootstrap(fullOpts())
	if err != nil {
		t.Fatal(err)
	}
	must := []string{
		"set -euo pipefail",
		"INSTALL_K3S_VERSION=\"$K3S_VERSION\"", // pinned version
		"K3S_VERSION='v1.33.5+k3s1'",           // value comes from options
		"- fail-swap-on=false",                 // kubelet swap flag
		"secrets-encryption: true",             // SPEC 3.1
		"- metrics-server",                     // trimmed
		"disable-cloud-controller: true",       // trimmed
		"ufw default deny incoming",            // default-deny host firewall
		"ufw allow from '198.51.100.9/32' to any port \"$SSH_PORT\" proto tcp", // operator-restricted ssh
		"ufw allow 80/tcp",
		"ufw allow 443/tcp",
		"ufw allow from \"$POD_CIDR\"",
		"ufw allow from \"$SERVICE_CIDR\"",
		"POD_CIDR='10.42.0.0/16'",
		"SERVICE_CIDR='10.43.0.0/16'",
		"ufw allow in on cni0",
		"/var/lib/rancher/k3s/agent/images", // pre-seed dir
		"sha256sum -c",                      // tarballs verified
		"/etc/can/backup.d",                 // restic/systemd placeholder
		"/var/lib/can/bootstrap.done",       // completion marker
		"CAN_BOOTSTRAP_DONE",
		"mktemp",                // no predictable /tmp names
		"systemctl restart k3s", // changed config.yaml is re-applied
		"k3s-config.applied",
		"trap 'rm -f",
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("script missing %q", m)
		}
	}
	if strings.Contains(s, "/tmp/k3s-install.sh") {
		t.Error("predictable temp path for the installer")
	}
	// 6443 must never be opened: the CLI uses an SSH tunnel.
	if regexp.MustCompile(`ufw allow[^\n]*6443`).MatchString(s) {
		t.Error("script opens the k3s API port")
	}
	// ufw must be enabled only after every allow rule (no lockout window).
	if i, j := strings.LastIndex(s, "ufw allow"), strings.Index(s, "ufw --force enable"); i > j {
		t.Error("ufw is enabled before the last allow rule")
	}
	// Pre-seeding must overlap the k3s install: fetches start before it.
	if strings.Index(s, "fetch_image 'can-core.tar.zst'") > strings.Index(s, "step k3s\n") {
		t.Error("image fetch does not start before the k3s install")
	}
}

func TestBootstrapDefaultsHaveNoOperatorRestriction(t *testing.T) {
	s, _ := Bootstrap(BootstrapOptions{})
	if !strings.Contains(s, `ufw allow "${SSH_PORT}/tcp"`) {
		t.Error("default ssh rule missing")
	}
	if strings.Contains(s, "fetch_image '") {
		t.Error("no tarballs configured but fetch present")
	}
}

func TestBootstrapRejectsUnsafeValues(t *testing.T) {
	bad := map[string]func(*BootstrapOptions){
		"version too old":   func(o *BootstrapOptions) { o.K3sVersion = "v1.31.5+k3s1" },
		"version just old":  func(o *BootstrapOptions) { o.K3sVersion = "v1.32.1+k3s1" },
		"version unpinned":  func(o *BootstrapOptions) { o.K3sVersion = "latest" },
		"version injection": func(o *BootstrapOptions) { o.K3sVersion = "v1.33.5+k3s1'; rm -rf /; '" },
		"node injection":    func(o *BootstrapOptions) { o.NodeName = "a;b" },
		"port":              func(o *BootstrapOptions) { o.SSHPort = 70000 },
		"swap":              func(o *BootstrapOptions) { o.SwapMB = 10 },
		"pod cidr":          func(o *BootstrapOptions) { o.PodCIDR = "10.42.0.0/16; reboot" },
		"operator":          func(o *BootstrapOptions) { o.OperatorCIDR = "$(id)" },
		"tarball name": func(o *BootstrapOptions) {
			o.ImageTarballs = []ImageTarball{{"../x.tar", "https://a.b/x", strings.Repeat("a", 64)}}
		},
		"tarball http": func(o *BootstrapOptions) {
			o.ImageTarballs = []ImageTarball{{"x.tar", "http://a.b/x", strings.Repeat("a", 64)}}
		},
		"tarball url quote": func(o *BootstrapOptions) {
			o.ImageTarballs = []ImageTarball{{"x.tar", "https://a.b/x'y", strings.Repeat("a", 64)}}
		},
		"tarball sha": func(o *BootstrapOptions) { o.ImageTarballs = []ImageTarball{{"x.tar", "https://a.b/x", "nothex"}} },
		"tarball duplicate": func(o *BootstrapOptions) {
			t := ImageTarball{"x.tar", "https://a.b/x", strings.Repeat("a", 64)}
			o.ImageTarballs = []ImageTarball{t, t}
		},
	}
	for name, mut := range bad {
		o := BootstrapOptions{}
		mut(&o)
		if _, err := Bootstrap(o); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestBootstrapSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	scripts := map[string]string{"preflight": PreflightScript(), "status": StatusScript(), "uninstall": UninstallScript()}
	scripts["bootstrap-default"], _ = Bootstrap(BootstrapOptions{})
	scripts["bootstrap-full"], _ = Bootstrap(fullOpts())
	for name, s := range scripts {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: bash -n: %v\n%s", name, err, out)
		}
	}
}

func TestBootstrapShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	scripts := map[string]string{"preflight": PreflightScript(), "status": StatusScript(), "uninstall": UninstallScript()}
	scripts["bootstrap-default"], _ = Bootstrap(BootstrapOptions{})
	scripts["bootstrap-full"], _ = Bootstrap(fullOpts())
	for name, s := range scripts {
		cmd := exec.Command(sc, "--shell=bash", "--severity=warning", "-")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: shellcheck: %v\n%s", name, err, out)
		}
	}
}

func TestCloudInit(t *testing.T) {
	ci, err := CloudInit(fullOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ci, "#cloud-config\n") {
		t.Fatal("not a cloud-config document")
	}
	m := regexp.MustCompile(`content: (\S+)`).FindStringSubmatch(ci)
	if m == nil {
		t.Fatal("no content line")
	}
	packed, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ci, "encoding: gz+b64\n") {
		t.Fatal("cloud-init is not told the payload is gzipped")
	}
	want, _ := Bootstrap(fullOpts())
	if string(raw) != want {
		t.Fatal("cloud-init payload differs from the bootstrap script")
	}
	if !strings.Contains(ci, "can-bootstrap.sh") || !strings.Contains(ci, "runcmd:") {
		t.Fatal("runcmd missing")
	}
	if len(ci) > MaxCloudInitBytes {
		t.Fatalf("user data too large: %d", len(ci))
	}
	// The compressed bytes may differ between Go releases; the golden file covers the wrapper, the
	// bootstrap golden files cover the script.
	golden(t, "cloudinit_default.golden.yaml", regexp.MustCompile(`content: \S+`).ReplaceAllString(mustCloudInit(t), "content: <gzipped bootstrap script>"))
}

func mustCloudInit(t *testing.T) string {
	t.Helper()
	ci, err := CloudInit(BootstrapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ci
}

func TestCloudInitSizeLimit(t *testing.T) {
	// Distinct digests do not compress away.
	o := BootstrapOptions{}
	for i := 0; i < 600; i++ {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte{byte(i), byte(i >> 8)}))
		o.ImageTarballs = append(o.ImageTarballs, ImageTarball{
			Name: "image-" + sum[:30] + ".tar", URL: "https://example.invalid/" + sum, SHA256: sum,
		})
	}
	if _, err := CloudInit(o); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("want size error, got %v", err)
	}
}

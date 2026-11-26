package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real script with stub tools, so nothing on the host is touched. test/hostfw
// exercises it with real nftables.

type scriptHost struct {
	t   *testing.T
	dir string
}

func newScriptHost(t *testing.T) *scriptHost {
	t.Helper()
	for _, c := range []string{"bash", "awk", "comm"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip(c + " not installed")
		}
	}
	h := &scriptHost{t: t, dir: t.TempDir()}
	h.write("can-firewall", FirewallScript())
	// nft lists whatever $dir/table and $dir/chain hold and records what is loaded.
	h.stub("nft", `case "$1 $2" in
  "list table") [ -e "$D/table" ] && cat "$D/table" ;;
  "list chain") cat "$D/chain" ;;
  "list set") cat "$D/set" ;;
  "-c -f") cat >/dev/null ;;
  "-f -") cat >>"$D/loads" ;;
esac`)
	// The reference ruleset: both chains, listed the way nft lists them above.
	h.stub("unshare", `cat >/dev/null; [ ! -e "$D/no-unshare" ] && cat "$D/chain" "$D/chain"`)
	h.stub("ip", `exit 0`)
	h.stub("kubectl", `cat "$D/pods"`)
	// The sync loop sleeps a second a turn; here it makes twelve turns and is stopped.
	h.stub("sleep", `echo x >>"$D/ticks"; [ "$(wc -l <"$D/ticks")" -lt 12 ] || kill "$PPID"`)
	h.write("table", "table inet can_isolation {\n}\n")
	h.write("chain", "drop\n")
	h.write("set", "elements = { 10.42.0.10 }\n")
	h.write("pods", "kube-system||false|10.42.0.10|\n")
	return h
}

func (h *scriptHost) write(name, body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *scriptHost) stub(name, body string) {
	h.t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte("#!"+bash+"\nD="+h.dir+"\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // a test stub must be executable
		h.t.Fatal(err)
	}
}

func (h *scriptHost) run(verb string) string {
	h.t.Helper()
	cmd := exec.Command("bash", filepath.Join(h.dir, "can-firewall"), verb)
	cmd.Env = append(os.Environ(), "PATH="+h.dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CAN_FW_KUBECTL=kubectl", "CAN_FW_SYSTEMCTL=no-systemctl-here", "CAN_FW_CNI_STATE="+h.dir+"/cni", "CAN_FW_CONF_DIR="+h.dir+"/conf")
	out, _ := cmd.CombinedOutput() // the output says how it went
	return string(out)
}

// reloads counts how often the whole table was replaced.
func (h *scriptHost) reloads() int {
	b, _ := os.ReadFile(filepath.Join(h.dir, "loads"))
	return strings.Count(string(b), "delete table")
}

// QA of #53, 12: a system pod address that another pod also reports is not trusted, and that must not read as ok.
func TestFirewallCheckReportsAContestedAddress(t *testing.T) {
	h := newScriptHost(t)
	if out := h.run("check"); !strings.Contains(out, "status=ok") {
		t.Fatalf("control: %s", out)
	}
	h.write("pods", "kube-system||false|10.42.0.10|\nmod-x|app|false|10.42.0.10|\n")
	h.write("set", "")
	out := h.run("check")
	c := ParseFirewallCheck(out)
	if c.OK || !strings.Contains(c.Detail, "also reported by another pod") || !strings.Contains(out, "status=fail") {
		t.Fatalf("%+v\n%s", c, out)
	}
}

// QA of #53, 14: without a reference to compare with (no unshare) the rules are not known to be wrong, so the sync
// loop must not replace them every few seconds.
func TestFirewallLoopReloadsOnlyWhatIsKnownWrong(t *testing.T) {
	h := newScriptHost(t)
	h.write("no-unshare", "")
	out := h.run("run")
	if n := h.reloads(); n != 0 {
		t.Errorf("%d reloads of rules that could not be compared:\n%s", n, out)
	}
	if strings.Count(out, "cannot compare") != 1 {
		t.Errorf("say so, once:\n%s", out)
	}
	for _, state := range []string{"missing", "dormant", "changed"} {
		h := newScriptHost(t)
		switch state {
		case "missing":
			if err := os.Remove(filepath.Join(h.dir, "table")); err != nil {
				t.Fatal(err)
			}
		case "dormant":
			h.write("table", "table inet can_isolation {\n\tflags dormant\n}\n")
		case "changed":
			h.stub("unshare", `cat >/dev/null; echo something else`)
		}
		if out := h.run("run"); h.reloads() == 0 || !strings.Contains(out, "rules "+state) {
			t.Errorf("%s: not reloaded:\n%s", state, out)
		}
	}
}

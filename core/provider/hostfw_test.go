package provider

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Static properties of the node isolation rules (specs/038-node-isolation). Their behaviour is
// tested with real nftables by test/hostfw/netns.sh.

func TestFirewallNeverTouchesNonPodTraffic(t *testing.T) {
	s := FirewallScript()
	// Both chains: policy accept, and the first rule accepts everything not from a pod interface.
	// This is what makes locking out the operator's SSH impossible.
	for _, chain := range []string{"input", "forward"} {
		hook := regexp.MustCompile(`add chain \$t ` + chain + ` \{ type filter hook ` + chain + ` priority -10; policy accept; \}`)
		if !hook.MatchString(s) {
			t.Errorf("chain %s must be a policy-accept base chain", chain)
		}
		first := regexp.MustCompile(`(?m)^add rule \$t ` + chain + ` (.*)$`).FindStringSubmatch(s)
		if first == nil || first[1] != "iifname != { $IFACES } accept" {
			t.Errorf("first rule of %s must accept non-pod interfaces, got %v", chain, first)
		}
	}
	if !strings.Contains(s, `IFACES=${CAN_FW_IFACES:-'"cni0", "flannel.1"'}`) {
		t.Error("default pod interfaces changed")
	}
}

func TestFirewallOwnsOnlyItsTable(t *testing.T) {
	// Comments aside: never flush the ruleset or touch kube-proxy's, flannel's or ufw's tables.
	code := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(FirewallScript(), "")
	for _, bad := range []string{"flush ruleset", "iptables", "ufw ", "delete table ip"} {
		if strings.Contains(code, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
	for _, must := range []string{
		"ip daddr 169.254.0.0/16 counter drop",
		"ip saddr @trusted4 tcp dport 6443 accept",
		"ip saddr @trusted4 meta l4proto { tcp, udp } ct original proto-dst 53 accept",
		"nft -c -f -",
		"Before=k3s.service",
		"Restart=always",
	} {
		if !strings.Contains(code, must) {
			t.Errorf("script missing %q", must)
		}
	}
}

func TestBootstrapInstallsFirewall(t *testing.T) {
	s, err := Bootstrap(BootstrapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, FirewallScript()+"CAN_FIREWALL_SCRIPT\n") {
		t.Error("the bootstrap does not write the firewall script verbatim")
	}
	if strings.Count(FirewallScript(), "CAN_FIREWALL_SCRIPT") != 0 || !strings.HasSuffix(FirewallScript(), "\n") {
		t.Error("the script would break its heredoc")
	}
	order := []string{"step ready", "node did not become Ready", "step node-firewall", "/usr/local/sbin/can-firewall install", "step complete"}
	last := -1
	for _, m := range order {
		i := strings.Index(s, m)
		if i <= last {
			t.Fatalf("%q missing or out of order", m)
		}
		last = i
	}
	if !strings.Contains(UninstallScript(), "can-firewall uninstall") {
		t.Error("uninstall leaves the firewall behind")
	}
}

func TestFirewallScriptSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	for name, s := range map[string]string{"firewall": FirewallScript(), "check": FirewallCheckScript()} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: bash -n: %v\n%s", name, err, out)
		}
	}
}

func TestParseFirewallCheck(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		ok        bool
		detail    string
	}{
		{"ok", "rules=ok\nbridge=present\nservice=active\ntrusted=3\nstatus=ok\n", true, "cannot reach"},
		{"fail", "rules=missing\nproblem=the isolation rules are not loaded\nstatus=fail\n", false, "not loaded"},
		{"absent", "status=not-installed\n", false, "not installed"},
		{"empty fails closed", "", false, "no verdict"},
		{"garbage fails closed", "bash: nft: command not found\n", false, "no verdict"},
		{"ok with a problem is not ok", "problem=x\nstatus=ok\n", false, "x"},
	} {
		c := ParseFirewallCheck(tc.out)
		if c.OK != tc.ok || !strings.Contains(c.Detail, tc.detail) {
			t.Errorf("%s: %+v", tc.name, c)
		}
	}
}

func TestFirewallWarning(t *testing.T) {
	for out, want := range map[string]string{
		"status=not-installed\n":          "has no node firewall",
		"rules=missing\nstatus=fail\n":    "not enforcing",
		"rules=changed\nstatus=fail\n":    "not enforcing",
		"rules=dormant\nstatus=fail\n":    "not enforcing",
		"rules=ok\nstatus=ok\n":           "",
		"rules=ok\nstatus=fail\n":         "",
		"bash: nft: command not found\n":  "",
		"problem=rules=missing, honest\n": "",
	} {
		got := FirewallWarning(out)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q: %q", out, got)
		}
	}
}

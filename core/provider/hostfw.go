package provider

import (
	_ "embed"
	"strings"
)

// Pod-to-node isolation (specs/038-node-isolation): the host script the bootstrap installs, and
// the read-only probe `can doctor --server` runs.

//go:embed hostfw/can-firewall.sh
var firewallScript string

// FirewallScript returns /usr/local/sbin/can-firewall.
func FirewallScript() string { return firewallScript }

// FirewallCheckScript reports the isolation state of a server. It always exits 0: the verdict is
// the status= line.
func FirewallCheckScript() string {
	return `#!/usr/bin/env bash
set -u
if [ -x /usr/local/sbin/can-firewall ]; then
  /usr/local/sbin/can-firewall check || true
else
  echo "status=not-installed"
fi
`
}

// ParseFirewallCheck turns FirewallCheckScript output into one check. It fails closed: anything
// but status=ok is a failure. Only the problem lines the script itself writes are kept.
func ParseFirewallCheck(out string) Check {
	status, problems := "", []string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		switch k {
		case "status":
			status = v
		case "problem":
			if len(problems) < 10 && len(v) <= 200 {
				problems = append(problems, v)
			}
		}
	}
	c := Check{Name: "node firewall", OK: status == "ok" && len(problems) == 0}
	switch {
	case c.OK:
		c.Detail = "pods cannot reach node services"
	case status == "not-installed":
		c.Detail = "not installed: pods can reach the node's services (docs/node-isolation.md)"
	case len(problems) > 0:
		c.Detail = strings.Join(problems, "; ")
	default:
		c.Detail = "the check gave no verdict"
	}
	return c
}

// FirewallWarning turns FirewallCheckScript output into the warning `can up` and `can status`
// print when the server definitely has no isolation in force, or "". Everything else (a stopped
// service, a stale address) is left to `can doctor --server`. Only fixed text is returned.
func FirewallWarning(out string) string {
	for _, l := range strings.Split(out, "\n") {
		switch strings.TrimSpace(l) {
		case "status=not-installed":
			return "this server has no node firewall: pods can reach the node's services; install it as described in docs/node-isolation.md"
		case "rules=missing", "rules=changed", "rules=dormant":
			return "this server's node firewall is not enforcing: pods can reach the node's services; run `can doctor --server` and see docs/node-isolation.md"
		}
	}
	return ""
}

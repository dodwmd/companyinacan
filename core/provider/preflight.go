package provider

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Preflight thresholds. SPEC.md targets a 2 GB server; a "2 GB" VM reports
// somewhat less than 2048 MB, so the floor is lower.
const (
	MinMemMB  = 1700
	MinDiskMB = 10 * 1024
)

// RequiredFreePorts must be free before the bootstrap installs k3s/Traefik.
var RequiredFreePorts = []int{80, 443, K3sAPIPort}

// PreflightScript returns a read-only probe whose output ParsePreflight reads.
func PreflightScript() string {
	return `#!/usr/bin/env bash
set -u
echo "uid=$(id -u)"
echo "bootstrapped=$(cat /var/lib/can/bootstrap.done 2>/dev/null || true)"
. /etc/os-release 2>/dev/null || true
echo "os_id=${ID:-unknown}"
echo "os_version=${VERSION_ID:-unknown}"
echo "arch=$(uname -m)"
echo "mem_mb=$(awk '/^MemTotal:/ {print int($2/1024)}' /proc/meminfo)"
echo "disk_free_mb=$(df -Pm /var/lib 2>/dev/null | awk 'NR==2 {print $4}')"
if [ -d /run/systemd/system ]; then echo "systemd=yes"; else echo "systemd=no"; fi
for p in 80 443 6443; do
  if (ss -Hltn 2>/dev/null || true) | awk '{print $4}' | grep -Eq "[:.]${p}$"; then echo "port_${p}=busy"; else echo "port_${p}=free"; fi
done
`
}

func parseKV(out string) map[string]string {
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// BootstrappedVersion returns the k3s version recorded by a finished
// bootstrap in probe output, or "".
func BootstrappedVersion(out string) string { return parseKV(out)["bootstrapped"] }

var supportedOS = map[string]bool{"ubuntu": true, "debian": true}

// ParsePreflight turns probe output into checks. A check with OK=false blocks
// the bootstrap.
func ParsePreflight(out string) []Check {
	kv := parseKV(out)
	var cs []Check
	add := func(name string, ok bool, detail string) { cs = append(cs, Check{name, ok, detail}) }

	add("root", kv["uid"] == "0", "uid "+kv["uid"]+": need root or passwordless sudo")

	osID, osVer := kv["os_id"], kv["os_version"]
	add("os", supportedOS[osID], fmt.Sprintf("%s %s (supported: Ubuntu, Debian)", osID, osVer))

	add("arch", kv["arch"] == "x86_64" || kv["arch"] == "aarch64", kv["arch"])

	mem, err := strconv.Atoi(kv["mem_mb"])
	add("ram", err == nil && mem >= MinMemMB, fmt.Sprintf("%d MB (need >= %d)", mem, MinMemMB))

	disk, err := strconv.Atoi(kv["disk_free_mb"])
	add("disk", err == nil && disk >= MinDiskMB, fmt.Sprintf("%d MB free (need >= %d)", disk, MinDiskMB))

	add("systemd", kv["systemd"] == "yes", kv["systemd"])

	for _, p := range RequiredFreePorts {
		v := kv["port_"+strconv.Itoa(p)]
		// on a re-run k3s itself holds these ports
		add(fmt.Sprintf("port %d", p), v == "free" || kv["bootstrapped"] != "", v)
	}
	return cs
}

// PreflightOK reports whether every check passed.
func PreflightOK(cs []Check) bool {
	for _, c := range cs {
		if !c.OK {
			return false
		}
	}
	return true
}

// PreflightError describes the failed checks, or returns nil.
func PreflightError(cs []Check) error {
	var bad []string
	for _, c := range cs {
		if !c.OK {
			bad = append(bad, fmt.Sprintf("%s: %s", c.Name, c.Detail))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("preflight failed: %s", strings.Join(bad, "; "))
}

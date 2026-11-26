package verify

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/provider"
)

// ServerScript is the read-only fact collector, fed to `bash -s` over ssh. It writes nothing,
// restarts nothing and reads no Secret value: only `key=value` lines that EvalServer reads. It is
// POSIX sh so that CI can also run it inside a k3d node.
func ServerScript() string { return serverScript }

const serverScript = `#!/usr/bin/env bash
# can verify: read-only facts
set -u
R=''
k() { k3s kubectl "$@" 2>/dev/null; }
have() { command -v "$1" >/dev/null 2>&1; }
kv() { printf '%s=%s\n' "$1" "$2"; }
mtime() { stat -c %Y "$1" 2>/dev/null || true; }

kv now "$(date +%s)"
kv boot "$(awk '/^btime / {print $2}' /proc/stat)"
kv bootstrapped "$(cat "$R/var/lib/can/bootstrap.done" 2>/dev/null || true)"
kv bootstrap_time "$(mtime "$R/var/lib/can/bootstrap.done")"
kv k3s_version "$(k3s --version 2>/dev/null | awk 'NR==1 {print $3}')"
if have systemctl; then kv k3s_active "$(systemctl is-active k3s 2>/dev/null || true)"; fi
kv nodes "$(k get nodes --no-headers | awk 'END {print NR+0}')"
kv nodes_ready "$(k get nodes --no-headers | awk '$2=="Ready" {n++} END {print n+0}')"

awk '/^MemTotal:/ {t=$2} /^MemAvailable:/ {a=$2} /^SwapTotal:/ {st=$2} /^SwapFree:/ {sf=$2}
  END {printf "mem_total_mb=%d\nmem_available_mb=%d\nswap_total_mb=%d\nswap_used_mb=%d\n", t/1024, a/1024, st/1024, (st-sf)/1024}' /proc/meminfo
kv k3s_rss_mb "$(ps -eo rss=,comm= 2>/dev/null | awk '$2 ~ /^k3s/ {s+=$1} END {printf "%d", s/1024}')"
if have journalctl; then kv oom_kernel "$(journalctl -k -b --no-pager 2>/dev/null | grep -c 'Out of memory')"; fi
kv disk_used_pct "$(df -P "$R/var/lib" 2>/dev/null | awk 'NR==2 {sub("%","",$5); print $5}')"
if have timedatectl; then kv ntp_synced "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)"; fi

if have ufw; then
  st=$(ufw status 2>/dev/null)
  case "$st" in *"Status: active"*) kv ufw active ;; *) kv ufw inactive ;; esac
  printf '%s\n' "$st" | awk '/ALLOW( IN)? +Anywhere/ {print "ufw_open=" $1}'
else
  kv ufw absent
fi

kv secrets_encryption "$(k3s secrets-encrypt status 2>/dev/null | sed -n 's/^Encryption Status: //p')"
kv traefik_policy "$(k -n kube-system get svc traefik -o jsonpath='{.spec.externalTrafficPolicy}')"
kv traefik_pvc "$(k -n kube-system get pvc traefik -o jsonpath='{.status.phase}')"
acme=$(find "$R/var/lib/rancher/k3s/storage" -maxdepth 2 -path '*_kube-system_traefik*' -name acme.json 2>/dev/null | head -n 1)
if [ -n "$acme" ]; then
  kv acme_bytes "$(stat -c %s "$acme")"
  kv acme_mode "$(stat -c %a "$acme")"
  kv acme_certs "$(grep -o '"certificate"' "$acme" 2>/dev/null | wc -l | tr -d ' ')"
fi
kv acme_errors "$(k -n kube-system logs deploy/traefik --since=24h | grep -ci 'unable to obtain acme certificate')"

k get deploy,statefulset -A -o jsonpath='{range .items[*]}{"workload="}{.metadata.namespace}{"/"}{.metadata.name}{" "}{.spec.replicas}{" "}{.status.readyReplicas}{"\n"}{end}'
k get pods -A -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{"container="}{.restartCount}{" "}{.lastState.terminated.reason}{"\n"}{end}{end}'
k get ingressroutes.traefik.io -A -o jsonpath='{range .items[*]}{"route="}{.metadata.namespace}{"/"}{.metadata.name}{"|"}{.spec.entryPoints}{"|"}{range .spec.routes[*]}{.match}{" "}{end}{"\n"}{end}'
kv login_limit "$(k -n can-system get middlewares.traefik.io can-login-rate -o name)"
k -n can-system get secrets -o name | sed 's|^secret/|system_secret=|'

if have systemctl; then
  kv timer_enabled "$(systemctl is-enabled can-backup.timer 2>/dev/null || true)"
  kv timer_active "$(systemctl is-active can-backup.timer 2>/dev/null || true)"
  kv backup_result "$(systemctl show can-backup.service -p Result --value 2>/dev/null || true)"
  kv backup_exit "$(systemctl show can-backup.service -p ExecMainStatus --value 2>/dev/null || true)"
fi
kv backup_finished "$(sed -n 's/^finished_at=//p' "$R/var/lib/can/backup-status" 2>/dev/null | head -n 1)"
kv marker_finished "$(sed -n 's/^finished_at=//p' "$R/var/lib/can/dumps/COMPLETE" 2>/dev/null | head -n 1)"
if [ -f "$R/var/lib/can/dumps/LAST-RUN" ]; then
  if grep -q '"ok": *true' "$R/var/lib/can/dumps/LAST-RUN"; then kv last_dump ok; else kv last_dump failed; fi
fi
for f in restic.env restic.password; do
  if [ -f "$R/etc/can/backup.d/$f" ]; then kv "mode_$f" "$(stat -c '%a:%u' "$R/etc/can/backup.d/$f")"; fi
done
kv end ok
`

// Facts are the parsed `key=value` lines. Repeated keys keep every value.
type Facts map[string][]string

// ParseFacts reads ServerScript output.
func ParseFacts(out string) Facts {
	f := Facts{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			f[k] = append(f[k], strings.TrimSpace(v))
		}
	}
	return f
}

func (f Facts) str(k string) string {
	if v := f[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (f Facts) num(k string) (int64, bool) {
	n, err := strconv.ParseInt(f.str(k), 10, 64)
	return n, err == nil
}

// Route is one routed hostname.
type Route struct {
	Namespace, Host string
	TLS             bool
}

var (
	reHost     = regexp.MustCompile("Host\\(`([a-z0-9]([a-z0-9.-]*[a-z0-9])?)`\\)")
	reWorkload = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9.-]+$`)
)

// Routes are the hostnames Traefik is told to serve, each once (TLS if any of its routes has it).
func (f Facts) Routes() []Route {
	byHost := map[string]Route{}
	var hosts []string
	for _, l := range f["route"] {
		p := strings.SplitN(l, "|", 3)
		if len(p) != 3 {
			continue
		}
		ns, _, _ := strings.Cut(p[0], "/")
		tls := strings.Contains(p[1], "websecure")
		for _, m := range reHost.FindAllStringSubmatch(p[2], -1) {
			r, seen := byHost[m[1]]
			if !seen {
				hosts = append(hosts, m[1])
				r = Route{Namespace: ns, Host: m[1]}
			}
			r.TLS = r.TLS || tls
			byHost[m[1]] = r
		}
	}
	slices.Sort(hosts)
	out := make([]Route, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, byHost[h])
	}
	return out
}

// ServerOptions is what the company directory says should be on the server.
type ServerOptions struct {
	K3sVersion string // expected; "" = provider.DefaultK3sVersion
	// BackupInstalled is what `can up` recorded.
	BackupInstalled bool
	// RelaySecret is true when company.yaml has email.relay, SSOSecret names the SSO client secret.
	EmailRelay bool
	SSOSecret  string
}

const (
	minAvailablePct = 10
	maxDiskUsedPct  = 85
	maxBackupAge    = 26 * time.Hour
)

// CollectServer runs the two read-only scripts and judges them. A server that cannot be asked
// fails; the output of a failed command is not repeated.
func CollectServer(ctx context.Context, r provider.Runner, o ServerOptions) ([]Check, Facts) {
	out, err := r.Run(ctx, ServerScript())
	f := ParseFacts(out)
	if err != nil || f.str("end") != "ok" {
		return []Check{fail("server.ssh", "the server could not be checked over ssh (try `ssh` to it yourself; root or passwordless sudo is needed)")}, Facts{}
	}
	checks := []Check{pass("server.ssh", "facts collected")}
	fw, err := r.Run(ctx, provider.FirewallCheckScript())
	var nodeFW Check
	if err != nil {
		nodeFW = fail("firewall.node-isolation", "the check could not be run")
	} else {
		c := provider.ParseFirewallCheck(fw)
		nodeFW = verdict("firewall.node-isolation", c.OK, c.Detail, c.Detail)
	}
	return append(append(checks, EvalServer(f, o, nodeFW.Status == Pass)...), nodeFW), f
}

// EvalServer judges the facts.
func EvalServer(f Facts, o ServerOptions, firewallOK bool) []Check {
	var cs []Check
	add := func(c ...Check) { cs = append(cs, c...) }

	want := o.K3sVersion
	if want == "" {
		want = provider.DefaultK3sVersion
	}
	switch v := f.str("bootstrapped"); {
	case v == "":
		add(fail("server.bootstrap", "the bootstrap never finished (/var/log/can-bootstrap.log, or run `can up` again)"))
	case f.str("k3s_version") != want:
		add(fail("server.bootstrap", fmt.Sprintf("k3s is %q, expected %q", f.str("k3s_version"), want)))
	default:
		add(pass("server.bootstrap", "bootstrapped, k3s "+want))
	}
	nodes, _ := f.num("nodes")
	ready, _ := f.num("nodes_ready")
	active := f.str("k3s_active")
	add(verdict("server.k3s", nodes > 0 && ready == nodes && (active == "" || active == "active"),
		fmt.Sprintf("%d of %d nodes ready", ready, nodes), fmt.Sprintf("%d of %d nodes ready, service %q", ready, nodes, active)))

	total, ok1 := f.num("mem_total_mb")
	avail, ok2 := f.num("mem_available_mb")
	if ok1 && ok2 && total > 0 {
		swapT, _ := f.num("swap_total_mb")
		swapU, _ := f.num("swap_used_mb")
		rss, _ := f.num("k3s_rss_mb")
		up := int64(0)
		if now, ok := f.num("now"); ok {
			if boot, ok := f.num("boot"); ok {
				up = (now - boot) / 60
			}
		}
		d := fmt.Sprintf("total %d MB, available %d MB, swap used %d of %d MB, k3s resident %d MB, up %d min", total, avail, swapU, swapT, rss, up)
		add(verdict("server.memory", avail*100 >= total*minAvailablePct, d, d+fmt.Sprintf("; less than %d%% is available", minAvailablePct)))
	} else {
		add(skip("server.memory", "could not read /proc/meminfo"))
	}

	oomK, _ := f.num("oom_kernel")
	var killed, restarts int64
	for _, c := range f["container"] {
		p := strings.Fields(c)
		if len(p) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(p[0], 10, 64)
		restarts += n
		if len(p) > 1 && p[1] == "OOMKilled" {
			killed++
		}
	}
	d := fmt.Sprintf("%d kernel OOM events this boot, %d containers last ended by OOM, %d restarts in all", oomK, killed, restarts)
	add(verdict("server.oom", oomK == 0 && killed == 0, d, d))

	if used, ok := f.num("disk_used_pct"); ok {
		d := fmt.Sprintf("/var/lib is %d%% full", used)
		add(verdict("server.disk", used <= maxDiskUsedPct, d, d))
	} else {
		add(skip("server.disk", "could not read disk usage"))
	}
	switch f.str("ntp_synced") {
	case "yes":
		add(pass("server.clock", "clock is synchronised"))
	case "no":
		add(fail("server.clock", "clock is not synchronised: TOTP sign-in, certificates and object storage signatures depend on it"))
	default:
		add(skip("server.clock", "timedatectl is not available"))
	}

	boot, ok1 := f.num("boot")
	done, ok2 := f.num("bootstrap_time")
	switch {
	case !ok1 || !ok2:
		add(skip("server.reboot", "no bootstrap record"))
	case boot <= done:
		add(skip("server.reboot", "not rebooted since the bootstrap: reboot once and run again (docs/real-server-verification.md)"))
	default:
		add(verdict("server.reboot", firewallOK && ready == nodes && nodes > 0,
			"rebooted since the bootstrap; the node is ready and isolation is enforced",
			"rebooted since the bootstrap, and the node is not ready or isolation is not enforced"))
	}

	switch f.str("ufw") {
	case "active":
		open := map[string]bool{}
		for _, to := range f["ufw_open"] {
			open[strings.TrimSuffix(to, "/tcp")] = true
		}
		var bad []string
		for _, p := range []string{"80", "443"} {
			if !open[p] {
				bad = append(bad, p+" is not allowed")
			}
		}
		for _, p := range []string{"6443", "10250"} {
			if open[p] {
				bad = append(bad, p+" is open to everyone")
			}
		}
		add(verdict("firewall.ufw", len(bad) == 0, "active; 80 and 443 allowed, 6443 and 10250 not opened to everyone", strings.Join(bad, "; ")))
	case "inactive":
		add(fail("firewall.ufw", "ufw is installed but not active"))
	default:
		add(skip("firewall.ufw", "ufw is not installed: check your own host or cloud firewall"))
	}

	add(verdict("cluster.secrets-encryption", f.str("secrets_encryption") == "Enabled", "k3s encrypts Secrets at rest", "k3s secrets encryption is not enabled"))
	add(evalWorkloads(f))

	add(verdict("edge.client-address-policy", f.str("traefik_policy") == "Local",
		"Traefik's Service has externalTrafficPolicy Local", fmt.Sprintf("Traefik's Service has externalTrafficPolicy %q: rate limits and lockouts would not see client addresses", f.str("traefik_policy"))))

	add(evalACME(f)...)
	add(evalBackup(f, o)...)

	if o.EmailRelay {
		add(verdict("email.relay-secret", slices.Contains(f["system_secret"], "can-smtp-relay"),
			"Secret can-smtp-relay exists", "company.yaml has email.relay but Secret can-system/can-smtp-relay is missing (docs/email.md)"))
	} else {
		add(skip("email.relay-secret", "no email.relay in company.yaml"))
	}
	if o.SSOSecret != "" {
		add(verdict("sso.client-secret", slices.Contains(f["system_secret"], o.SSOSecret),
			"the SSO client Secret exists", "sso is enabled but its client Secret is missing from can-system (docs/sso.md)"))
	} else {
		add(skip("sso.client-secret", "sso is not enabled"))
	}
	return cs
}

func evalWorkloads(f Facts) Check {
	var seen, down int
	var names []string
	core := false
	for _, w := range f["workload"] {
		p := strings.Fields(w)
		if len(p) < 2 || !reWorkload.MatchString(p[0]) {
			continue
		}
		ns := path.Dir(p[0])
		if ns != "kube-system" && ns != "can-system" && ns != "can-data" && !strings.HasPrefix(ns, "mod-") {
			continue
		}
		want, _ := strconv.Atoi(p[1])
		have := 0
		if len(p) > 2 {
			have, _ = strconv.Atoi(p[2])
		}
		seen++
		core = core || p[0] == "can-system/can-core"
		if have < want {
			down++
			if len(names) < 10 {
				names = append(names, p[0])
			}
		}
	}
	switch {
	case !core:
		return fail("cluster.workloads", "can-core is not deployed or the cluster could not be queried")
	case down > 0:
		return fail("cluster.workloads", fmt.Sprintf("%d of %d workloads are not ready: %s", down, seen, strings.Join(names, ", ")))
	}
	return pass("cluster.workloads", fmt.Sprintf("%d workloads ready", seen))
}

func evalACME(f Facts) []Check {
	tls := slices.ContainsFunc(f.Routes(), func(r Route) bool { return r.TLS })
	if !tls {
		const why = "no route uses TLS (plain-HTTP fallback or nothing routed)"
		return []Check{skip("tls.acme-persistence", why), skip("tls.acme-errors", why)}
	}
	size, _ := f.num("acme_bytes")
	certs, _ := f.num("acme_certs")
	var cs []Check
	switch {
	case f.str("traefik_pvc") != "Bound":
		cs = append(cs, fail("tls.acme-persistence", "Traefik has no bound volume: a restart would request every certificate again"))
	case size == 0 || certs == 0:
		cs = append(cs, fail("tls.acme-persistence", "acme.json on Traefik's volume holds no certificate"))
	case f.str("acme_mode") != "600":
		cs = append(cs, fail("tls.acme-persistence", "acme.json has mode "+f.str("acme_mode")+", want 600"))
	default:
		cs = append(cs, pass("tls.acme-persistence", fmt.Sprintf("acme.json on the volume holds %d certificates", certs)))
	}
	n, _ := f.num("acme_errors")
	return append(cs, verdict("tls.acme-errors", n == 0, "no certificate errors in Traefik's log for 24 h",
		fmt.Sprintf("%d certificate errors in Traefik's log in 24 h (kubectl -n kube-system logs deploy/traefik)", n)))
}

func evalBackup(f Facts, o ServerOptions) []Check {
	ids := []string{"backup.timer", "backup.dump", "backup.last-run", "backup.credential-files"}
	enabled := f.str("timer_enabled")
	if !o.BackupInstalled && enabled != "enabled" {
		var cs []Check
		for _, id := range ids {
			cs = append(cs, skip(id, "backups were not set up (`can up --backup-*`): the server holds the only copy of your data"))
		}
		return cs
	}
	now, _ := f.num("now")
	age := func(k string) (time.Duration, bool) {
		t, ok := f.num(k)
		return time.Duration(now-t) * time.Second, ok
	}
	cs := []Check{verdict("backup.timer", enabled == "enabled" && f.str("timer_active") == "active",
		"can-backup.timer is enabled and active", fmt.Sprintf("can-backup.timer is %q and %q", enabled, f.str("timer_active")))}

	if a, ok := age("marker_finished"); !ok {
		cs = append(cs, fail("backup.dump", "no completed dump run (the first is about 10 minutes after can-core starts)"))
	} else {
		d := fmt.Sprintf("last completed dump run %d h ago, last run %s", int(a.Hours()), orUnknown(f.str("last_dump")))
		cs = append(cs, verdict("backup.dump", a <= maxBackupAge && f.str("last_dump") != "failed", d, d))
	}

	result, code := f.str("backup_result"), f.str("backup_exit")
	if a, ok := age("backup_finished"); !ok {
		cs = append(cs, fail("backup.last-run", "no backup has completed (journalctl -u can-backup)"))
	} else {
		d := fmt.Sprintf("last completed backup %d h ago, last service result %s (exit %s)", int(a.Hours()), orUnknown(result), orUnknown(code))
		cs = append(cs, verdict("backup.last-run", a <= maxBackupAge && (result == "" || result == "success"), d, d))
	}

	env, pw := f.str("mode_restic.env"), f.str("mode_restic.password")
	cs = append(cs, verdict("backup.credential-files", env == "600:0" && pw == "600:0",
		"restic credential files are root-only", "restic.env or restic.password is missing, or not mode 600 and owned by root"))
	return cs
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// healthy is a host where every server check passes.
const healthy = `now=1800000000
boot=1799990000
bootstrapped=v1.33.5+k3s1
bootstrap_time=1799000000
k3s_version=v1.33.5+k3s1
k3s_active=active
nodes=1
nodes_ready=1
mem_total_mb=1960
mem_available_mb=600
swap_total_mb=1024
swap_used_mb=10
k3s_rss_mb=450
oom_kernel=0
disk_used_pct=30
ntp_synced=yes
ufw=active
ufw_open=22/tcp
ufw_open=80/tcp
ufw_open=443/tcp
secrets_encryption=Enabled
traefik_policy=Local
traefik_pvc=Bound
acme_bytes=9000
acme_mode=600
acme_certs=3
acme_errors=0
workload=can-system/can-core 1 1
workload=mod-forms/forms 1 1
container=0
route=can-system/site|["websecure"]|Host(` + "`acme.example`" + `)
route=can-system/admin|["websecure"]|Host(` + "`admin.acme.example`" + `)
login_limit=middleware.traefik.io/can-login-rate
system_secret=can-smtp-relay
system_secret=sso-client
timer_enabled=enabled
timer_active=active
backup_result=success
backup_exit=0
backup_finished=1799996000
marker_finished=1799995000
last_dump=ok
mode_restic.env=600:0
mode_restic.password=600:0
end=ok
`

var allOn = ServerOptions{BackupInstalled: true, EmailRelay: true, SSOSecret: "sso-client"}

func byID(cs []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range cs {
		m[c.ID] = c
	}
	return m
}

func TestEvalHealthy(t *testing.T) {
	for _, c := range EvalServer(ParseFacts(healthy), allOn, true) {
		if c.Status != Pass {
			t.Errorf("%s: %s %s", c.ID, c.Status, c.Detail)
		}
	}
}

func TestEvalFailures(t *testing.T) {
	for _, tc := range []struct{ from, to, id string }{
		{"bootstrapped=v1.33.5+k3s1", "bootstrapped=", "server.bootstrap"},
		{"\nk3s_version=v1.33.5+k3s1", "\nk3s_version=v1.30.0+k3s1", "server.bootstrap"},
		{"nodes_ready=1", "nodes_ready=0", "server.k3s"},
		{"mem_available_mb=600", "mem_available_mb=100", "server.memory"},
		{"oom_kernel=0", "oom_kernel=2", "server.oom"},
		{"container=0", "container=3 OOMKilled", "server.oom"},
		{"disk_used_pct=30", "disk_used_pct=91", "server.disk"},
		{"ntp_synced=yes", "ntp_synced=no", "server.clock"},
		{"ufw=active", "ufw=inactive", "firewall.ufw"},
		{"ufw_open=80/tcp", "ufw_open=6443/tcp", "firewall.ufw"},
		{"secrets_encryption=Enabled", "secrets_encryption=Disabled", "cluster.secrets-encryption"},
		{"workload=mod-forms/forms 1 1", "workload=mod-forms/forms 1 ", "cluster.workloads"},
		{"workload=can-system/can-core 1 1\n", "", "cluster.workloads"},
		{"traefik_policy=Local", "traefik_policy=Cluster", "edge.client-address-policy"},
		{"traefik_pvc=Bound", "traefik_pvc=", "tls.acme-persistence"},
		{"acme_certs=3", "acme_certs=0", "tls.acme-persistence"},
		{"acme_mode=600", "acme_mode=644", "tls.acme-persistence"},
		{"acme_errors=0", "acme_errors=4", "tls.acme-errors"},
		{"timer_active=active", "timer_active=inactive", "backup.timer"},
		{"marker_finished=1799995000", "marker_finished=1799000000", "backup.dump"},
		{"last_dump=ok", "last_dump=failed", "backup.dump"},
		{"backup_finished=1799996000", "backup_finished=", "backup.last-run"},
		{"backup_result=success", "backup_result=exit-code", "backup.last-run"},
		{"mode_restic.password=600:0", "mode_restic.password=644:0", "backup.credential-files"},
		{"system_secret=can-smtp-relay", "system_secret=x", "email.relay-secret"},
		{"system_secret=sso-client", "system_secret=x", "sso.client-secret"},
	} {
		in := strings.Replace(healthy, tc.from, tc.to, 1)
		if in == healthy {
			t.Fatalf("%q not in the fixture", tc.from)
		}
		if c := byID(EvalServer(ParseFacts(in), allOn, true))[tc.id]; c.Status != Fail {
			t.Errorf("%s -> %s: want %s to fail, got %s %q", tc.from, tc.to, tc.id, c.Status, c.Detail)
		}
	}
}

func TestEvalSkips(t *testing.T) {
	in := healthy
	for _, drop := range []string{"timer_enabled=enabled", "ufw=active", "ntp_synced=yes"} {
		in = strings.Replace(in, drop+"\n", "", 1)
	}
	in = strings.ReplaceAll(in, "websecure", "web")
	got := byID(EvalServer(ParseFacts(in), ServerOptions{}, true))
	for _, id := range []string{"server.clock", "firewall.ufw", "tls.acme-persistence", "tls.acme-errors", "backup.timer", "backup.last-run", "email.relay-secret", "sso.client-secret"} {
		if got[id].Status != Skip {
			t.Errorf("%s: %s %q", id, got[id].Status, got[id].Detail)
		}
	}
	// A timer `can up` recorded and the server no longer has is a failure, not a skip.
	if c := byID(EvalServer(ParseFacts(in), ServerOptions{BackupInstalled: true}, true))["backup.timer"]; c.Status != Fail {
		t.Errorf("missing timer: %s", c.Status)
	}
}

func TestEvalReboot(t *testing.T) {
	notYet := strings.Replace(healthy, "boot=1799990000", "boot=1798000000", 1)
	if c := byID(EvalServer(ParseFacts(notYet), allOn, true))["server.reboot"]; c.Status != Skip {
		t.Errorf("not rebooted: %s", c.Status)
	}
	if c := byID(EvalServer(ParseFacts(healthy), allOn, false))["server.reboot"]; c.Status != Fail {
		t.Errorf("rebooted without isolation: %s", c.Status)
	}
}

type scripted struct {
	reply func(script string) (string, error)
	ran   []string
}

func (s *scripted) Run(_ context.Context, script string) (string, error) {
	s.ran = append(s.ran, script)
	return s.reply(script)
}
func (s *scripted) Copy(context.Context, string, []byte, string) error {
	return errors.New("verify must never copy to the server")
}

func TestCollectServer(t *testing.T) {
	r := &scripted{reply: func(s string) (string, error) {
		if strings.Contains(s, "can-firewall check") {
			return "status=ok\n", nil
		}
		return healthy, nil
	}}
	cs, f := CollectServer(context.Background(), r, allOn)
	if got := byID(cs); got["server.ssh"].Status != Pass || got["firewall.node-isolation"].Status != Pass || len(f.Routes()) != 2 || len(r.ran) != 2 {
		t.Fatalf("%+v", cs)
	}
	for _, bad := range []func(string) (string, error){
		func(string) (string, error) { return "password=hunter22 at 203.0.113.7", errors.New("exit 255") },
		func(string) (string, error) { return "now=1\n", nil }, // cut off before the end
	} {
		cs, f := CollectServer(context.Background(), &scripted{reply: bad}, allOn)
		if len(cs) != 1 || cs[0].Status != Fail || len(f) != 0 || strings.Contains(cs[0].Detail, "hunter22") {
			t.Fatalf("%+v", cs)
		}
	}
}

// The script is run for real against stand-ins for the host's tools and files.
func TestServerScriptOnAFakeHost(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	write := func(p, body string, mode os.FileMode) {
		t.Helper()
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	fake := func(name, body string) { write("bin/"+name, "#!/bin/sh\n"+body+"\n", 0o700) }
	fake("k3s", `
case "$*" in
  --version) echo "k3s version v1.33.5+k3s1 (abc)" ;;
  "secrets-encrypt status") echo "Encryption Status: Enabled" ;;
  *"get nodes"*) echo "can Ready control-plane 1d v1.33.5" ;;
  *"get svc traefik"*) echo Local ;;
  *"get pvc traefik"*) echo Bound ;;
  *"get deploy,statefulset"*) echo "workload=can-system/can-core 1 1" ;;
  *"get pods"*) echo "container=0 " ;;
  *"get ingressroutes"*) echo 'route=can-system/site|["websecure"]|Host(`+"`acme.example`"+`) ' ;;
  *"get middlewares"*) echo middleware.traefik.io/can-login-rate ;;
  *"get secrets"*) echo secret/can-smtp-relay ;;
  *logs*) echo "level=error msg=\"Unable to obtain ACME certificate for domains\"" ;;
esac`)
	fake("systemctl", `case "$*" in
  "is-active k3s"|"is-active can-backup.timer") echo active ;;
  "is-enabled can-backup.timer") echo enabled ;;
  *Result*) echo success ;;
  *ExecMainStatus*) echo 0 ;;
esac`)
	fake("ufw", `printf 'Status: active\n\nTo Action From\n-- ------ ----\n22/tcp ALLOW 198.51.100.9\n80/tcp ALLOW Anywhere\n443/tcp ALLOW IN Anywhere\nAnywhere ALLOW 10.42.0.0/16\n80/tcp (v6) ALLOW Anywhere (v6)\n'`)
	fake("timedatectl", "echo yes")
	fake("journalctl", "echo nothing")
	write("var/lib/can/bootstrap.done", "v1.33.5+k3s1\n", 0o600)
	write("var/lib/can/backup-status", "run_id=x\nfinished_at=1799996000\n", 0o600)
	write("var/lib/can/dumps/COMPLETE", "run_id=x\nfinished_at=1799995000\n", 0o600)
	write("var/lib/can/dumps/LAST-RUN", `{"run_id":"x","ok":true}`, 0o600)
	write("var/lib/rancher/k3s/storage/pvc-1_kube-system_traefik/acme.json", "{\n \"certificate\": \"AAAA\",\n \"key\": \"SECRETKEY\"\n}\n", 0o600)
	write("etc/can/backup.d/restic.env", "AWS_SECRET_ACCESS_KEY='SECRETKEY'\n", 0o600)
	write("etc/can/backup.d/restic.password", "SECRETKEY", 0o600)

	script := strings.Replace(ServerScript(), "\nR=''\n", "\nR='"+root+"'\n", 1)
	cmd := exec.Command(bash, "-s") //nolint:gosec // test
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(string(out), "SECRETKEY") || strings.Contains(string(out), "198.51.100.9") {
		t.Fatalf("the facts carry a secret or the operator's address:\n%s", out)
	}
	f := ParseFacts(string(out))
	for k, want := range map[string]string{
		"end": "ok", "bootstrapped": "v1.33.5+k3s1", "k3s_version": "v1.33.5+k3s1", "k3s_active": "active", "nodes": "1", "nodes_ready": "1",
		"ufw": "active", "secrets_encryption": "Enabled", "traefik_policy": "Local", "acme_certs": "1", "acme_mode": "600", "acme_errors": "1",
		"timer_enabled": "enabled", "backup_result": "success", "backup_finished": "1799996000", "marker_finished": "1799995000",
		"last_dump": "ok", "oom_kernel": "0", "ntp_synced": "yes",
	} {
		if f.str(k) != want {
			t.Errorf("%s = %q, want %q", k, f.str(k), want)
		}
	}
	if got := strings.Join(f["ufw_open"], ","); got != "80/tcp,443/tcp,80/tcp" {
		t.Errorf("ufw_open = %q", got)
	}
	if !strings.HasPrefix(f.str("mode_restic.env"), "600:") || len(f.Routes()) != 1 || f.str("mem_total_mb") == "" || f.str("boot") == "" {
		t.Errorf("facts: %v", f)
	}
}

// Everything the scripts run must be read-only.
func TestScriptsChangeNothing(t *testing.T) {
	write := regexp.MustCompile(`(?m)(^|[;&|(]\s*)(rm|mv|cp|tee|install|mkdir|touch|chmod|chown|apt-get|reboot|kill)\s|systemctl\s+(start|stop|restart|enable|disable)|kubectl[^\n]*\s(apply|delete|patch|create|scale|edit)\s|\bk\s[^\n]*\s(apply|delete|patch|create|scale)\s|restic\s+(forget|prune|backup|restore|unlock|init)|ufw\s+(allow|deny|enable|disable|delete)|>\s*/(etc|var|usr)`)
	for name, s := range map[string]string{"server": ServerScript(), "repository": backupRepoScript} {
		if m := write.FindString(s); m != "" {
			t.Errorf("%s script: %q", name, m)
		}
	}
	if sc, err := exec.LookPath("shellcheck"); err == nil {
		for name, s := range map[string]string{"server": ServerScript(), "repository": backupRepoScript} {
			cmd := exec.Command(sc, "--severity=warning", "-s", "bash", "-") //nolint:gosec // test
			cmd.Stdin = strings.NewReader(s)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: %v\n%s", name, err, out)
			}
		}
	}
}

func TestScrubber(t *testing.T) {
	r := &Report{Checks: []Check{{ID: "a", Status: Fail, Detail: "admin.acme.example missing at 203.0.113.7 and 2001:db8::1, 203-0-113-7.sslip.io, bob@x.org, https://u:p@h/x Acme Co"},
		{ID: "b", Status: Pass, Detail: "k3s v1.33.5+k3s1, 2 h, 6443 closed"}}}
	r.Finish(NewScrubber("acme.example", "203.0.113.7", "Acme Co", ""))
	for _, bad := range []string{"acme.example", "203.0.113.7", "203-0-113-7", "2001:db8", "bob@", "u:p", "Acme"} {
		if strings.Contains(r.Checks[0].Detail, bad) {
			t.Errorf("%q survived: %s", bad, r.Checks[0].Detail)
		}
	}
	if r.Checks[1].Detail != "k3s v1.33.5+k3s1, 2 h, 6443 closed" || r.Summary != (Summary{Pass: 1, Fail: 1}) {
		t.Errorf("%q %+v", r.Checks[1].Detail, r.Summary)
	}
}

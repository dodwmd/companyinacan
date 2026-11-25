package provision

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodConfig = `
listen: ":8090"
db_path: /data/provision.db
directory: { url: "http://people-api.mod-people:8081", allow_insecure_http: true }
tasks: { joiner_template: tpl_join, leaver_template: tpl_leave }
workflows:
  privileged_groups: [admins]
  delete_after_days: 30
  timezone: Australia/Sydney
retry: { max_attempts: 4, base_delay: 10s, max_delay: 10m, step_timeout: 20s }
scim: { max_scan: 2000 }
connectors:
  - { name: wiki, type: scim-client, url: "https://wiki.example.com/scim/v2", token_env: WIKI_TOKEN, default_groups: [all-staff] }
  - { name: hooks, type: webhook, url: "https://hooks.example.com/provision", secret_env: HOOKS_SECRET, privileged: true }
  - { name: gws, type: google-workspace }
`

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(goodConfig))
	if err != nil {
		t.Fatal(err)
	}
	pol, err := cfg.Policy()
	if err != nil {
		t.Fatal(err)
	}
	if pol.MaxAttempts != 4 || pol.BaseDelay != 10*time.Second || pol.MaxDelay != 10*time.Minute || pol.StepTimeout != 20*time.Second || pol.TZ.String() != "Australia/Sydney" || pol.DeleteAfterDays != 30 {
		t.Fatalf("policy %+v", pol)
	}
	env := map[string]string{"WIKI_TOKEN": "wiki-token-123", "HOOKS_SECRET": "hooks-secret-0123456789"}
	cs, err := BuildConnectors(cfg, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cs.Names(), ","); got != "wiki,hooks,gws" {
		t.Fatalf("connectors %s", got)
	}
	if c, _ := cs.Get("gws"); c.Verified() {
		t.Error("a stub reports itself verified")
	}
	// secrets are required: a connector never starts without its credential
	if _, err := BuildConnectors(cfg, func(string) string { return "" }); err == nil {
		t.Fatal("connectors built without credentials")
	}
	if _, err := BuildConnectors(cfg, func(k string) string {
		return map[string]string{"WIKI_TOKEN": "x", "HOOKS_SECRET": env["HOOKS_SECRET"]}[k]
	}); err == nil {
		t.Fatal("a one-character token was accepted")
	}
}

func TestParseConfigRejects(t *testing.T) {
	base := func(mut string) string {
		return strings.Replace(goodConfig, "scim: { max_scan: 2000 }", "scim: { max_scan: 2000 }\n"+mut, 1)
	}
	cases := map[string]string{
		"unknown key":          base("bogus: 1"),
		"no directory":         strings.Replace(goodConfig, `directory: { url: "http://people-api.mod-people:8081", allow_insecure_http: true }`, "", 1),
		"plain http directory": strings.Replace(goodConfig, "allow_insecure_http: true", "allow_insecure_http: false", 1),
		"bad timezone":         strings.Replace(goodConfig, "Australia/Sydney", "Mars/Olympus", 1),
		"attempts":             strings.Replace(goodConfig, "max_attempts: 4", "max_attempts: 99", 1),
		"delay":                strings.Replace(goodConfig, "base_delay: 10s", "base_delay: 0s", 1),
		"scan":                 strings.Replace(goodConfig, "max_scan: 2000", "max_scan: 9999999", 1),
		"delete days":          strings.Replace(goodConfig, "delete_after_days: 30", "delete_after_days: -1", 1),
		"stub with url":        strings.Replace(goodConfig, "{ name: gws, type: google-workspace }", "{ name: gws, type: google-workspace, url: \"https://x.example.com\" }", 1),
		"unknown type":         strings.Replace(goodConfig, "type: webhook", "type: ldap", 1),
		"reserved name":        strings.Replace(goodConfig, "name: hooks", "name: can-core", 1),
		"duplicate":            strings.Replace(goodConfig, "name: hooks", "name: wiki", 1),
		"http connector":       strings.Replace(goodConfig, "https://wiki.example.com", "http://wiki.example.com", 1),
		"private connector":    strings.Replace(goodConfig, "https://wiki.example.com", "https://169.254.169.254", 1),
		"no token env":         strings.Replace(goodConfig, "token_env: WIKI_TOKEN, ", "", 1),
		"bad group":            strings.Replace(goodConfig, "[all-staff]", "[\"a\\nb\"]", 1),
		"bad privileged":       strings.Replace(goodConfig, "[admins]", "[\"\"]", 1),
	}
	for name, in := range cases {
		if _, err := ParseConfig([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMainCommands(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(cfg, []byte(goodConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WIKI_TOKEN": "wiki-token-123", "HOOKS_SECRET": "hooks-secret-0123456789",
		"CAN_PROVISIONING_API_TOKEN": strings.Repeat("a", 32), "CAN_PROVISIONING_DIRECTORY_TOKEN": "directory-token"}
	getenv := func(k string) string { return env[k] }
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(args, getenv, &out, &errb)
		return code, out.String(), errb.String()
	}
	if c, out, _ := run("version"); c != 0 || !strings.Contains(out, "can-provision") {
		t.Errorf("version %d %q", c, out)
	}
	if c, _, _ := run("bogus"); c != 2 {
		t.Errorf("unknown command %d", c)
	}
	if c, _, e := run("check-config", "-config", cfg); c != 0 {
		t.Errorf("check-config %d %s", c, e)
	}
	if c, _, _ := run("check-config", "-config", filepath.Join(dir, "missing.yaml")); c != 2 {
		t.Errorf("missing config %d", c)
	}
	for _, drop := range []string{"CAN_PROVISIONING_API_TOKEN", "CAN_PROVISIONING_DIRECTORY_TOKEN", "WIKI_TOKEN"} {
		saved := env[drop]
		env[drop] = ""
		if c, _, e := run("check-config", "-config", cfg); c != 2 || strings.Contains(e, saved) {
			t.Errorf("without %s: %d %q", drop, c, e)
		}
		env[drop] = saved
	}
	env["CAN_PROVISIONING_API_TOKEN"] = "short"
	if c, _, _ := run("check-config", "-config", cfg); c != 2 {
		t.Errorf("short api token %d", c)
	}
	env["CAN_PROVISIONING_API_TOKEN"] = strings.Repeat("a", 32)
	// verify-audit works on a fresh database and nothing in any output contains a secret
	dbcfg := strings.Replace(goodConfig, "/data/provision.db", filepath.Join(dir, "p.db"), 1)
	if err := os.WriteFile(cfg, []byte(dbcfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, out, e := run("verify-audit", "-config", cfg); c != 0 || !strings.Contains(out, "ok") {
		t.Errorf("verify-audit %d %s %s", c, out, e)
	}
	if c, _, _ := run("healthcheck", "-addr", "127.0.0.1:1"); c != 1 {
		t.Errorf("healthcheck of nothing %d", c)
	}
}

// State is in SQLite: a restart with a new process picks the run up where the file says.
func TestRunSurvivesRestartThroughTheDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	h := newHarness(t, nil)
	st1, err := OpenStore(path, h.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	h.st = st1
	h.eng = h.newEngine()
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.a.failures[opCreateAccount] = 1 // the first attempt fails: the run is mid-flight at "shutdown"
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	h.tick()
	_ = st1.Close()

	st2, err := OpenStore(path, h.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	h.st = st2
	h.eng = h.newEngine()
	h.clk.add(time.Minute)
	h.settle()
	runs, _ := st2.ListRuns(context.Background(), RunFilter{})
	if len(runs) != 1 || runs[0].State != RunSucceeded {
		t.Fatalf("runs %+v", runs)
	}
	if h.a.count("create_account") != 2 {
		t.Fatalf("create_account calls %d", h.a.count("create_account"))
	}
	if err := st2.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	// tampering with the audit trail is detected
	if _, err := st2.db.Exec(`UPDATE audit SET action='nothing happened' WHERE seq=1`); err != nil {
		t.Fatal(err)
	}
	if err := st2.VerifyAudit(context.Background()); err == nil {
		t.Fatal("a tampered audit trail verified")
	}
}

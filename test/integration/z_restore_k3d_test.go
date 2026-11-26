//go:build integration

package integration

// specs/042-restore: backup, destroy the cluster, restore, check. Two k3d clusters stand in for
// the old and the new server. The backup runs the installed script on the CI runner against a
// copy of the old node's directories (as z_backup_k3d_test.go does), with a local restic
// repository in place of the bucket. The restore runs `can restore` INSIDE the new node, as root,
// against that node's own k3s and local-path volumes, which is how it runs on a server.
//
// It deletes the cluster and writes under /var/lib on the machine running it: throwaway CI only.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/server/auth"
)

const (
	restoreCompany = `schema: 1
name: Restore Co
domain: restore.example
provider:
  type: local
modules:
  forms: {}
  people: {}
  work: {}
  stub:
    settings:
      greeting: hello-before-the-disaster
`
	e2eRoot   = "/var/lib/can-e2e" // the "bucket": restic/ and ledger/
	e2eRepo   = e2eRoot + "/restic"
	keptName  = "Keptperson"
	goneName  = "Erasedperson"
	nationalA = "NID-KEPT-4471"
	nationalB = "NID-GONE-9902"
)

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // test helper
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func moduleToken(t *testing.T, ns, key string) string {
	t.Helper()
	return mustKubectl(t, "-n", ns, "get", "secret", "can-module", "-o", fmt.Sprintf("go-template={{index .data %q | base64decode}}", key))
}

// rcall is an HTTP request against a port-forwarded service.
func rcall(t *testing.T, method, u, token string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req) //nolint:gosec // loopback
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitModules(t *testing.T) {
	t.Helper()
	for _, m := range []string{"forms", "people", "stub", "work"} {
		eventually(t, 6*time.Minute, "module "+m+" to run", func() (bool, string) {
			out, err := kubectl("-n", "mod-"+m, "rollout", "status", "deploy/"+m, "--timeout=5s")
			if err != nil {
				logs, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=15")
				return false, out + "\n" + logs
			}
			want, _ := kubectl("-n", "mod-"+m, "get", "deploy", m, "-o", "jsonpath={.status.readyReplicas}")
			return want == "1", "ready replicas " + want
		})
	}
}

func volumeDir(t *testing.T, ns, claim string) string {
	t.Helper()
	pv := mustKubectl(t, "-n", ns, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
	return mustKubectl(t, "get", "pv", pv, "-o", "jsonpath={.spec.hostPath.path}{.spec.local.path}")
}

func peopleClaim(t *testing.T) string {
	t.Helper()
	return volumeDir(t, "mod-people", "people-data")
}

func importImages(t *testing.T) {
	t.Helper()
	cluster := strings.TrimPrefix(kubeContext(), "k3d-")
	for _, img := range strings.Fields(os.Getenv("CAN_IT_RESTORE_IMAGES")) {
		run(t, "k3d", "image", "import", img, "-c", cluster)
	}
}

type adminLogin struct{ password, totp string }

func (a adminLogin) attempt(t *testing.T, base string) (int, string) {
	t.Helper()
	key, err := auth.DecodeTOTPSecret(a.totp)
	if err != nil {
		t.Fatal(err)
	}
	return rcall(t, "POST", base+"/api/v1/login", "", map[string]string{"username": "admin", "password": a.password, "totp": auth.Code(key, auth.SHA1, 6, 30, time.Now())})
}

type seeded struct {
	admin                       adminLogin
	keptID, goneID              string
	peopleToken, formsReadToken string
	secretKeyring               string
	workToken                   string
}

// seed deploys the company on the current cluster and creates the data.
func seed(t *testing.T) seeded {
	t.Helper()
	var s seeded
	importImages(t)
	prepareDumpsDir(t) // host bootstrap's part

	doc, ps := config.LoadBytes("company.yaml", []byte(restoreCompany))
	if doc == nil {
		t.Fatal(ps)
	}
	co, err := config.DecodeCompany(doc)
	if err != nil {
		t.Fatal(err)
	}
	cm, err := launch.CompanyConfigMap(restoreCompany)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := launch.CoreManifests(launch.CoreOptions{Image: "can-core:dev", CompanyName: co.Name, Domain: co.Domain})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{cm, string(manifests)} {
		if out, err := kubectlStdin(m, "apply", "-f", "-"); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
	}
	// The nightly dump run deletes the completion marker: keep it away from the minutes in which the
	// directories are copied.
	mustKubectl(t, "-n", "can-system", "set", "env", "deploy/can-core", "CAN_DUMP_AT="+time.Now().UTC().Add(12*time.Hour).Format("15:04"))
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")

	s.admin.password = "correct horse battery staple 42"
	out, err := kubectlStdin(s.admin.password+"\n", "-n", "can-system", "exec", "-i", "deploy/can-core", "--", "/can-core", "admin", "bootstrap")
	if err != nil {
		t.Fatalf("admin bootstrap: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`TOTP secret \(shown once\): ([A-Z2-7]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no TOTP secret in %q", out)
	}
	s.admin.totp = m[1]
	waitModules(t)

	// --- people: two records, one with an encrypted field each ---
	s.peopleToken = moduleToken(t, "mod-people", "CAN_PEOPLE_API_TOKEN")
	s.secretKeyring = moduleToken(t, "mod-people", "keyring")
	api := portForward(t, "mod-people", "people-api", 8081)
	create := func(family, national string) string {
		st, body := rcall(t, "POST", api+"/v1/people", s.peopleToken, map[string]string{"kind": "employee", "status": "candidate", "given_name": "Robin", "family_name": family})
		if st != 201 {
			t.Fatalf("create person: %d %s", st, body)
		}
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(body), &p); err != nil || p.ID == "" {
			t.Fatalf("person: %v %s", err, body)
		}
		if st, body := rcall(t, "PUT", api+"/v1/people/"+p.ID+"/sensitive", s.peopleToken, map[string]string{"national_id": national}); st != 204 {
			t.Fatalf("sensitive: %d %s", st, body)
		}
		return p.ID
	}
	s.keptID, s.goneID = create(keptName, nationalA), create(goneName, nationalB)

	// --- work: a project with one item ---
	s.workToken = moduleToken(t, "mod-work", "CAN_WORK_ADMIN_TOKEN")
	work := portForward(t, "mod-work", "work", 8080)
	if st, body := rcall(t, "POST", work+"/v1/projects", s.workToken, map[string]string{"key": "DR", "name": "Disaster recovery"}); st != 201 {
		t.Fatalf("work project: %d %s", st, body)
	}
	if st, body := rcall(t, "POST", work+"/v1/projects/DR/issues", s.workToken, map[string]string{"type": "task", "title": "item kept across the restore"}); st != 201 {
		t.Fatalf("work item: %d %s", st, body)
	}

	// --- forms: a lead ---
	forms := portForward(t, "mod-forms", "forms", 8080)
	st, body := rcall(t, "GET", forms+"/contact/token", "", nil)
	var tok struct {
		Token string `json:"token"`
	}
	if st != 200 || json.Unmarshal([]byte(body), &tok) != nil {
		t.Fatalf("form token: %d %s", st, body)
	}
	time.Sleep(4 * time.Second) // min_fill_seconds
	form := url.Values{"name": {"Lead Before"}, "email": {"lead@restore.example"}, "message": {"kept across the restore"}, "_t": {tok.Token}}
	resp, err := http.PostForm(forms+"/contact", form) //nolint:noctx // loopback
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("form post: %d", resp.StatusCode)
	}
	s.formsReadToken = moduleToken(t, "mod-forms", "CAN_FORMS_API_READ_TOKEN")

	// --- stub: a dump (which binds its volume), then a raw file on that volume ---
	requestDump(t)
	onNode(t, "echo raw-volume-data > "+volumeDir(t, "mod-stub", "data")+"/raw.txt")
	return s
}

// backupHost runs the installed backup script against the old node's directories.
func backupHost(t *testing.T) (password []byte, can string) {
	t.Helper()
	set := backup.DefaultSettings()
	set.Repository = e2eRepo
	if _, err := os.Stat(set.StateDir); err == nil {
		t.Fatalf("%s already exists on this machine: refusing to overwrite it", set.StateDir)
	}
	mustSudo(t, "sh", "-c", backup.HostToolsScript("/usr/local/bin/restic"))

	can = filepath.Join(t.TempDir(), "can")
	build := exec.Command("go", "build", "-trimpath", "-o", can, "./cmd/can") //nolint:gosec // test helper
	build.Dir, build.Env = repoRoot(t), append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	mustSudo(t, "install", "-m", "0755", can, "/usr/local/bin/can")

	copyCmd := fmt.Sprintf("docker exec %s tar -C / -cf - var/lib/can/dumps var/lib/rancher/k3s/storage var/lib/rancher/k3s/server/db var/lib/rancher/k3s/server/token var/lib/rancher/k3s/server/cred | tar -C / -xpf -", nodeName())
	mustSudo(t, "bash", "-o", "pipefail", "-c", "install -d -m 0700 /var/lib/can && "+copyCmd)

	co := &config.Company{}
	plan, err := backup.BuildPlan(co, nil, set)
	if err != nil {
		t.Fatal(err)
	}
	password = []byte("e2e-restic-password-0123456789abcdef")
	pw := set.ConfigDir + "/" + backup.PasswordFileName
	sudoWrite(t, set.ScriptPath, "0700", []byte(backup.GenerateScript(plan, set)))
	sudoWrite(t, pw, "0600", password)
	sudoWrite(t, set.ConfigDir+"/"+backup.EnvFileName, "0600", []byte("RESTIC_REPOSITORY='"+e2eRepo+"'\n"))
	mustSudo(t, "install", "-d", "-m", "0700", e2eRoot)
	mustSudo(t, "/usr/local/bin/restic", "-r", e2eRepo, "--password-file", pw, "init")
	t.Log(mustSudo(t, set.ScriptPath))
	t.Log(mustSudo(t, can, "backup", "status", "--repo", e2eRepo))
	t.Log(mustSudo(t, can, "backup", "verify", "--repo", e2eRepo))
	return password, can
}

// moveToNewNode copies what a new server would have (can, restic, the "bucket", the kit) into the
// new cluster's node.
func moveToNewNode(t *testing.T, can string, password []byte) {
	t.Helper()
	kit, err := backup.NewKit(backup.KitInput{Password: string(password), CompanyName: "Restore Co", Domain: "restore.example",
		Endpoint: "https://s3.example.invalid", Bucket: "e2e", AccessKeyID: "unused", SecretAccessKey: "unused-secret", ProviderType: "local"})
	if err != nil {
		t.Fatal(err)
	}
	kitFile := filepath.Join(t.TempDir(), "kit.txt")
	if err := os.WriteFile(kitFile, []byte(kit.Text()), 0o600); err != nil {
		t.Fatal(err)
	}
	onNode(t, "mkdir -p /tmp /root /usr/local/bin /var/lib")
	for src, dst := range map[string]string{can: "/usr/local/bin/can", "/usr/local/bin/restic": "/usr/local/bin/restic", kitFile: "/root/kit.txt"} {
		run(t, "docker", "cp", src, nodeName()+":"+dst)
	}
	onNode(t, "chmod 0755 /usr/local/bin/can /usr/local/bin/restic")
	mustSudo(t, "bash", "-o", "pipefail", "-c", fmt.Sprintf("tar -C /var/lib -cf - can-e2e | docker exec -i %s tar -C /var/lib -xf -", nodeName()))
}

// canRestore runs `can restore` as root inside the new node.
func canRestore(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"exec", nodeName(), "/usr/local/bin/can", "restore", "--kit", "/root/kit.txt", "--repo", e2eRepo,
		"--core-image", "can-core:dev", "--dir", "/root/company"}, extra...)
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // test helper
	t.Logf("can restore %s:\n%s", strings.Join(extra, " "), out)
	return string(out), err
}

func dumpState(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{
		{"get", "pods,deploy,pvc,jobs", "-A"},
		{"-n", "can-system", "logs", "deploy/can-core", "--tail=120"},
		{"get", "events", "-A", "--field-selector", "type=Warning"},
	} {
		out, _ := kubectl(args...)
		t.Log(out)
	}
}

func TestRestoreOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_RESTORE") != "1" {
		t.Skip("CAN_IT_RESTORE=1 not set (destroys the k3d cluster; throwaway CI runner only)")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("CAN_IT_RESTORE=1 writes under /var/lib and /etc as root: only on a throwaway CI runner")
	}
	t.Cleanup(func() {
		if t.Failed() {
			dumpState(t)
		}
	})
	s := seed(t)

	// --- back up ------------------------------------------------------------------------------
	// (the stub has a backup and a restore hook, so `can backup verify` below accepts the set)
	requestDump(t)
	onNode(t, "test -s /var/lib/can/dumps/can-core/secrets.json && test -s /var/lib/can/dumps/modules/stub/probe.tar.gz")
	// The in-house modules are dumped by their own binaries; their live SQLite files are then not
	// in the snapshot, so what comes back below came back through their restore subcommands.
	for _, m := range []string{"forms", "people", "work"} {
		onNode(t, "test -s /var/lib/can/dumps/modules/"+m+"/database.sqlite3.gz && test -s /var/lib/can/dumps/modules/"+m+"/manifest.json")
	}
	password, can := backupHost(t)

	for _, l := range strings.Split(mustSudo(t, "/usr/local/bin/restic", "-r", e2eRepo, "--password-file", backup.DefaultSettings().ConfigDir+"/"+backup.PasswordFileName, "ls", "latest"), "\n") {
		for _, db := range []string{"_mod-forms_forms-data/forms.db", "_mod-people_people-data/people.db", "_mod-work_work-data/work.db"} {
			if strings.Contains(l, db) {
				t.Errorf("a live database file is in the snapshot: %s", l)
			}
		}
	}

	// --- AFTER the backup, one person is erased. Nobody records it: can-core collects the
	// tombstones from the module, and the installed script's ledger run takes them to the bucket ---
	api := portForward(t, "mod-people", "people-api", 8081)
	if st, body := rcall(t, "POST", api+"/v1/people/"+s.goneID+"/erasure", s.peopleToken, nil); st != 200 {
		t.Fatalf("erasure: %d %s", st, body)
	}
	ledger := backup.DefaultSettings().LedgerSource
	collected := backup.DefaultSettings().DumpsDir + "/" + backup.CollectedDir
	eventually(t, 4*time.Minute, "can-core to collect the erasure", func() (bool, string) {
		out := onNode(t, "cat "+collected+"/"+backup.CollectedLedger+" 2>/dev/null || true")
		for _, class := range people.Classes {
			if !strings.Contains(out, `"subject":"`+s.goneID+`","class":"`+class+`"`) {
				return false, out
			}
		}
		return true, out
	})
	// the runner stands in for the host: take over what can-core wrote and run what the timer runs
	mustSudo(t, "bash", "-o", "pipefail", "-c", fmt.Sprintf("docker exec %s tar -C / -cf - %s | tar -C / -xpf -", nodeName(), strings.TrimPrefix(collected, "/")))
	t.Log(mustSudo(t, backup.DefaultSettings().ScriptPath, backup.LedgerOnlyArg))
	if got := mustSudo(t, "cat", ledger); !strings.Contains(got, s.goneID) {
		t.Fatalf("the erasure is not in the host ledger: %q", got)
	}
	// A class nothing can replay: the restore must refuse to go live until it is acknowledged.
	mustSudo(t, can, "backup", "ledger-add", "--ledger", ledger, "--subject", "lead_000", "--class", "lead-submission")
	mustSudo(t, can, "backup", "ledger-sync", "--ledger", ledger, "--repo", e2eRepo)

	// --- disaster ------------------------------------------------------------------------------
	run(t, "k3d", "cluster", "delete", strings.TrimPrefix(kubeContext(), "k3d-"))
	started := time.Now()
	run(t, "go", "run", "./cmd/can", "dev", "up")
	importImages(t)
	moveToNewNode(t, can, password)
	t.Logf("new cluster ready after %s", time.Since(started).Round(time.Second))

	// --- restore: stops, fail closed, at the tombstone it cannot replay -------------------------
	restoreStart := time.Now()
	out, err := canRestore(t)
	if err == nil || !strings.Contains(out, "forms:lead-submission") || !strings.Contains(out, "step 7 (erasure)") {
		t.Fatalf("the restore must stop at the erasure step: %v", err)
	}
	if got := mustKubectl(t, "-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "restore", "status"); !strings.HasPrefix(got, "restoring ") {
		t.Fatalf("can-core status: %q", got)
	}
	core := portForward(t, "can-system", "can-core", 8080)
	if st, body := s.admin.attempt(t, core); st != 503 {
		t.Fatalf("no logins while restoring: %d %s", st, body)
	}
	if got := mustKubectl(t, "-n", "mod-people", "get", "deploy", "people", "-o", "jsonpath={.spec.replicas}"); got != "0" {
		t.Fatalf("a module must not run before the erasures are applied: replicas %q", got)
	}
	// A normal manifest re-apply must not end restore mode.
	run(t, "kubectl", "--context", kubeContext(), "-n", "can-system", "set", "env", "deploy/can-core", "CAN_RESTORE-")
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	if got := mustKubectl(t, "-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "restore", "status"); !strings.HasPrefix(got, "restoring ") {
		t.Fatalf("restore mode ended without `restore complete`: %q", got)
	}

	// --- resume ---------------------------------------------------------------------------------
	firstRun := out // the hooks ran in the run that stopped at the erasure step
	out, err = canRestore(t, "--accept-unerased", "forms:lead-submission")
	if err != nil {
		t.Fatalf("resumed restore: %v", err)
	}
	for _, want := range []string{"already done", "restore complete", "erased: people: directory-basic " + s.goneID} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	t.Logf("restore (both runs, including the manual checks between them) took %s", time.Since(restoreStart).Round(time.Second))
	waitModules(t)

	// --- the company is back ----------------------------------------------------------------------
	if got := mustKubectl(t, "-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "restore", "status"); !strings.HasPrefix(got, "normal restored ") {
		t.Fatalf("status after the restore: %q", got)
	}
	core = portForward(t, "can-system", "can-core", 8080)
	if st, body := s.admin.attempt(t, core); st != 200 {
		t.Fatalf("the original admin password and TOTP seed must work: %d %s", st, body)
	}
	if got := onNode(t, "cat /root/company/company.yaml"); got != strings.TrimSpace(restoreCompany) {
		t.Errorf("company.yaml written back: %q", got)
	}
	if moduleToken(t, "mod-people", "keyring") != s.secretKeyring || moduleToken(t, "mod-people", "CAN_PEOPLE_API_TOKEN") != s.peopleToken ||
		moduleToken(t, "mod-forms", "CAN_FORMS_API_READ_TOKEN") != s.formsReadToken {
		t.Error("module Secrets were regenerated instead of restored")
	}

	api = portForward(t, "mod-people", "people-api", 8081)
	st, body := rcall(t, "GET", api+"/v1/people/"+s.keptID, s.peopleToken, nil)
	if st != 200 || !strings.Contains(body, keptName) {
		t.Fatalf("the kept person: %d %s", st, body)
	}
	st, body = rcall(t, "GET", api+"/v1/people/"+s.keptID+"/sensitive?fields=national_id", s.peopleToken, nil)
	if st != 200 || !strings.Contains(body, nationalA) {
		t.Fatalf("the sealed field must decrypt with the restored keyring: %d %s", st, body)
	}
	_, body = rcall(t, "GET", api+"/v1/people/"+s.goneID, s.peopleToken, nil)
	if strings.Contains(body, goneName) {
		t.Fatalf("THE ERASED PERSON IS BACK: %s", body)
	}
	if _, body = rcall(t, "GET", api+"/v1/people/"+s.goneID+"/sensitive?fields=national_id", s.peopleToken, nil); strings.Contains(body, nationalB) {
		t.Fatalf("THE ERASED PERSON'S SENSITIVE DATA IS BACK: %s", body)
	}
	if _, body = rcall(t, "GET", api+"/v1/people", s.peopleToken, nil); strings.Contains(body, goneName) || !strings.Contains(body, keptName) {
		t.Fatalf("directory listing: %s", body)
	}
	// ... and not in the database files either (the purge scrubs them).
	dir := peopleClaim(t)
	if got := onNode(t, "cat "+dir+"/*.db* | grep -a -c "+goneName+" || true"); got != "0" {
		t.Errorf("the erased name is still in the database files (%s matches)", got)
	}
	if got := onNode(t, "cat "+dir+"/*.db* | grep -a -c "+keptName+" || true"); got == "0" {
		t.Error("the grep above proves nothing: the kept name is not found either")
	}

	if !strings.Contains(firstRun, "restore hooks run for: forms, people, stub, work") {
		t.Errorf("every dump must be loaded by its hook")
	}
	work := portForward(t, "mod-work", "work", 8080)
	if st, body := rcall(t, "GET", work+"/v1/issues?project=DR", s.workToken, nil); st != 200 || !strings.Contains(body, "item kept across the restore") {
		t.Fatalf("the work item: %d %s", st, body)
	}

	api = portForward(t, "mod-forms", "forms-api", 8081)
	if st, leads := rcall(t, "GET", api+"/v1/leads", s.formsReadToken, nil); st != 200 || !strings.Contains(leads, "lead@restore.example") {
		t.Fatalf("the lead: %d %s", st, leads)
	}
	stub := volumeDir(t, "mod-stub", "data")
	if got := onNode(t, "cat "+stub+"/raw.txt"); got != "raw-volume-data" {
		t.Errorf("raw volume file: %q", got)
	}
	if got := onNode(t, "cat "+stub+"/restored-by-hook/probe"); got != "stub-data-from-the-hook" {
		t.Errorf("restore hook: %q", got)
	}
	if got := onNode(t, "cat /var/lib/can/erasure-ledger.jsonl"); !strings.Contains(got, s.goneID) {
		t.Errorf("the new server has no erasure ledger: %q", got)
	}
	onNode(t, "test -s /etc/can/backup.d/restic.password && test ! -e /var/lib/can/restore-staging && test ! -e /var/lib/can/dumps/modules/people")

	// --- the restored server can be backed up again, and is not "blank" any more -------------------
	requestDump(t)
	onNode(t, "test -s /var/lib/can/dumps/COMPLETE")
	if again, aerr := canRestore(t); aerr == nil || !strings.Contains(again, "NOTHING was done") {
		t.Fatalf("running a finished restore again must be refused: %v", aerr)
	}
	out, err = canRestore(t, "--state", "/root/second.json", "--accept-unerased", "forms:lead-submission")
	if err == nil || !strings.Contains(out, "not blank") {
		t.Fatalf("a second restore over a live server must be refused: %v", err)
	}
}

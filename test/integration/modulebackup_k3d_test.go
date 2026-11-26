//go:build integration

package integration

// Epic E08d (specs/043-module-backup-hooks): forms, work and files installed through can-core on
// k3d, dumped by their own `backup` subcommands as hook Jobs, checked, and restored into fresh
// instances of the same images (docker, on the runner) that serve the data again. The module-backup
// workflow builds the images, imports them into the cluster and sets CAN_IT_MODULE_BACKUP=1.
//
// The restored files instance gets the blobs from the module's volume (what the restic backup
// holds) and the keyring from the module's Secret (what a restore needs and the dump does not hold).

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const backupCompany = "schema: 1\nname: Backup Co\ndomain: backup.example\nprovider:\n  type: local\nmodules:\n" +
	"  forms:\n    enabled: true\n  work:\n    enabled: %v\n  files:\n    enabled: true\n"

type backupModule struct {
	id, service, volumeDir string
	port                   int
}

var backupModules = []backupModule{
	{id: "forms", service: "forms", port: 8080},
	{id: "work", service: "work", port: 8080},
	{id: "files", service: "files", port: 8095},
}

func call(t *testing.T, method, url, token, contentType string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body)) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "application/json")
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req) //nolint:gosec // loopback
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

func jsonID(t *testing.T, b []byte) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.ID == "" {
		t.Fatalf("no id in %s", b)
	}
	return v.ID
}

// moduleSecret returns the decoded values of mod-<id>/can-module.
func moduleSecret(t *testing.T, id string) map[string]string {
	t.Helper()
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(mustKubectl(t, "-n", "mod-"+id, "get", "secret", "can-module", "-o", "json")), &s); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range s.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = string(b)
	}
	return out
}

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

// checkDump verifies a copied dump set: the manifest's hash, and SQLite's own integrity check.
func checkDump(t *testing.T, dir, binary string) {
	t.Helper()
	var m struct {
		Binary   string `json:"binary"`
		Database struct {
			Size int64 `json:"size"`
		} `json:"database"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Binary != binary {
		t.Fatalf("manifest %s: %v %s", dir, err, b)
	}
	gz, err := os.Open(filepath.Join(dir, "database.sqlite3.gz")) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil || int64(len(raw)) != m.Database.Size {
		t.Fatalf("%s: %d bytes, manifest %d: %v", dir, len(raw), m.Database.Size, err)
	}
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var res string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" { //nolint:noctx // test helper
		t.Fatalf("%s: integrity_check %q %v", dir, res, err)
	}
}

func TestModuleBackupHooksOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_MODULE_BACKUP") != "1" {
		t.Skip("CAN_IT_MODULE_BACKUP=1 not set (needs can-core, can-forms, can-work and can-files images in the cluster)")
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		out, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=120")
		t.Logf("can-core log:\n%s", out)
		out, _ = kubectl("get", "pods,jobs,pvc,quota", "-A", "-o", "wide")
		t.Logf("objects:\n%s", out)
		out, _ = kubectl("get", "events", "-A", "--sort-by=.lastTimestamp")
		t.Logf("events:\n%s", out)
	})
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	prepareDumpsDir(t)
	canApplyModules(t, fmt.Sprintf(backupCompany, true))
	rolledOut := func(id string) {
		eventually(t, 240*time.Second, id+" to be rolled out", func() (bool, string) {
			o, err := kubectl("-n", "mod-"+id, "rollout", "status", "deploy/"+id, "--timeout=5s")
			return err == nil, o
		})
	}
	for _, m := range backupModules {
		rolledOut(m.id)
	}
	// files refuses uploads without a virus scanner; this throwaway cluster has none.
	cfg := mustKubectl(t, "-n", "mod-files", "get", "cm", "files-config", "-o", `jsonpath={.data.files\.yaml}`)
	if !strings.Contains(cfg, "required: true") {
		t.Fatalf("files config:\n%s", cfg)
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]string{"files.yaml": strings.Replace(cfg, "required: true", "required: false", 1)}})
	if err != nil {
		t.Fatal(err)
	}
	mustKubectl(t, "-n", "mod-files", "patch", "cm", "files-config", "--type=merge", "-p", string(patch))
	mustKubectl(t, "-n", "mod-files", "rollout", "restart", "deploy/files")
	rolledOut("files")

	secrets := map[string]map[string]string{}
	for _, m := range backupModules {
		secrets[m.id] = moduleSecret(t, m.id)
	}

	// --- data in each module -------------------------------------------------------------------
	forms := portForward(t, "mod-forms", "forms", 8080)
	st, b := call(t, "GET", forms+"/_forms/contact/token", "", "", nil)
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &tok); st != 200 || err != nil {
		t.Fatalf("form token: %d %s", st, b)
	}
	time.Sleep(4 * time.Second) // a faster submit is treated as spam
	form := url.Values{"name": {"Backup Tester"}, "email": {"backup@acme.example"}, "message": {"kept across a restore"}, "_t": {tok.Token}}
	if st, b = call(t, "POST", forms+"/_forms/contact", "", "application/x-www-form-urlencoded", []byte(form.Encode())); st != 200 && st != 201 {
		t.Fatalf("form post: %d %s", st, b)
	}

	workURL := portForward(t, "mod-work", "work", 8080)
	if st, b = call(t, "POST", workURL+"/v1/projects", secrets["work"]["CAN_WORK_ADMIN_TOKEN"], "application/json", []byte(`{"key":"BK","name":"Kept project"}`)); st != 201 {
		t.Fatalf("create project: %d %s", st, b)
	}

	filesURL := portForward(t, "mod-files", "files", 8095)
	filesTok := secrets["files"]["CAN_FILES_API_TOKEN"]
	content := []byte("these bytes live on the volume, not in the database\n")
	if st, b = call(t, "POST", filesURL+"/v1/spaces", filesTok, "application/json", []byte(`{"name":"Kept space"}`)); st != 201 {
		t.Fatalf("create space: %d %s", st, b)
	}
	space := jsonID(t, b)
	if st, b = call(t, "POST", filesURL+"/v1/spaces/"+space+"/files?name=kept.txt", filesTok, "text/plain", content); st != 201 {
		t.Fatalf("upload: %d %s", st, b)
	}
	fileID := jsonID(t, b)

	// --- dump: every module's hook Job runs in its own image within its own limit --------------
	t.Logf("dump run with three module hook Jobs: %s", requestDump(t).Round(100*time.Millisecond))
	dumped := onNode(t, "cat /var/lib/can/dumps/DUMPED")
	for _, m := range backupModules {
		if !strings.Contains(dumped, m.id+" sqlite") {
			t.Fatalf("DUMPED lacks %s:\n%s", m.id, dumped)
		}
		if out := mustKubectl(t, "-n", "mod-"+m.id, "get", "jobs", "-o", "name"); out != "" {
			t.Errorf("hook Jobs left behind: %s", out)
		}
	}
	work := t.TempDir()
	dockerCLI(t, "cp", nodeName()+":/var/lib/can/dumps/modules", filepath.Join(work, "dumps"))
	// The files volume as the host backup sees it.
	blobDir := onNode(t, "ls -d /var/lib/rancher/k3s/storage/pvc-*_mod-files_files-data/blobs")
	dockerCLI(t, "cp", nodeName()+":"+blobDir, filepath.Join(work, "blobs"))
	for _, m := range backupModules {
		checkDump(t, filepath.Join(work, "dumps", m.id), "can-"+m.id)
	}

	// --- restore into fresh instances of the same images ---------------------------------------
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	for _, m := range backupModules {
		image := mustKubectl(t, "-n", "mod-"+m.id, "get", "deploy", m.id, "-o", "jsonpath={.spec.template.spec.containers[0].image}")
		base := filepath.Join(work, "fresh-"+m.id)
		data, conf := filepath.Join(base, "data"), filepath.Join(base, "config")
		for _, d := range []string{data, conf} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		yml := mustKubectl(t, "-n", "mod-"+m.id, "get", "cm", m.id+"-config", "-o", `jsonpath={.data.`+m.id+`\.yaml}`)
		// The keyring sits where the config expects it. It is a Secret of the module, not part of the dump.
		if kr := secrets[m.id]["keyring"]; kr != "" {
			if err := os.WriteFile(filepath.Join(conf, "keyring"), []byte(kr), 0o400); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(conf, m.id+".yaml"), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		common := []string{"--user", user, "-v", data + ":/data", "-v", conf + ":/etc/can-" + m.id + ":ro"}
		args := append([]string{"run", "--rm", "--network", "none", "--memory", "64m"}, common...)
		args = append(args, "-v", filepath.Join(work, "dumps", m.id)+":/restore:ro")
		if m.id == "files" {
			args = append(args, "-v", filepath.Join(work, "blobs")+":/volume-blobs:ro", image, "restore", "-from", "/restore", "-blobs", "/volume-blobs")
		} else {
			args = append(args, image, "restore", "-from", "/restore")
		}
		t.Log(dockerCLI(t, args...))

		// The fresh instance serves it, with the secrets the live module had.
		env := ""
		for k, v := range secrets[m.id] {
			if strings.HasPrefix(k, "CAN_"+strings.ToUpper(m.id)+"_") {
				env += k + "=" + v + "\n"
			}
		}
		envFile := filepath.Join(base, "env")
		if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("e08d-restored-%s-%d", m.id, time.Now().UnixNano())
		local, second := freePort(t), freePort(t)
		args = append([]string{"run", "-d", "--name", name, "--env-file", envFile, "-p", fmt.Sprintf("127.0.0.1:%d:%d", local, m.port)}, common...)
		if m.id == "forms" { // the leads API is its own listener
			args = append([]string{args[0], args[1], "-p", fmt.Sprintf("127.0.0.1:%d:8081", second)}, args[2:]...)
		}
		args = append(args, image, "serve", "-config", "/etc/can-"+m.id+"/"+m.id+".yaml")
		dockerCLI(t, args...)
		t.Cleanup(func() {
			if t.Failed() {
				out, _ := exec.Command("docker", "logs", "--tail", "40", name).CombinedOutput() //nolint:gosec // test helper
				t.Logf("%s:\n%s", name, out)
			}
			_ = exec.Command("docker", "rm", "-f", name).Run() //nolint:gosec // only the container this test started
		})
		restored := fmt.Sprintf("http://127.0.0.1:%d", local)
		eventually(t, 30*time.Second, "the restored "+m.id, func() (bool, string) {
			resp, err := http.Get(restored + "/healthz") //nolint:gosec,noctx // loopback
			if err != nil {
				return false, err.Error()
			}
			_ = resp.Body.Close()
			return resp.StatusCode == 200, resp.Status
		})
		switch m.id {
		case "forms":
			st, b = call(t, "GET", fmt.Sprintf("http://127.0.0.1:%d/v1/leads", second), secrets["forms"]["CAN_FORMS_API_READ_TOKEN"], "", nil)
			if st != 200 || !strings.Contains(string(b), "backup@acme.example") {
				t.Errorf("restored forms lacks the lead: %d %s", st, b)
			}
		case "work":
			st, b = call(t, "GET", restored+"/v1/projects/BK", secrets["work"]["CAN_WORK_ADMIN_TOKEN"], "", nil)
			if st != 200 || !strings.Contains(string(b), "Kept project") {
				t.Errorf("restored work lacks the project: %d %s", st, b)
			}
		case "files":
			st, b = call(t, "GET", restored+"/v1/files/"+fileID+"/content", filesTok, "", nil)
			if st != 200 || !bytes.Equal(b, content) {
				t.Errorf("restored files content: %d %q", st, b)
			}
		}
	}

	// --- a disabled module (scaled to zero) is still dumped by its own image --------------------
	canApplyModules(t, fmt.Sprintf(backupCompany, false))
	eventually(t, 120*time.Second, "work to scale to zero", func() (bool, string) {
		o, _ := kubectl("-n", "mod-work", "get", "deploy", "work", "-o", "jsonpath={.spec.replicas}/{.status.replicas}")
		return o == "0/" || o == "0/0", o
	})
	requestDump(t)
	if got := onNode(t, "cat /var/lib/can/dumps/DUMPED"); !strings.Contains(got, "work sqlite") {
		t.Errorf("the disabled module was not dumped:\n%s\n%s", got, onNode(t, "cat /var/lib/can/dumps/LAST-RUN"))
	}
}

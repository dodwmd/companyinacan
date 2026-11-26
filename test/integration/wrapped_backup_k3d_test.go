//go:build integration

package integration

// Epic E08e (specs/046-wrapped-backups): analytics, support and newsletter installed through
// can-core on k3d from their pinned images, data made in each through the application, one dump
// run; then the module namespaces and the database server are deleted and restored from the dumps. Runs only with
// CAN_IT_WRAPPED_BACKUP=1 (.github/workflows/wrapped-backup.yml); it installs three modules, so
// use a throw-away cluster.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/routing"
)

const (
	fileMarker  = "kept-across-a-restore"
	markerPath  = "can-it/marker.txt"
	dumpsOnNode = "/var/lib/can/dumps"
)

var wrappedVolumes = map[string]string{"support": "/data", "newsletter": "/listmonk/uploads"}

// checkParts compares every part of a copied set with the size and hash its manifest records.
func checkParts(t *testing.T, dir, module string, want ...string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["binary"] != "can-core" || m["module"] != module {
		t.Fatalf("manifest of %s: %v %s", module, err, b)
	}
	got := map[string]bool{}
	for _, v := range m {
		p, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _ := p["name"].(string)
		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // test file
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if p["sha256"] != hex.EncodeToString(sum[:]) || p["size"] != float64(len(raw)) {
			t.Errorf("%s/%s does not match the manifest", module, name)
		}
		got[name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s: the set lacks %s (%v)", module, w, got)
		}
	}
}

// tarEntry returns the content of one file of a files.tar.gz.
func tarEntry(t *testing.T, file, name string) string {
	t.Helper()
	f, err := os.Open(file) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("%s holds no %s", file, name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == name {
			b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
}

// coreRestore runs one of `can restore`'s operations in the can-core pod (specs/042-restore).
func coreRestore(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectlStdin("{}", append([]string{"-n", "can-system", "exec", "-i", "deploy/can-core", "--", "/can-core", "restore"}, args...)...)
	if err != nil {
		logs, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=80")
		jobs, _ := kubectl("get", "jobs,pods", "-A", "-o", "wide")
		t.Fatalf("restore %v: %v\n%s\ncan-core log:\n%s\n%s", args, err, out, logs, jobs)
	}
	t.Logf("restore %v: %s", args, out)
	return out
}

func setRestoreMode(t *testing.T, env ...string) {
	t.Helper()
	mustKubectl(t, append([]string{"-n", "can-system", "set", "env", "deploy/can-core"}, env...)...)
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=240s")
}

// restoreOntoNothing deletes every module namespace and the shared Postgres with its volume, and
// restores from the dumps that are on the node: new volumes, a new database server.
func restoreOntoNothing(t *testing.T) {
	t.Helper()
	// Restore mode first: it imports the state dump and stops the reconciler and the dump runs.
	setRestoreMode(t, "CAN_RESTORE=e08e-ci", "CAN_RESTORE_OVERWRITE=e08e-ci")
	if got := coreRestore(t, "status"); !strings.HasPrefix(got, "restoring") {
		t.Fatalf("not in restore mode: %s", got)
	}
	mustKubectl(t, "delete", "namespace", "mod-analytics", "mod-support", "mod-newsletter", "--wait=true", "--timeout=300s")
	mustKubectl(t, "-n", "can-data", "delete", "deploy/postgres", "service/postgres", "secret/can-postgres", "--wait=true", "--timeout=180s")
	mustKubectl(t, "-n", "can-data", "delete", "pvc/postgres-data", "--wait=true", "--timeout=180s")

	coreRestore(t, "apply")
	start := time.Now()
	out := coreRestore(t, "hooks")
	t.Logf("MEASURE restore hooks of analytics, support and newsletter: %s", time.Since(start).Round(time.Second))
	for _, id := range []string{"analytics", "support", "newsletter"} {
		if !strings.Contains(out, `"`+id+`"`) {
			t.Fatalf("%s was not restored: %s", id, out)
		}
	}
	coreRestore(t, "complete")
	setRestoreMode(t, "CAN_RESTORE-", "CAN_RESTORE_OVERWRITE-")
}

func TestWrappedModuleBackupOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_WRAPPED_BACKUP") != "1" {
		t.Skip("CAN_IT_WRAPPED_BACKUP=1 not set (needs can-core in a throw-away cluster and docker)")
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, args := range [][]string{
			{"-n", "can-system", "logs", "deploy/can-core", "--tail=120"},
			{"get", "pods,jobs,pvc,quota", "-A", "-o", "wide"},
			{"get", "events", "-A", "--sort-by=.lastTimestamp"},
		} {
			out, _ := kubectl(args...)
			t.Logf("kubectl %v:\n%s", args, out)
		}
	})
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	applyRoutingFacts(t, routing.Default(wrappedDomain))
	prepareDumpsDir(t)

	company := fmt.Sprintf("schema: 1\nname: Wrapped Co\ndomain: %s\nprovider:\n  type: local\nmodules:\n"+
		"  analytics: {enabled: true}\n  support: {enabled: true}\n  newsletter: {enabled: true}\n", wrappedDomain)
	canApplyModules(t, company)
	// The dumps copy the mounted ConfigMap, which the kubelet fills only a minute or two after it is
	// first created: restart, as a server that was set up with its company has it from the start.
	mustKubectl(t, "-n", "can-system", "rollout", "restart", "deploy/can-core")
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=240s")
	for _, id := range []string{"support", "analytics", "newsletter"} {
		eventually(t, 15*time.Minute, id+" to be rolled out", func() (bool, string) {
			o, err := kubectl("-n", "mod-"+id, "rollout", "status", "deploy/"+id, "--timeout=5s")
			return err == nil, o
		})
	}

	// --- data, made through the applications ---------------------------------------------------
	supportMailbox(t)
	analyticsChecks(t)
	newsletterChecks(t)
	for id, dir := range wrappedVolumes {
		mustKubectl(t, "-n", "mod-"+id, "exec", "deploy/"+id, "-c", "app", "--", "sh", "-c",
			"mkdir -p "+dir+"/can-it && printf %s "+fileMarker+" > "+dir+"/"+markerPath)
	}

	// --- one dump run: every module's Job, within its namespace's quota and policies -----------
	t.Logf("MEASURE dump run with analytics, support and newsletter: %s", requestDump(t).Round(100*time.Millisecond))
	dumped := onNode(t, "cat "+dumpsOnNode+"/DUMPED")
	for _, want := range []string{"analytics sqlite", "support postgres", "newsletter postgres"} {
		if !strings.Contains(dumped, want) {
			t.Fatalf("DUMPED lacks %q:\n%s\n%s", want, dumped, onNode(t, "cat "+dumpsOnNode+"/LAST-RUN"))
		}
	}
	if got := onNode(t, "cat "+dumpsOnNode+"/LAST-RUN"); strings.Contains(got, `"skipped"`) {
		t.Errorf("something was skipped: %s", got)
	}
	// A restore needs the company.yaml of the set.
	onNode(t, "test -s "+dumpsOnNode+"/company.yaml")
	t.Log("MEASURE dump set sizes:\n" + onNode(t, "cd "+dumpsOnNode+"/modules && ls -l */ | cat"))

	work := t.TempDir()
	dockerCLI(t, "cp", nodeName()+":"+dumpsOnNode+"/modules", filepath.Join(work, "dumps"))
	set := func(id string, name ...string) string {
		return filepath.Join(append([]string{work, "dumps", id}, name...)...)
	}
	checkParts(t, set("analytics"), "analytics", "database.sqlite3.gz")
	checkParts(t, set("support"), "support", "database.pgdump", "files.tar.gz")
	checkParts(t, set("newsletter"), "newsletter", "database.pgdump", "files.tar.gz")

	for id := range wrappedVolumes {
		if got := tarEntry(t, set(id, "files.tar.gz"), markerPath); got != fileMarker {
			t.Errorf("%s: archived file holds %q", id, got)
		}
	}
	// FreeScout links its storage, modules and .env (the key it encrypts mail passwords with) into
	// the volume: they are in the archive.
	if got := tarEntry(t, set("support", "files.tar.gz"), "config"); !strings.Contains(got, "APP_KEY") {
		t.Error("support: the archived volume lacks FreeScout's application key")
	}

	// --- everything gone, then restored from the dumps -----------------------------------------
	appKey := mustKubectl(t, "-n", "mod-support", "exec", "deploy/support", "-c", "app", "--", "grep", "^APP_KEY", "/data/config")
	restoreOntoNothing(t)
	for _, id := range []string{"support", "analytics", "newsletter"} {
		eventually(t, 15*time.Minute, id+" to be back", func() (bool, string) {
			o, err := kubectl("-n", "mod-"+id, "rollout", "status", "deploy/"+id, "--timeout=5s")
			return err == nil, o
		})
	}
	for id, dir := range wrappedVolumes {
		if got := mustKubectl(t, "-n", "mod-"+id, "exec", "deploy/"+id, "-c", "app", "--", "cat", dir+"/"+markerPath); got != fileMarker {
			t.Errorf("%s: the restored file holds %q", id, got)
		}
	}
	if got := mustKubectl(t, "-n", "mod-support", "exec", "deploy/support", "-c", "app", "--", "grep", "^APP_KEY", "/data/config"); got != appKey {
		t.Error("support: the application key changed")
	}
	if got := moduleSQL(t, "mod_support", "select email from mailboxes where name = 'CI mailbox'"); got != "help@"+wrappedDomain {
		t.Errorf("support: the mailbox was not restored: %q", got)
	}
	if got := moduleSQL(t, "mod_newsletter", "select count(*) from lists where name = 'ci-list'"); got != "1" {
		t.Errorf("newsletter: the list was not restored: %q", got)
	}
	eventually(t, 90*time.Second, "the restored page view", func() (bool, string) {
		o, err := kubectl("-n", "mod-analytics", "exec", "deploy/analytics", "-c", "app", "--",
			"goatcounter", "db", "query", "select path from paths where path = '/ci-page'", "-db", "sqlite+/data/goatcounter.sqlite3")
		return err == nil && strings.Contains(o, "/ci-page"), o
	})
	// The administrators of the restored apps are the ones from before: sign in again.
	signInSupport(t)
	// And the restored server can be backed up again.
	requestDump(t)
}

// signInSupport signs in to FreeScout with the password in the restored Secret.
func signInSupport(t *testing.T) {
	t.Helper()
	host := "support." + wrappedDomain
	pw := adminPassword(t, "support", "CAN_SUPPORT_ADMIN_PASSWORD")
	throughTraefik(t, host, "/login", `name="password"`)
	b := newBrowser(portForward(t, "mod-support", "support", 8080), host)
	p := b.do(t, "GET", "/login", "", "", nil)
	m := csrfRE.FindStringSubmatch(p.body)
	if p.status != 200 || m == nil {
		t.Fatalf("login page: %d", p.status)
	}
	p = b.form(t, "/login", url.Values{"_token": {m[1]}, "email": {"admin@" + wrappedDomain}, "password": {pw}})
	if p.status != 302 || strings.HasSuffix(p.location, "/login") {
		t.Errorf("signing in after the restore: %d to %q", p.status, p.location)
	}
}

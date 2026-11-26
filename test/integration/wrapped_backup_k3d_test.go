//go:build integration

package integration

// Epic E08e (specs/046-wrapped-backups): analytics, support and newsletter installed through
// can-core on k3d from their pinned images, data made in each through the application, one dump
// run, and the dump restored into fresh databases and checked. Runs only with
// CAN_IT_WRAPPED_BACKUP=1 (.github/workflows/wrapped-backup.yml); it installs three modules, so
// use a throw-away cluster.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// restorePostgres loads a pg_dump archive into a database created empty for it, as the module's
// own role (what the restore does), and returns a query function for it.
func restorePostgres(t *testing.T, role, file string) func(string) string {
	t.Helper()
	db := fmt.Sprintf("e08e_fresh_%s_%d", role, time.Now().UnixNano())
	moduleSQL(t, "postgres", "create database "+db+" owner "+role)
	t.Cleanup(func() { moduleSQL(t, "postgres", "drop database "+db+" with (force)") })
	raw, err := os.ReadFile(file) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kubectlStdin(string(raw), "-n", "can-data", "exec", "-i", "deploy/postgres", "--",
		"pg_restore", "--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges", "--role="+role, "-U", "postgres", "-d", db); err != nil {
		t.Fatalf("pg_restore into %s: %v\n%s", db, err, out)
	}
	return func(q string) string { return moduleSQL(t, db, q) }
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
	t.Log("MEASURE dump set sizes:\n" + onNode(t, "cd "+dumpsOnNode+"/modules && ls -l */ | cat"))

	work := t.TempDir()
	dockerCLI(t, "cp", nodeName()+":"+dumpsOnNode+"/modules", filepath.Join(work, "dumps"))
	set := func(id string, name ...string) string {
		return filepath.Join(append([]string{work, "dumps", id}, name...)...)
	}
	checkParts(t, set("analytics"), "analytics", "database.sqlite3.gz")
	checkParts(t, set("support"), "support", "database.pgdump", "files.tar.gz")
	checkParts(t, set("newsletter"), "newsletter", "database.pgdump", "files.tar.gz")

	// --- the dumps load into fresh databases and hold the data ---------------------------------
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
	q := restorePostgres(t, "mod_support", set("support", "database.pgdump"))
	if got := q("select email from mailboxes where name = 'CI mailbox'"); got != "help@"+wrappedDomain {
		t.Errorf("support: the mailbox is not in the restored database: %q", got)
	}
	q = restorePostgres(t, "mod_newsletter", set("newsletter", "database.pgdump"))
	if got := q("select count(*) from lists where name = 'ci-list'"); got != "1" {
		t.Errorf("newsletter: the list is not in the restored database: %q", got)
	}
	gz, err := os.Open(set("analytics", "database.sqlite3.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(work, "analytics.db")
	if err := os.WriteFile(fresh, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+fresh+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var path string
	if err := db.QueryRow(`select path from paths where path = '/ci-page'`).Scan(&path); err != nil { //nolint:noctx // test helper
		t.Errorf("analytics: the page view is not in the restored database: %v", err)
	}
}

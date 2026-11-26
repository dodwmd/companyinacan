package restoreops

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/backup/backuptest"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func dumpsWithState(t *testing.T, state []byte) string {
	t.Helper()
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "can-core"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, backup.StateDump), state, 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBootWithoutRestoreIsNormal(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	r, err := Boot(context.Background(), db, "", "", false)
	if err != nil || r {
		t.Fatal(r, err)
	}
	if _, err := os.Stat(db); err == nil {
		t.Fatal("a normal start must not create or import anything")
	}
}

// Restore mode is latched on disk: it survives a start without CAN_RESTORE, imports once, and
// only finish ends it. A finished restore id never latches again.
func TestBootLatchesAndImportsOnce(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.db")
	dumps := dumpsWithState(t, backuptest.StateDB(t, backuptest.Module{ID: "forms", State: "enabled"}))
	r, err := Boot(ctx, db, dumps, "abc", false)
	if err != nil || !r {
		t.Fatal(r, err)
	}
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if mods, err := st.ListModules(ctx); err != nil || len(mods) != 1 {
		t.Fatal(mods, err)
	}
	if _, err := st.ExecContext(ctx, `DELETE FROM modules`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	// no CAN_RESTORE any more (someone re-applied the normal manifests): still restoring, no re-import
	r, err = Boot(ctx, db, dumps, "", false)
	if err != nil || !r {
		t.Fatal("restore mode ended without `restore complete`", r, err)
	}
	st, _ = store.Open(ctx, db)
	if mods, _ := st.ListModules(ctx); len(mods) != 0 {
		t.Fatal("the state was imported a second time over the restore's own work")
	}
	_ = st.Close()
	if err := finish(db); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "abc"} {
		if r, err := Boot(ctx, db, dumps, id, false); err != nil || r {
			t.Fatalf("a finished restore must start normally (CAN_RESTORE=%q): %v %v", id, r, err)
		}
	}
}

func TestBootRefusesToReplaceDataWithoutConfirmation(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, backuptest.StateDB(t, backuptest.Module{ID: "wiki", State: "enabled"}), 0o600); err != nil {
		t.Fatal(err)
	}
	dumps := dumpsWithState(t, backuptest.StateDB(t))
	if _, err := Boot(ctx, db, dumps, "x", false); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatal(err)
	}
	// A failed import stays latched: it never comes up as a normal server in between.
	if id, _ := Pending(db); id != "x" {
		t.Fatalf("pending = %q", id)
	}
	if r, err := Boot(ctx, db, dumps, "x", true); err != nil || !r {
		t.Fatal(r, err)
	}
	if _, err := os.Stat(db + ".pre-restore"); err != nil {
		t.Error("the replaced database must be kept")
	}
}

func TestBootRejectsADamagedDump(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if _, err := Boot(context.Background(), db, dumpsWithState(t, []byte("garbage")), "x", false); err == nil {
		t.Fatal("imported garbage")
	}
	if _, err := os.Stat(db); err == nil {
		t.Fatal("a damaged dump was put in place")
	}
}

type fakeHooks struct {
	calls []string
	fail  map[string]error
}

func (f *fakeHooks) RunRestore(_ context.Context, m string, missingBlobs bool) error {
	if missingBlobs {
		f.calls = append(f.calls, "missing-blobs "+m)
	}
	f.calls = append(f.calls, "restore "+m)
	return f.fail[m]
}
func (f *fakeHooks) HasSubcommandRestore(m string) bool { return m == "forms" }
func (f *fakeHooks) RunPurgeScript(_ context.Context, m, s, c string) error {
	f.calls = append(f.calls, "script "+m+" "+s+" "+c)
	return f.fail[m]
}
func (f *fakeHooks) RunPurgeCLI(_ context.Context, m, s, c string) error {
	f.calls = append(f.calls, "cli "+m+" "+s+" "+c)
	return f.fail[m]
}

func module(id, state string, change func(*config.Module)) store.Module {
	m := config.Module{ID: id, Version: "0.1.0"}
	if change != nil {
		change(&m)
	}
	return store.Module{ID: id, Version: "0.1.0", Namespace: "mod-" + id, ServiceAccount: id, State: state, SettingsHash: "h", Manifest: m}
}

func classes(cs ...string) func(*config.Module) {
	return func(m *config.Module) {
		for _, c := range cs {
			m.DataClasses = append(m.DataClasses, struct {
				Class       string `yaml:"class"`
				Retention   string `yaml:"retention"`
				Sensitivity string `yaml:"sensitivity"`
			}{Class: c})
		}
	}
}

func ops(t *testing.T, mods ...store.Module) (*Ops, *fakeHooks, *kube.Fake) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, m := range mods {
		if err := db.UpsertModule(ctx, m, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeID(filepath.Join(dir, pendingFile), "rid"); err != nil {
		t.Fatal(err)
	}
	h := &fakeHooks{fail: map[string]error{}}
	api := kube.NewFake()
	return &Ops{DB: db, DBPath: filepath.Join(dir, "state.db"), DumpsDir: t.TempDir(), API: api, Hooks: h, Poll: time.Millisecond, Wait: time.Second}, h, api
}

func entry(subject, class string) backup.Entry {
	return backup.Entry{Subject: subject, Class: class, Date: "2026-11-25"}
}

func TestOperationsRefuseALiveServer(t *testing.T) {
	o, h, _ := ops(t)
	if err := finish(o.DBPath); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := o.Apply(ctx, false); err == nil {
		t.Error("apply ran on a live server")
	}
	if _, err := o.RunHooks(ctx, backup.HookOptions{}); err == nil {
		t.Error("hooks ran on a live server")
	}
	if _, err := o.Erase(ctx, []backup.Entry{entry("s", "c")}, nil); err == nil {
		t.Error("erase ran on a live server")
	}
	if len(h.calls) != 0 {
		t.Error(h.calls)
	}
}

// A tombstone that cannot be replayed stops everything before anything is purged, unless named.
func TestEraseFailsClosed(t *testing.T) {
	o, h, _ := ops(t,
		module("people", "enabled", classes("directory-basic")),
		module("analytics", "enabled", classes("lead-submission")))
	es := []backup.Entry{entry("pers_1", "directory-basic"), entry("l1", "lead-submission"), entry("x", "no-such-class")}
	rep, err := o.Erase(context.Background(), es, []string{"analytics:lead-submission"})
	if err == nil || !strings.Contains(err.Error(), "*:no-such-class") || len(h.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, h.calls)
	}
	if strings.Join(rep.Unapplied, ",") != "*:no-such-class,analytics:lead-submission" {
		t.Error(rep.Unapplied)
	}
	rep, err = o.Erase(context.Background(), es, []string{"analytics:lead-submission", "*:no-such-class"})
	if err != nil || strings.Join(h.calls, ",") != "cli people pers_1 directory-basic" || len(rep.Purged) != 1 {
		t.Fatalf("err=%v calls=%v rep=%+v", err, h.calls, rep)
	}
}

// Disabled modules hold data too; a manifest purge hook is preferred; purged dumps are removed.
func TestEraseMethodsAndDumpRemoval(t *testing.T) {
	o, h, _ := ops(t,
		module("people", "disabled", classes("directory-basic")),
		module("wiki", "enabled", func(m *config.Module) { classes("wiki-page")(m); m.Hooks.Purge = "./hooks/purge.sh" }))
	for _, id := range []string{"people", "wiki", "other"} {
		p := filepath.Join(o.DumpsDir, "modules", id)
		_ = os.MkdirAll(p, 0o700)
		_ = os.WriteFile(filepath.Join(p, "d"), []byte("x"), 0o600)
	}
	if _, err := o.Erase(context.Background(), []backup.Entry{entry("pers_1", "directory-basic"), entry("p2", "wiki-page")}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.calls, ",") != "cli people pers_1 directory-basic,script wiki p2 wiki-page" {
		t.Error(h.calls)
	}
	left, _ := os.ReadDir(filepath.Join(o.DumpsDir, "modules"))
	if len(left) != 1 || left[0].Name() != "other" {
		t.Errorf("dumps of purged modules still hold the erased data: %v", left)
	}
}

func TestEraseFailureIsReported(t *testing.T) {
	o, h, _ := ops(t, module("people", "enabled", classes("directory-basic")))
	h.fail["people"] = context.DeadlineExceeded
	es := []backup.Entry{entry("pers_1", "directory-basic")}
	if _, err := o.Erase(context.Background(), es, nil); err == nil || !strings.Contains(err.Error(), "--accept-unerased people:directory-basic") {
		t.Fatal("a failed purge must stop the restore", err)
	}
	rep, err := o.Erase(context.Background(), es, []string{"people:directory-basic"})
	if err != nil || len(rep.Purged) != 0 || strings.Join(rep.Unapplied, ",") != "people:directory-basic" {
		t.Fatal("accepted by name, and reported as not applied", rep, err)
	}
}

// files and jobs purge over their API while running without a route, and are stopped again.
func TestEraseOverHTTP(t *testing.T) {
	o, _, api := ops(t, module("files", "enabled", func(m *config.Module) {
		classes("file-content")(m)
		m.Provides = append(m.Provides, struct {
			Capability string `yaml:"capability"`
			Schema     string `yaml:"schema"`
			Service    string `yaml:"service"`
			Port       int    `yaml:"port"`
			Status     string `yaml:"status"`
		}{Service: "files", Port: 8095})
	}))
	ctx := context.Background()
	for _, obj := range []kube.Object{
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "files", "namespace": "mod-files"}, "spec": map[string]any{"replicas": 0}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "can-module", "namespace": "mod-files"},
			"data": map[string]any{"CAN_FILES_RESTORE_TOKEN": base64.StdEncoding.EncodeToString([]byte("restore-token"))}},
	} {
		if err := api.Apply(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, _ := api.Get(ctx, "Deployment", "mod-files", "files")
		b := make([]byte, 200)
		n, _ := r.Body.Read(b)
		got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization") + " " + string(b[:n])
		if spec := d["spec"].(map[string]any); spec["replicas"] == nil || spec["replicas"] == float64(0) {
			got = "called while not running"
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	o.ServiceURL = func(string, string, int) string { return srv.URL }
	// the fake does not run pods: mark it ready as a kubelet would
	if err := api.MergePatch(ctx, "Deployment", "mod-files", "files", kube.Object{"status": map[string]any{"readyReplicas": 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Erase(ctx, []backup.Entry{entry("f1", "file-content")}, nil); err != nil {
		t.Fatal(err)
	}
	if got != `POST /v1/purge Bearer restore-token {"class":"file-content","subject":"f1"}` {
		t.Error(got)
	}
	d, _ := api.Get(ctx, "Deployment", "mod-files", "files")
	if d["spec"].(map[string]any)["replicas"] != float64(0) {
		t.Error("the module was left running before go-live")
	}
}

func TestRunHooks(t *testing.T) {
	hooked := func(m *config.Module) { m.Hooks.Backup, m.Hooks.Restore = "./b.sh", "./r.sh" }
	o, h, _ := ops(t, module("wiki", "disabled", hooked), module("crm", "enabled", hooked), module("people", "enabled", nil))
	for _, id := range []string{"wiki", "people"} {
		p := filepath.Join(o.DumpsDir, "modules", id)
		_ = os.MkdirAll(p, 0o700)
		_ = os.WriteFile(filepath.Join(p, "d"), []byte("x"), 0o600)
	}
	h.fail["people"] = context.DeadlineExceeded
	if _, err := o.RunHooks(context.Background(), backup.HookOptions{}); err == nil || !strings.Contains(err.Error(), "--accept-unrestored people") {
		t.Fatal(err)
	}
	h.calls = nil
	rep, err := o.RunHooks(context.Background(), backup.HookOptions{Accept: []string{"people"}, MissingBlobs: []string{"wiki"}})
	if err != nil || strings.Join(rep.Restored, ",") != "wiki" || strings.Join(h.calls, ",") != "missing-blobs wiki,restore wiki" {
		t.Fatal(err, rep, h.calls)
	}
	all := strings.Join(rep.Warnings, "\n")
	for _, want := range []string{`"crm" has no dump`, `"people": its dump is NOT loaded`} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in %s", want, all)
		}
	}
}

// Leaving restore mode sends nothing that was queued before the backup and marks the enabled
// modules for re-apply.
func TestComplete(t *testing.T) {
	ctx := context.Background()
	o, _, _ := ops(t, module("people", "enabled", nil), module("wiki", "disabled", nil))
	for _, q := range []string{
		`INSERT INTO admin_users(username,password_hash,totp_secret,created_at) VALUES ('a','h','s',1)`,
		`INSERT INTO sessions(id_hash,user_id,csrf_token,ip,created_at,expires_at) SELECT 'x', id, 'c', 'ip', 1, 9999999999 FROM admin_users`,
		`INSERT INTO user_invites(user_id,token_hash,totp_secret,purpose,created_by,created_at,expires_at) SELECT id,'h','s','reset','user:a',1,9999999999 FROM admin_users`,
		`INSERT INTO email_outbox(id,source,recipient,recipient_mask,subject,body,status,next_attempt_at,created_at,queued_at) VALUES ('m1','@core','a@b.example','a','s','b','pending',0,1,1)`,
	} {
		if _, err := o.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	rep, err := o.Complete(ctx)
	if err != nil || strings.Join(rep.Modules, ",") != "people" {
		t.Fatal(rep, err)
	}
	count := func(q string) (n int) {
		if err := o.DB.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	if count(`SELECT COUNT(*) FROM email_outbox WHERE status='pending'`) != 0 || count(`SELECT COUNT(*) FROM sessions`) != 0 || count(`SELECT COUNT(*) FROM user_invites`) != 0 {
		t.Error("queued mail or sessions survived")
	}
	if count(`SELECT COUNT(*) FROM modules WHERE settings_hash='' AND state='enabled'`) != 1 || count(`SELECT COUNT(*) FROM modules WHERE settings_hash='h'`) != 1 {
		t.Error("only the enabled modules are re-applied")
	}
	if id, _ := Pending(o.DBPath); id != "" {
		t.Error("still in restore mode")
	}
	if _, err := o.Complete(ctx); err != nil {
		t.Error("completing twice must be harmless", err)
	}
}

// can-core's own derived copies of an erased subject go.
func TestEraseCleansCoreState(t *testing.T) {
	ctx := context.Background()
	o, _, _ := ops(t, module("people", "enabled", classes("directory-basic")))
	for _, q := range []string{
		`INSERT INTO events(id,type,time,source,data_json) VALUES ('e1','person.created.v1',1,'people','{"party_id":"pers_1"}'), ('e2','person.created.v1',1,'people','{"party_id":"pers_10"}')`,
		`INSERT INTO deliveries(event_id,module_id,url,status,next_attempt_at) VALUES ('e1','m','u','pending',0)`,
	} {
		if _, err := o.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	rep, err := o.Erase(ctx, []backup.Entry{entry("pers_1", "directory-basic")}, nil)
	if err != nil || rep.CoreRows != 1 {
		t.Fatal(rep, err)
	}
	var events, deliveries int
	_ = o.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&events)
	_ = o.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries`).Scan(&deliveries)
	if events != 1 || deliveries != 0 {
		t.Errorf("events=%d deliveries=%d", events, deliveries)
	}
}

// A restore hook never runs while a pod of the module still does.
func TestRunHooksWaitsForTheModuleToStop(t *testing.T) {
	ctx := context.Background()
	o, h, api := ops(t, module("wiki", "enabled", func(m *config.Module) { m.Hooks.Restore = "./r.sh" }))
	o.Wait = 20 * time.Millisecond
	p := filepath.Join(o.DumpsDir, "modules", "wiki")
	_ = os.MkdirAll(p, 0o700)
	_ = os.WriteFile(filepath.Join(p, "d"), []byte("x"), 0o600)
	pod := func(phase string) {
		if err := api.Apply(ctx, kube.Object{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "wiki-0", "namespace": "mod-wiki"}, "status": map[string]any{"phase": phase}}); err != nil {
			t.Fatal(err)
		}
	}
	pod("Running")
	if _, err := o.RunHooks(ctx, backup.HookOptions{}); err == nil || !strings.Contains(err.Error(), "still running") || len(h.calls) != 0 {
		t.Fatal(err, h.calls)
	}
	pod("Succeeded")
	if _, err := o.RunHooks(ctx, backup.HookOptions{}); err != nil || len(h.calls) != 1 {
		t.Fatal(err, h.calls)
	}
}

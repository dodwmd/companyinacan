package restoreops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/validate"
)

// Hooks runs the module restore and purge Jobs (*dumps.KubeHooks).
type Hooks interface {
	RunRestore(ctx context.Context, module string) error
	// HasSubcommandRestore: the shipped module restores with its own binary, whatever the
	// recorded manifest says (specs/043-module-backup-hooks).
	HasSubcommandRestore(module string) bool
	RunPurgeScript(ctx context.Context, module, subject, class string) error
	RunPurgeCLI(ctx context.Context, module, subject, class string) error
}

var _ Hooks = (*dumps.KubeHooks)(nil)

// Ops are the operations of a restore. Every one is idempotent and refuses to run unless
// can-core is in restore mode.
type Ops struct {
	DB         *store.DB
	Audit      *audit.Log
	DBPath     string
	DumpsDir   string
	ModulesDir string
	API        kube.API
	Hooks      Hooks
	// Installer returns the quiescing installer; seed re-creates a module's exported Secrets and
	// postgresURL is the database administration URL, if one is configured or was exported.
	Installer func(seed func(context.Context, reconcile.Spec) error, postgresURL string) reconcile.ModuleInstaller
	// PostgresURL is the configured CAN_POSTGRES_ADMIN_URL ("" falls back to the exported Secret).
	PostgresURL string
	HTTP        *http.Client
	// ServiceURL is the in-cluster address of a module Service (a seam for tests).
	ServiceURL func(ns, service string, port int) string
	// Poll and Wait bound waiting for a module to start (defaults 2 s and 5 minutes).
	Poll, Wait time.Duration
}

func (o *Ops) check() error {
	id, err := Pending(o.DBPath)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("can-core is not in restore mode: refusing to run a restore operation on a live server")
	}
	return nil
}

func (o *Ops) export() (*backup.SecretsExport, error) {
	b, err := os.ReadFile(filepath.Join(o.DumpsDir, filepath.FromSlash(backup.SecretsDump))) //nolint:gosec // our dumps directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return backup.ParseSecretsExport(b)
}

func (o *Ops) company() (*config.Company, error) {
	b, err := os.ReadFile(filepath.Join(o.DumpsDir, backup.CompanyDump)) //nolint:gosec // our dumps directory
	if err != nil {
		return nil, err
	}
	doc, ps := config.LoadBytes(backup.CompanyDump, b)
	if doc == nil {
		return nil, fmt.Errorf("company.yaml: %v", ps)
	}
	return config.DecodeCompany(doc)
}

// manifest is the module's manifest as this release ships it (its chart is what gets rendered).
func (o *Ops) manifest(id string) (*config.Module, error) {
	p, ok := validate.ManifestPath(o.ModulesDir, id)
	if !ok {
		return nil, fmt.Errorf("this release of can-core has no module %q: restore with the release the backup was made with", id)
	}
	doc, ps := config.Load(p)
	if doc == nil {
		return nil, fmt.Errorf("%s: %v", p, ps)
	}
	return config.DecodeModule(doc)
}

func secretObject(s backup.ExportedSecret, module string) kube.Object {
	data := map[string]any{}
	for k, v := range s.Data {
		data[k] = v
	}
	typ := s.Type
	if typ == "" {
		typ = "Opaque"
	}
	labels := map[string]any{}
	if module != "" {
		labels["app.kubernetes.io/managed-by"] = "can-core"
		labels["can.dev/module"] = module
	}
	return kube.Object{"apiVersion": "v1", "kind": "Secret", "type": typ, "data": data,
		"metadata": map[string]any{"name": s.Name, "namespace": s.Namespace, "labels": labels}}
}

// createSecrets re-creates the exported Secrets of one namespace. An existing Secret is kept
// unless overwrite, so a resumed restore changes nothing.
func (o *Ops) createSecrets(ctx context.Context, e *backup.SecretsExport, ns, module string, overwrite bool) (int, error) {
	if e == nil {
		return 0, nil
	}
	n := 0
	for _, s := range e.Secrets {
		if s.Namespace != ns {
			continue
		}
		obj := secretObject(s, module)
		if overwrite {
			if err := o.API.Apply(ctx, obj); err != nil {
				return n, fmt.Errorf("secret %s/%s: %w", ns, s.Name, err)
			}
		} else if _, err := o.API.Create(ctx, obj); err != nil {
			return n, fmt.Errorf("secret %s/%s: %w", ns, s.Name, err)
		}
		n++
	}
	return n, nil
}

// Apply re-creates the exported Secrets and every recorded module (disabled and failed ones too:
// their data is restored as well), quiesced. The module records are not changed.
func (o *Ops) Apply(ctx context.Context, overwrite bool) (backup.ApplyReport, error) {
	var rep backup.ApplyReport
	if err := o.check(); err != nil {
		return rep, err
	}
	e, err := o.export()
	if err != nil {
		return rep, err
	}
	if e == nil {
		rep.Warnings = append(rep.Warnings, "no Secrets export: modules get NEW keys, and data sealed with the old ones cannot be read")
	}
	n, err := o.createSecrets(ctx, e, "can-system", "", overwrite)
	if err != nil {
		return rep, err
	}
	rep.Secrets += n
	pg := o.PostgresURL
	if pg == "" && e != nil {
		for _, s := range e.Secrets {
			if s.Namespace == "can-system" && s.Name == "can-postgres-admin" {
				b, err := base64.StdEncoding.DecodeString(s.Data["url"])
				if err != nil {
					return rep, errors.New("the exported database administration URL is not base64")
				}
				pg = string(b)
			}
		}
	}
	co, err := o.company()
	if err != nil {
		return rep, err
	}
	mods, err := o.DB.ListModules(ctx)
	if err != nil {
		return rep, err
	}
	seed := func(ctx context.Context, s reconcile.Spec) error {
		n, err := o.createSecrets(ctx, e, s.Namespace, s.ID, overwrite)
		rep.Secrets += n
		return err
	}
	inst := o.Installer(seed, pg)
	var errs []error
	for _, m := range mods {
		man, err := o.manifest(m.ID)
		if err == nil {
			if man.Version != m.Version {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("module %q was at version %s, this release installs %s", m.ID, m.Version, man.Version))
			}
			var settings map[string]any
			if ce, ok := co.Modules[m.ID]; ok {
				settings = ce.Settings
			}
			err = inst.Enable(ctx, reconcile.Spec{ID: m.ID, Version: man.Version, Namespace: m.Namespace, ServiceAccount: m.ServiceAccount,
				Settings: settings, Manifest: man, HMACSecret: m.HMACSecret, Domain: co.Domain})
		}
		switch {
		case err == nil:
			rep.Modules = append(rep.Modules, m.ID)
		case m.State == store.StateFailed:
			// Never ran successfully as far as the record says; it is retried after the restore.
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("module %q was failed when backed up and could not be recreated: %v", m.ID, err))
		default:
			errs = append(errs, fmt.Errorf("module %s: %w", m.ID, err))
		}
	}
	return rep, errors.Join(errs...)
}

// dumpedModules are the module ids with at least one file in the dumps.
func (o *Ops) dumpedModules() (map[string]bool, error) {
	out := map[string]bool{}
	root := filepath.Join(o.DumpsDir, "modules")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".") {
			rel, _ := filepath.Rel(root, p)
			id, _, ok := strings.Cut(filepath.ToSlash(rel), "/")
			if ok {
				out[id] = true
			}
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	return out, err
}

// RunHooks runs the replace-all restore hook of every module with a dump. A dump that cannot be
// loaded fails the step unless the module is listed in accept.
func (o *Ops) RunHooks(ctx context.Context, accept []string) (backup.HookReport, error) {
	var rep backup.HookReport
	if err := o.check(); err != nil {
		return rep, err
	}
	dumped, err := o.dumpedModules()
	if err != nil {
		return rep, err
	}
	mods, err := o.DB.ListModules(ctx)
	if err != nil {
		return rep, err
	}
	recorded := map[string]bool{}
	var errs []error
	for _, m := range mods {
		recorded[m.ID] = true
		man := &m.Manifest
		loadable := man.Hooks.Restore != "" || o.Hooks.HasSubcommandRestore(m.ID)
		if !dumped[m.ID] {
			if man.Hooks.Backup != "" {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("module %q has no dump in this snapshot; only its volume files (if any) are restored", m.ID))
			}
			continue
		}
		if slices.Contains(accept, m.ID) && !loadable {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("module %q: its dump is NOT loaded (no restore hook)", m.ID))
			continue
		}
		if err := o.Hooks.RunRestore(ctx, m.ID); err != nil {
			if slices.Contains(accept, m.ID) {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("module %q: its dump is NOT loaded: %v", m.ID, err))
				continue
			}
			errs = append(errs, fmt.Errorf("restore %s: %w (--accept-unrestored %s to go on without it)", m.ID, err, m.ID))
			continue
		}
		rep.Restored = append(rep.Restored, m.ID)
	}
	for id := range dumped {
		if !recorded[id] {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("the snapshot holds a dump of %q, which can-core does not record: not loaded", id))
		}
	}
	sort.Strings(rep.Warnings)
	return rep, errors.Join(errs...)
}

// How a module's purge(subject, class) is replayed.
type purgeMethod struct {
	cli bool // the binary's `purge` command, offline
	// otherwise: POST /v1/purge on the running (unrouted) module with this key of can-module
	tokenKey string
}

// inHouse are the in-house modules that implement purge without a hooks.purge script.
var inHouse = map[string]purgeMethod{
	"people": {cli: true},
	"equity": {cli: true},
	"board":  {cli: true},
	"files":  {tokenKey: "CAN_FILES_RESTORE_TOKEN"},
	"jobs":   {tokenKey: "CAN_JOBS_ADMIN_TOKEN"},
}

type purge struct {
	module store.Module
	entry  backup.Entry
}

func holds(m *config.Module, class string) bool {
	for _, c := range m.DataClasses {
		if c.Class == class {
			return true
		}
	}
	return false
}

// Erase replays the tombstones. First every tombstone is matched to the modules that declare its
// class; if one cannot be replayed and is not listed in accept, nothing is purged.
func (o *Ops) Erase(ctx context.Context, es []backup.Entry, accept []string) (backup.EraseReport, error) {
	var rep backup.EraseReport
	if err := o.check(); err != nil {
		return rep, err
	}
	mods, err := o.DB.ListModules(ctx)
	if err != nil {
		return rep, err
	}
	var todo []purge
	unapplied := map[string]bool{}
	for _, e := range es {
		held := false
		for _, m := range mods {
			if !holds(&m.Manifest, e.Class) {
				continue
			}
			held = true
			if _, ok := inHouse[m.ID]; !ok && m.Manifest.Hooks.Purge == "" {
				unapplied[m.ID+":"+e.Class] = true
				continue
			}
			todo = append(todo, purge{m, e})
		}
		if !held {
			unapplied["*:"+e.Class] = true
		}
	}
	var blocked []string
	for u := range unapplied {
		rep.Unapplied = append(rep.Unapplied, u)
		if !slices.Contains(accept, u) {
			blocked = append(blocked, u)
		}
	}
	sort.Strings(rep.Unapplied)
	sort.Strings(blocked)
	if len(blocked) > 0 {
		return rep, fmt.Errorf("tombstones cannot be replayed for %s (module:class; * means no installed module holds the class). Refusing to go live; if you will erase that data by hand, re-run with --accept-unerased for each", strings.Join(blocked, ", "))
	}
	stop, err := o.startForHTTPPurge(ctx, todo)
	defer stop()
	if err != nil {
		return rep, err
	}
	purged := map[string]bool{}
	for _, p := range todo {
		id, s, c := p.module.ID, p.entry.Subject, p.entry.Class
		var err error
		method, builtin := inHouse[id]
		switch {
		case p.module.Manifest.Hooks.Purge != "":
			err = o.Hooks.RunPurgeScript(ctx, id, s, c)
		case builtin && method.cli:
			err = o.Hooks.RunPurgeCLI(ctx, id, s, c)
		default:
			err = o.httpPurge(ctx, p.module, method.tokenKey, s, c)
		}
		if err != nil {
			// A purge that can never succeed (a mistyped subject in the append-only ledger) must
			// not make the company unrestorable: accepting it by name is the way out.
			if u := id + ":" + c; slices.Contains(accept, u) {
				if !unapplied[u] {
					unapplied[u] = true
					rep.Unapplied = append(rep.Unapplied, u)
				}
				continue
			}
			return rep, fmt.Errorf("purge %s of %s in module %s: %w (if it can never succeed and you will erase it by hand: --accept-unerased %s:%s)", c, s, id, err, id, c)
		}
		purged[id] = true
		rep.Purged = append(rep.Purged, fmt.Sprintf("%s: %s %s", id, c, s))
	}
	// A dump of a purged module still holds what was just erased; the next dump run replaces it.
	for id := range purged {
		if err := os.RemoveAll(filepath.Join(o.DumpsDir, "modules", id)); err != nil {
			return rep, err
		}
	}
	rep.CoreRows, err = o.purgeCore(ctx, es)
	if err != nil {
		return rep, fmt.Errorf("can-core's own state: %w", err)
	}
	return rep, nil
}

func (o *Ops) wait() (time.Duration, time.Duration) {
	poll, wait := o.Poll, o.Wait
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	return poll, wait
}

// startForHTTPPurge scales up the modules whose purge is an API call (they have no public route
// in restore mode) and returns the function that scales them down again.
func (o *Ops) startForHTTPPurge(ctx context.Context, todo []purge) (func(), error) {
	var started []string
	stop := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, id := range started {
			_ = o.scale(cctx, id, 0)
		}
	}
	for _, p := range todo {
		id := p.module.ID
		if m, ok := inHouse[id]; !ok || m.cli || p.module.Manifest.Hooks.Purge != "" || slices.Contains(started, id) {
			continue
		}
		started = append(started, id)
		if err := o.scale(ctx, id, 1); err != nil {
			return stop, fmt.Errorf("start module %s for its purge: %w", id, err)
		}
	}
	poll, wait := o.wait()
	deadline := time.Now().Add(wait)
	for _, id := range started {
		for {
			d, err := o.API.Get(ctx, "Deployment", "mod-"+id, id)
			if err != nil {
				return stop, err
			}
			st, _ := d["status"].(map[string]any)
			if n := fmt.Sprint(st["readyReplicas"]); n != "<nil>" && n != "0" {
				break
			}
			if time.Now().After(deadline) {
				return stop, fmt.Errorf("module %s did not become ready for its purge", id)
			}
			select {
			case <-ctx.Done():
				return stop, ctx.Err()
			case <-time.After(poll):
			}
		}
	}
	return stop, nil
}

func (o *Ops) scale(ctx context.Context, id string, replicas int) error {
	return o.API.MergePatch(ctx, "Deployment", "mod-"+id, id, kube.Object{"spec": map[string]any{"replicas": replicas}})
}

func (o *Ops) httpPurge(ctx context.Context, m store.Module, tokenKey, subject, class string) error {
	if len(m.Manifest.Provides) == 0 {
		return errors.New("the manifest names no service to call")
	}
	sec, err := o.API.Get(ctx, "Secret", m.Namespace, "can-module")
	if err != nil {
		return fmt.Errorf("read the module Secret: %w", err)
	}
	data, _ := sec["data"].(map[string]any)
	enc, _ := data[tokenKey].(string)
	token, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(token) == 0 {
		return fmt.Errorf("the module Secret has no %s", tokenKey)
	}
	url := o.ServiceURL
	if url == nil {
		url = func(ns, service string, port int) string {
			return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service, ns, port)
		}
	}
	body, err := json.Marshal(map[string]string{"subject": subject, "class": class})
	if err != nil {
		return err
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	target := url(m.Namespace, m.Manifest.Provides[0].Service, m.Manifest.Provides[0].Port) + "/v1/purge"
	// A pod that just became ready may not be reachable yet (network rules): retry the connection.
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if attempt < 10 && ctx.Err() == nil {
				poll, _ := o.wait()
				time.Sleep(poll)
				continue
			}
			return err
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("the purge call answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		}
		return nil
	}
}

// purgeCore removes can-core's derived copies that name an erased subject: queued events and
// their deliveries, notifications and queued mail. The audit log is kept (it holds ids only).
func (o *Ops) purgeCore(ctx context.Context, es []backup.Entry) (int, error) {
	total := 0
	for _, e := range es {
		quoted := `"` + e.Subject + `"`
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM events WHERE instr(data_json, ?) > 0`, []any{quoted}},
			{`DELETE FROM notifications WHERE instr(data_json, ?) > 0`, []any{quoted}},
			{`DELETE FROM email_outbox WHERE instr(body, ?) > 0 OR instr(subject, ?) > 0`, []any{e.Subject, e.Subject}},
		} {
			res, err := o.DB.ExecContext(ctx, q.sql, q.args...)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += int(n)
		}
	}
	return total, nil
}

// Complete ends restore mode: nothing queued before the backup is sent again, old sessions are
// gone, and every enabled module is marked for re-apply so the reconcile after the restart puts
// its workloads and routes back.
func (o *Ops) Complete(ctx context.Context) (backup.CompleteReport, error) {
	var rep backup.CompleteReport
	id, err := Pending(o.DBPath)
	if err != nil {
		return rep, err
	}
	mods, err := o.DB.ListModules(ctx)
	if err != nil {
		return rep, err
	}
	for _, m := range mods {
		if m.State == store.StateEnabled {
			rep.Modules = append(rep.Modules, m.ID)
		}
	}
	if id == "" {
		return rep, nil // already completed: a resumed restore
	}
	now := time.Now().Unix()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		// Mail queued before the backup may have been sent since; it is not sent a second time.
		{`UPDATE email_outbox SET status='dead', last_error='discarded by restore', finished_at=?, recipient='', reply_to='', subject='', body='' WHERE status='pending'`, []any{now}},
		{`UPDATE notify_deliveries SET status='suppressed', detail='discarded by restore' WHERE status='queued'`, nil},
		{`DELETE FROM sessions`, nil},
		{`DELETE FROM login_tickets`, nil},
		{`DELETE FROM ui_launch_sessions`, nil},
		{`UPDATE modules SET settings_hash='' WHERE state='enabled'`, nil},
	} {
		if _, err := o.DB.ExecContext(ctx, q.sql, q.args...); err != nil {
			return rep, err
		}
	}
	if o.Audit != nil {
		if _, err := o.Audit.Append(ctx, audit.Entry{Actor: "can-restore", Action: "restore.completed", Target: id}); err != nil {
			return rep, err
		}
	}
	return rep, finish(o.DBPath)
}

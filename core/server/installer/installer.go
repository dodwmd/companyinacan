// Package installer is the real reconcile.ModuleInstaller: it turns an enabled module into a
// namespace (mod-<id>) with NetworkPolicies, quota and RBAC, a per-module database and Secrets, and
// the module's chart rendered and applied with server-side apply, rolling back on failure.
//
// It plays the role of `helm upgrade --install --atomic` without the Helm SDK (research R16): the
// chart is rendered with a Helm-compatible template subset (chart.go), applied object by object,
// waited on, and the previous rendering is restored if the rollout does not become ready.
package installer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/deploy"
)

// Names of the objects can-core owns in every module namespace. A chart may not define them.
const (
	secretModule  = "can-module"
	secretDB      = "can-db"
	secretRelease = "can-release"

	labelModule  = "can.dev/module"
	labelManaged = "app.kubernetes.io/managed-by"

	tokenVolume = "can-token"
	// TokenDir is where the projected `can`-audience token is mounted in module containers.
	TokenDir = "/var/run/secrets/can"
)

// Config configures an Installer.
type Config struct {
	API         kube.API
	ModulesDir  string // directory of module directories (module.yaml, chart/)
	PlatformURL string // CAN_API_URL given to modules
	NodeIP      string // excluded from module egress (templates/module-namespace.yaml)
	Domain      string // company domain: the only parent of a module's public host (<subdomain>.<domain>)
	// RequireAdmission makes Enable refuse to create anything until the can-core ValidatingAdmissionPolicies
	// and their Deny bindings are seen in the cluster (fail closed).
	RequireAdmission bool
	DB               dbprov.Provisioner
	Log              *slog.Logger

	RolloutTimeout time.Duration // default 5m
	Poll           time.Duration // default 2s
	RBACWait       time.Duration // how long to wait for a fresh RoleBinding to take effect, default 30s
}

// Installer implements reconcile.ModuleInstaller.
type Installer struct {
	cfg Config

	mu        sync.Mutex
	rendering map[string]bool // modules with a render still running (at most one each)
}

// New returns an Installer.
func New(cfg Config) *Installer {
	if cfg.RolloutTimeout == 0 {
		cfg.RolloutTimeout = 5 * time.Minute
	}
	if cfg.Poll == 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.RBACWait == 0 {
		cfg.RBACWait = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Installer{cfg: cfg, rendering: map[string]bool{}}
}

var (
	idRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	nodeIPRE = regexp.MustCompile(`^[0-9a-fA-F.:]{3,45}$`)
)

// quota returns the memory request and limit ceilings of a module namespace: 1.5x the declared
// figures (hook Jobs and rolling updates briefly run extra pods) plus fixed headroom.
func quota(ramMB, limitMB int) (requests, limits string) {
	if limitMB == 0 {
		limitMB = ramMB * 3 / 2
	}
	return fmt.Sprintf("%dMi", ramMB*3/2+32), fmt.Sprintf("%dMi", limitMB*3/2+64)
}

// Enable implements reconcile.ModuleInstaller: idempotent install-or-upgrade.
func (in *Installer) Enable(ctx context.Context, s reconcile.Spec) error {
	if !idRE.MatchString(s.ID) || s.Namespace != "mod-"+s.ID || s.ServiceAccount == "" {
		return fmt.Errorf("refusing module %q: namespace must be mod-<id> (got %q)", s.ID, s.Namespace)
	}
	if s.Manifest == nil {
		return errors.New("no manifest")
	}
	// 1. Render and validate before touching the cluster.
	objs, err := in.renderBounded(ctx, s)
	if err != nil {
		return err
	}
	if err := in.checkAdmission(ctx); err != nil {
		return err
	}
	// 2. Namespace, RBAC, policies, quota.
	if err := in.ensureNamespace(ctx, s); err != nil {
		return err
	}
	// 3. Database and Secrets.
	if err := in.ensureSecrets(ctx, s); err != nil {
		return err
	}
	// 4. The module's workloads, atomically.
	return in.rollout(ctx, s, objs)
}

// Disable implements reconcile.ModuleInstaller: scales workloads to zero, keeps everything else.
func (in *Installer) Disable(ctx context.Context, id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid module id %q", id)
	}
	ns, sel := "mod-"+id, labelModule+"="+id+","+labelManaged+"=can-core"
	if _, err := in.cfg.API.Get(ctx, "Namespace", "", ns); kube.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	for _, kind := range []string{"Deployment", "StatefulSet"} {
		items, err := in.cfg.API.List(ctx, kind, ns, sel)
		if err != nil {
			return err
		}
		for _, o := range items {
			if err := in.cfg.API.MergePatch(ctx, kind, ns, nameOf(o), kube.Object{"spec": map[string]any{"replicas": 0}}); err != nil {
				return err
			}
		}
	}
	crons, err := in.cfg.API.List(ctx, "CronJob", ns, sel)
	if err != nil {
		return err
	}
	for _, o := range crons {
		if err := in.cfg.API.MergePatch(ctx, "CronJob", ns, nameOf(o), kube.Object{"spec": map[string]any{"suspend": true}}); err != nil {
			return err
		}
	}
	// Running one-off Jobs are removed (they would keep a pod alive); their PVCs and Secrets stay.
	jobs, err := in.cfg.API.List(ctx, "Job", ns, sel)
	if err != nil {
		return err
	}
	for _, o := range jobs {
		if err := in.cfg.API.Delete(ctx, "Job", ns, nameOf(o)); err != nil {
			return err
		}
	}
	return nil
}

func nameOf(o kube.Object) string {
	m, _ := o["metadata"].(map[string]any)
	n, _ := m["name"].(string)
	return n
}

func nsOf(o kube.Object) string {
	m, _ := o["metadata"].(map[string]any)
	n, _ := m["namespace"].(string)
	return n
}

// ---- namespace -------------------------------------------------------------------------------

func (in *Installer) template(name string, repl map[string]string) ([]kube.Object, error) {
	b, err := deploy.Templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, err
	}
	pairs := make([]string, 0, len(repl)*2)
	for k, v := range repl {
		pairs = append(pairs, "${"+k+"}", v)
	}
	text := strings.NewReplacer(pairs...).Replace(string(b))
	if strings.Contains(text, "${") {
		return nil, fmt.Errorf("template %s has an unsubstituted variable", name)
	}
	dec := yaml.NewDecoder(strings.NewReader(text))
	var out []kube.Object
	for {
		var o kube.Object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(o) > 0 {
			out = append(out, o)
		}
	}
}

func setLabels(o kube.Object, labels map[string]string) {
	m, _ := o["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		o["metadata"] = m
	}
	l, _ := m["labels"].(map[string]any)
	if l == nil {
		l = map[string]any{}
		m["labels"] = l
	}
	for k, v := range labels {
		l[k] = v
	}
}

func (in *Installer) ensureNamespace(ctx context.Context, s reconcile.Spec) error {
	if !nodeIPRE.MatchString(in.cfg.NodeIP) {
		return fmt.Errorf("CAN_NODE_IP %q is not an IP address", in.cfg.NodeIP)
	}
	req, lim := quota(s.Manifest.Resources.RAMMB, s.Manifest.Resources.RAMLimitMB)
	repl := map[string]string{"MODULE_NAMESPACE": s.Namespace, "NODE_IP": in.cfg.NodeIP, "QUOTA_REQUESTS_MEMORY": req, "QUOTA_LIMITS_MEMORY": lim}
	objs, err := in.template("module-namespace.yaml", repl)
	if err != nil {
		return err
	}
	rbac, err := in.template("module-rbac.yaml", repl)
	if err != nil {
		return err
	}
	labels := map[string]string{labelManaged: "can-core", labelModule: s.ID}
	// Namespace and RoleBinding are created, never patched: can-core's RBAC grants create only
	// (deploy/base/rbac.yaml), so labels on an existing namespace can never be relaxed through it.
	for _, o := range append(objs[:1:1], rbac...) {
		setLabels(o, labels)
		created, err := in.cfg.API.Create(ctx, o)
		if err != nil {
			return fmt.Errorf("creating %v %s: %w", o["kind"], nameOf(o), err)
		}
		if !created { // already there: adopt it only if it is what we would have made
			if err := in.verifyAdopted(ctx, o, s); err != nil {
				return err
			}
		}
	}
	// Policies, limits and quota are applied (kept current on every enable). The first call can
	// race the RoleBinding becoming effective, so it is retried while it is forbidden.
	for i, o := range objs[1:] {
		setLabels(o, labels)
		if i == 0 {
			if err := in.retryForbidden(ctx, func() error { return in.cfg.API.Apply(ctx, o) }); err != nil {
				return fmt.Errorf("applying %v %s: %w", o["kind"], nameOf(o), err)
			}
			continue
		}
		if err := in.cfg.API.Apply(ctx, o); err != nil {
			return fmt.Errorf("applying %v %s: %w", o["kind"], nameOf(o), err)
		}
	}
	sa := kube.Object{"apiVersion": "v1", "kind": "ServiceAccount", "automountServiceAccountToken": false,
		"metadata": map[string]any{"name": s.ServiceAccount, "namespace": s.Namespace}}
	setLabels(sa, labels)
	return in.cfg.API.Apply(ctx, sa)
}

func (in *Installer) retryForbidden(ctx context.Context, f func() error) error {
	deadline := time.Now().Add(in.cfg.RBACWait)
	for {
		err := f()
		if !kube.IsForbidden(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(in.cfg.Poll):
		}
	}
}

// ---- secrets and database ----------------------------------------------------------------------

func randomPassword() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func secret(ns, name, module string, data map[string]string) kube.Object {
	sd := make(map[string]any, len(data))
	for k, v := range data {
		sd[k] = v
	}
	o := kube.Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "stringData": sd,
		"metadata": map[string]any{"name": name, "namespace": ns}}
	setLabels(o, map[string]string{labelManaged: "can-core", labelModule: module})
	return o
}

// existingSecretKey reads one key of a Secret as the API returns it (base64 under data).
func (in *Installer) existingSecretKey(ctx context.Context, ns, name, key string) string {
	o, err := in.cfg.API.Get(ctx, "Secret", ns, name)
	if err != nil {
		return ""
	}
	d, _ := o["data"].(map[string]any)
	enc, _ := d[key].(string)
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return ""
	}
	return string(b)
}

func (in *Installer) ensureSecrets(ctx context.Context, s reconcile.Spec) error {
	mod := map[string]string{
		"CAN_API_URL": in.cfg.PlatformURL, "CAN_MODULE_ID": s.ID, "CAN_WEBHOOK_SECRET": s.HMACSecret,
		"CAN_TOKEN_FILE": TokenDir + "/token", "CAN_TOKEN_AUDIENCE": "can",
	}
	kind := s.Manifest.Requires.Database
	if kind == "postgres" || kind == "mariadb" {
		prov := in.cfg.DB
		if prov == nil {
			prov = dbprov.Unconfigured{}
		}
		pw := in.existingSecretKey(ctx, s.Namespace, secretDB, "DB_PASSWORD")
		if pw == "" {
			pw = randomPassword()
		}
		c, err := prov.Ensure(ctx, kind, s.ID, pw)
		if err != nil {
			return fmt.Errorf("creating %s database: %w", kind, err)
		}
		db := map[string]string{
			"DB_KIND": c.Kind, "DB_HOST": c.Host, "DB_PORT": fmt.Sprint(c.Port), "DB_NAME": c.Database,
			"DB_USER": c.User, "DB_PASSWORD": c.Password, "DATABASE_URL": c.URL(),
		}
		if err := in.cfg.API.Apply(ctx, secret(s.Namespace, secretDB, s.ID, db)); err != nil {
			return fmt.Errorf("writing database Secret: %w", err)
		}
	}
	return in.cfg.API.Apply(ctx, secret(s.Namespace, secretModule, s.ID, mod))
}

// ---- rendering ---------------------------------------------------------------------------------

// moduleDir returns the module's directory, refusing ids or paths that leave ModulesDir.
func (in *Installer) moduleDir(id string) (string, error) {
	for _, d := range []string{id, "_" + id} { // `_id` directories are examples (validate.ManifestPath)
		p := filepath.Join(in.cfg.ModulesDir, d) // id matched idRE: no separators
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no module directory for %q under %s", id, in.cfg.ModulesDir)
}

func (in *Installer) values(ctx context.Context, s reconcile.Spec, dir string) (map[string]any, error) {
	can := map[string]any{
		"apiUrl": in.cfg.PlatformURL, "namespace": s.Namespace, "serviceAccount": s.ServiceAccount,
		"moduleSecret": secretModule, "databaseSecret": secretDB, "tokenDir": TokenDir,
	}
	settings := s.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	out := map[string]any{"settings": settings}
	if vf := s.Manifest.Runtime.Values; vf != "" {
		rootDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return nil, err
		}
		b, err := readInside(rootDir, filepath.Join(rootDir, filepath.Clean("/"+vf))) // cannot escape the module directory
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", vf, err)
		}
		vst := &renderState{ctx: ctx, deadline: time.Now().Add(renderTimeout)}
		t := template.New("values").Option("missingkey=zero")
		t.Funcs(funcs(t, vst))
		t.Funcs(template.FuncMap{"tick": vst.tick})
		if _, err := t.Parse(string(b)); err != nil {
			return nil, fmt.Errorf("%s does not parse", vf)
		}
		if err := instrumentRanges(t, vst); err != nil {
			return nil, err
		}
		buf := &limitWriter{max: maxObjectBytes}
		input := map[string]any{
			"Settings": settings, "Can": map[string]any{"APIURL": can["apiUrl"], "Namespace": s.Namespace, "ServiceAccount": s.ServiceAccount, "SecretName": secretModule, "DBSecretName": secretDB, "TokenDir": TokenDir},
			"Module": map[string]any{"ID": s.ID, "Version": s.Version},
		}
		if err := t.Execute(buf, input); err != nil {
			return nil, fmt.Errorf("%s: %w", vf, err)
		}
		var mapped map[string]any
		if err := yaml.Unmarshal([]byte(strings.ReplaceAll(buf.buf.String(), "<no value>", "")), &mapped); err != nil {
			return nil, fmt.Errorf("%s is not valid YAML after rendering: %w", vf, err)
		}
		out = deepMerge(out, mapped)
	}
	if h := in.hostFor(s); h != "" { // last, so no values file can override the host can-core validates routes against
		out = deepMerge(out, map[string]any{"ingress": map[string]any{"host": h}})
	}
	out["can"] = can
	return out, nil
}

// protected object names that only can-core may write in a module namespace.
func (in *Installer) reserved(s reconcile.Spec) map[string]bool {
	r := map[string]bool{
		"Secret/" + secretModule: true, "Secret/" + secretDB: true, "Secret/" + secretRelease: true,
		"ServiceAccount/" + s.ServiceAccount: true,
		"ResourceQuota/ceiling":              true, "LimitRange/defaults": true,
	}
	for _, n := range []string{"default-deny", "allow-ingress-from-traefik", "allow-ingress-from-can-core", "module-egress"} {
		r["NetworkPolicy/"+n] = true
	}
	return r
}

func (in *Installer) render(ctx context.Context, s reconcile.Spec) ([]kube.Object, error) {
	dir, err := in.moduleDir(s.ID)
	if err != nil {
		return nil, err
	}
	chart := filepath.Join(dir, filepath.Clean("/"+s.Manifest.Runtime.Chart))
	vals, err := in.values(ctx, s, dir)
	if err != nil {
		return nil, err
	}
	objs, err := RenderChartContext(ctx, chart, vals, ReleaseInfo{Name: s.ID, Namespace: s.Namespace})
	if err != nil {
		return nil, fmt.Errorf("rendering chart: %w", err)
	}
	reserved := in.reserved(s)
	seen := map[string]bool{}
	for _, o := range objs {
		kind, _ := o["kind"].(string)
		name := nameOf(o)
		if name == "" {
			return nil, fmt.Errorf("chart object %s has no name", kind)
		}
		if err := in.checkObject(o, s); err != nil {
			return nil, err
		}
		if ns := nsOf(o); ns != "" && ns != s.Namespace {
			return nil, fmt.Errorf("chart object %s/%s targets namespace %q; only %q is allowed", kind, name, ns, s.Namespace)
		}
		if reserved[kind+"/"+name] {
			return nil, fmt.Errorf("chart may not define %s %s: can-core owns it", kind, name)
		}
		if seen[kind+"/"+name] {
			return nil, fmt.Errorf("chart defines %s %s twice", kind, name)
		}
		seen[kind+"/"+name] = true
		m := o["metadata"].(map[string]any)
		m["namespace"] = s.Namespace
		setLabels(o, map[string]string{labelManaged: "can-core", labelModule: s.ID})
	}
	r := newRefs(objs, s)
	for _, o := range objs {
		if err := in.hardenWorkload(o, s, r); err != nil {
			return nil, fmt.Errorf("%v %s: %w", o["kind"], nameOf(o), err)
		}
	}
	return objs, nil
}

func podSpec(o kube.Object) map[string]any {
	var path []string
	switch o["kind"] {
	case "Deployment", "StatefulSet", "Job":
		path = []string{"spec", "template", "spec"}
	case "CronJob":
		path = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		return nil
	}
	cur := map[string]any(o)
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	return cur
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// ---- rollout, rollback, prune --------------------------------------------------------------------

func key(o kube.Object) string { return fmt.Sprint(o["kind"]) + "/" + nameOf(o) }

func (in *Installer) rollout(ctx context.Context, s reconcile.Spec, objs []kube.Object) error {
	prev, err := in.loadRelease(ctx, s)
	if err != nil {
		return fmt.Errorf("the recorded release in Secret %s is unreadable or fails its HMAC check (%w); inspect it or delete it to reinstall", secretRelease, err)
	}
	if err := in.applyAll(ctx, objs); err != nil {
		in.restore(ctx, s, prev, objs)
		return fmt.Errorf("applying chart (rolled back): %w", err)
	}
	if err := in.waitReady(ctx, s.Namespace, objs); err != nil {
		in.restore(ctx, s, prev, objs)
		if prev != nil {
			return fmt.Errorf("%w; rolled back to the previous release", err)
		}
		return fmt.Errorf("%w; removed the new workloads (data kept)", err)
	}
	// Success: prune what the new release no longer has (never data), then record it.
	if prev != nil {
		have := map[string]bool{}
		for _, o := range objs {
			have[key(o)] = true
		}
		for _, o := range prev {
			kind := fmt.Sprint(o["kind"])
			if have[key(o)] || kind == "PersistentVolumeClaim" || kind == "Secret" {
				continue
			}
			if err := in.deleteOwned(ctx, s, kind, nameOf(o)); err != nil {
				in.cfg.Log.Warn("pruning removed chart object failed", "module", s.ID, "object", key(o), "error", err)
			}
		}
	}
	if err := in.saveRelease(ctx, s, objs); err != nil {
		return fmt.Errorf("recording release: %w", err)
	}
	return nil
}

var applyOrder = map[string]int{"ServiceAccount": 0, "ConfigMap": 1, "Secret": 1, "PersistentVolumeClaim": 2, "Service": 3, "NetworkPolicy": 3,
	"Ingress": 3, "IngressRoute": 3, "Middleware": 3, "ResourceQuota": 3, "LimitRange": 3, "Job": 5, "Deployment": 6, "StatefulSet": 6, "CronJob": 7}

func (in *Installer) applyAll(ctx context.Context, objs []kube.Object) error {
	sorted := append([]kube.Object(nil), objs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return applyOrder[fmt.Sprint(sorted[i]["kind"])] < applyOrder[fmt.Sprint(sorted[j]["kind"])]
	})
	for _, o := range sorted {
		if err := in.cfg.API.Apply(ctx, o); err != nil {
			return fmt.Errorf("%s: %w", key(o), err)
		}
	}
	return nil
}

func workloads(objs []kube.Object) []kube.Object {
	var out []kube.Object
	for _, o := range objs {
		if k := o["kind"]; k == "Deployment" || k == "StatefulSet" {
			out = append(out, o)
		}
	}
	return out
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

func (in *Installer) waitReady(ctx context.Context, ns string, objs []kube.Object) error {
	deadline := time.Now().Add(in.cfg.RolloutTimeout)
	for _, w := range workloads(objs) {
		kind, name := fmt.Sprint(w["kind"]), nameOf(w)
		for {
			cur, err := in.cfg.API.Get(ctx, kind, ns, name)
			if err == nil {
				spec, _ := cur["spec"].(map[string]any)
				st, _ := cur["status"].(map[string]any)
				md, _ := cur["metadata"].(map[string]any)
				want := num(spec["replicas"])
				if num(st["observedGeneration"]) >= num(md["generation"]) && num(st["readyReplicas"]) >= want && num(st["updatedReplicas"]) >= want {
					break
				}
			} else if !kube.IsNotFound(err) {
				return err
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s %s not ready after %s", kind, name, in.cfg.RolloutTimeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(in.cfg.Poll):
			}
		}
	}
	return nil
}

// restore puts the previous release back, or removes the new workloads after a failed first install.
// It uses a fresh context: the caller's may be what failed.
func (in *Installer) restore(ctx context.Context, s reconcile.Spec, prev, failed []kube.Object) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), in.cfg.RolloutTimeout)
	defer cancel()
	if prev != nil {
		if err := in.applyAll(rctx, prev); err != nil {
			in.cfg.Log.Error("rollback apply failed", "module", s.ID, "error", err)
			return
		}
		keep := map[string]bool{}
		for _, o := range prev {
			keep[key(o)] = true
		}
		for _, o := range failed { // objects only the failed release had
			if kind := fmt.Sprint(o["kind"]); !keep[key(o)] && kind != "PersistentVolumeClaim" && kind != "Secret" {
				_ = in.deleteOwned(rctx, s, kind, nameOf(o))
			}
		}
		return
	}
	for _, o := range failed {
		switch kind := fmt.Sprint(o["kind"]); kind {
		case "Deployment", "StatefulSet", "Job", "CronJob", "Service", "Ingress", "IngressRoute", "Middleware":
			if err := in.deleteOwned(rctx, s, kind, nameOf(o)); err != nil {
				in.cfg.Log.Error("cleanup after failed install", "module", s.ID, "object", key(o), "error", err)
			}
		}
	}
}

// The release record is a Secret holding the last good rendering (gzip + base64 in one key).
func (in *Installer) saveRelease(ctx context.Context, s reconcile.Spec, objs []kube.Object) error {
	return in.saveReleaseObjects(ctx, s, objs)
}

func (in *Installer) saveReleaseObjects(ctx context.Context, s reconcile.Spec, objs []kube.Object) error {
	raw, err := json.Marshal(objs)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	mac, err := releaseMAC(s, raw)
	if err != nil {
		return err
	}
	return in.cfg.API.Apply(ctx, secret(s.Namespace, secretRelease, s.ID, map[string]string{
		"objects": base64.StdEncoding.EncodeToString(buf.Bytes()), "version": s.Version, "hmac": mac,
	}))
}

func (in *Installer) loadRelease(ctx context.Context, s reconcile.Spec) ([]kube.Object, error) {
	enc := in.existingSecretKey(ctx, s.Namespace, secretRelease, "objects")
	if enc == "" {
		return nil, nil
	}
	gz, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 16<<20))
	if err != nil {
		return nil, err
	}
	want, err := releaseMAC(s, raw)
	if err != nil {
		return nil, err
	}
	if have := in.existingSecretKey(ctx, s.Namespace, secretRelease, "hmac"); !hmac.Equal([]byte(have), []byte(want)) {
		return nil, errors.New("HMAC does not match (the record was not written by this can-core for this module)")
	}
	var objs []kube.Object
	if err := json.Unmarshal(raw, &objs); err != nil {
		return nil, err
	}
	// The record is replayed on rollback, so it gets the same scrutiny as a fresh render.
	for _, o := range objs {
		if err := in.checkObject(o, s); err != nil {
			return nil, err
		}
		if ns := nsOf(o); ns != s.Namespace {
			return nil, fmt.Errorf("%s targets namespace %q", key(o), ns)
		}
	}
	return objs, nil
}

// deleteOwned deletes a chart object only if it carries this module's labels: a name collision with
// something else in the namespace (or a record that names a foreign object) must not delete it.
func (in *Installer) deleteOwned(ctx context.Context, s reconcile.Spec, kind, name string) error {
	if kind == "PersistentVolumeClaim" || kind == "Secret" || kind == "Namespace" {
		return nil // data and credentials are never deleted by the installer
	}
	cur, err := in.cfg.API.Get(ctx, kind, s.Namespace, name)
	if kube.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	l := asMap(asMap(cur["metadata"])["labels"])
	if l[labelModule] != s.ID || l[labelManaged] != "can-core" {
		in.cfg.Log.Warn("not deleting an object without this module's labels", "module", s.ID, "object", kind+"/"+name)
		return nil
	}
	return in.cfg.API.Delete(ctx, kind, s.Namespace, name)
}

var _ reconcile.ModuleInstaller = (*Installer)(nil)

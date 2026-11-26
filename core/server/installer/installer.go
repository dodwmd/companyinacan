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
	"crypto/sha256"
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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/deploy"
)

// Names of the objects can-core owns in every module namespace. A chart may not define them.
const (
	secretModule  = "can-module"
	secretDB      = reconcile.DatabaseSecret
	secretRelease = "can-release"
	secretSMTP    = "can-smtp"

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
	// RenderExe is the binary re-executed as `render-chart` (default: this executable).
	RenderExe string
	// ChildAddressSpaceMB and ChildHeapMB limit the render child (defaults 256 extra and 32; negative AS = none).
	ChildAddressSpaceMB, ChildHeapMB int
	Domain                           string // company domain: the only parent of a module's public host (<subdomain>.<domain>)
	// RequireAdmission makes Enable refuse to create anything until the can-core ValidatingAdmissionPolicies
	// and their Deny bindings are seen in the cluster (fail closed).
	RequireAdmission bool
	DB               dbprov.Provisioner
	Log              *slog.Logger
	// SecretsFor lists the secrets can-core generates into a module's `can-module` Secret
	// (default modsecrets.For). Values are generated once and kept; see ensureSecrets.
	SecretsFor func(module string) []modsecrets.Spec
	// Relay returns the SMTP relay of the company being applied, or nil when none is usable. It is
	// asked only for a wrapped app that takes it (modsecrets.WantsRelay) and that the company has
	// opted in (reconcile.RelayOptIn).
	Relay func(s reconcile.Spec) (*SMTPRelay, error)

	RolloutTimeout time.Duration // default 5m
	Poll           time.Duration // default 2s
	RBACWait       time.Duration // how long to wait for a fresh RoleBinding to take effect, default 30s
}

// SMTPRelay is what a wrapped app needs to speak SMTP itself.
type SMTPRelay struct {
	Host                     string
	Port                     int
	Username, Password, From string
	// Encryption is `tls` (STARTTLS) or `ssl` (implicit TLS), the names PHP mailers use.
	Encryption string
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
	if cfg.SecretsFor == nil {
		cfg.SecretsFor = modsecrets.For
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
	if err := in.checkRotations(s); err != nil {
		return err
	}
	// 1. Plan the routes, render and validate before touching the cluster.
	plan, err := in.planRoutes(ctx, s)
	if err != nil {
		return err
	}
	s.Values = plan.values
	objs, err := in.renderBounded(ctx, s)
	if err != nil {
		return err
	}
	ports, err := routedPorts(objs, plan)
	if err != nil {
		return err
	}
	if err := in.checkAdmission(ctx, len(ports) > 0); err != nil {
		return err
	}
	// 2. Namespace, RBAC, policies, quota.
	policy, err := in.ensureNamespace(ctx, s)
	if err != nil {
		return err
	}
	// 3. Database and Secrets.
	version, err := in.ensureSecrets(ctx, s)
	if err != nil {
		return err
	}
	stampSecretVersion(objs, version)
	stampExposure(objs, plan.values)
	// 4. Routes that are no longer wanted go first.
	if err := in.pruneRoutes(ctx, s.ID, plan.objects); err != nil {
		return err
	}
	if !s.Expose.PublicOn() {
		if err := in.closePublic(ctx, s); err != nil {
			return err
		}
	}
	// 5. The module's workloads, atomically.
	if err := in.rollout(ctx, s, objs); err != nil {
		return err
	}
	// 6. Only a module that runs is opened to the ingress controller, and only on routed ports.
	return in.applyRoutes(ctx, s, traefikPolicy(policy, ports), plan)
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
	if err := in.closeRoutes(ctx, id); err != nil {
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

// ensureNamespace creates the namespace and applies its policies. The one that admits the ingress
// controller is only created closed; the template is returned for applyRoutes.
func (in *Installer) ensureNamespace(ctx context.Context, s reconcile.Spec) (kube.Object, error) {
	if !nodeIPRE.MatchString(in.cfg.NodeIP) {
		return nil, fmt.Errorf("CAN_NODE_IP %q is not an IP address", in.cfg.NodeIP)
	}
	req, lim := quota(s.Manifest.Resources.RAMMB, s.Manifest.Resources.RAMLimitMB)
	repl := map[string]string{"MODULE_NAMESPACE": s.Namespace, "NODE_IP": in.cfg.NodeIP, "QUOTA_REQUESTS_MEMORY": req, "QUOTA_LIMITS_MEMORY": lim}
	objs, err := in.template("module-namespace.yaml", repl)
	if err != nil {
		return nil, err
	}
	rbac, err := in.template("module-rbac.yaml", repl)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{labelManaged: "can-core", labelModule: s.ID}
	// Namespace and RoleBinding are created, never patched: can-core's RBAC grants create only
	// (deploy/base/rbac.yaml), so labels on an existing namespace can never be relaxed through it.
	for _, o := range append(objs[:1:1], rbac...) {
		setLabels(o, labels)
		created, err := in.cfg.API.Create(ctx, o)
		if err != nil {
			return nil, fmt.Errorf("creating %v %s: %w", o["kind"], nameOf(o), err)
		}
		if !created { // already there: adopt it only if it is what we would have made
			if err := in.verifyAdopted(ctx, o, s); err != nil {
				return nil, err
			}
		}
	}
	// Policies, limits and quota are applied (kept current on every enable). The first call can
	// race the RoleBinding becoming effective, so it is retried while it is forbidden.
	var traefik kube.Object
	for i, o := range objs[1:] {
		setLabels(o, labels)
		if o["kind"] == "NetworkPolicy" && nameOf(o) == policyTraefik {
			// Created closed if missing and left as it is otherwise (an upgrade must not cut the
			// running release off); applyRoutes opens the routed ports after the rollout.
			traefik = o
			closed, err := closedPolicy(o)
			if err != nil {
				return nil, err
			}
			if _, err := in.cfg.API.Create(ctx, closed); err != nil {
				return nil, fmt.Errorf("creating NetworkPolicy %s: %w", policyTraefik, err)
			}
			continue
		}
		if i == 0 {
			if err := in.retryForbidden(ctx, func() error { return in.cfg.API.Apply(ctx, o) }); err != nil {
				return nil, fmt.Errorf("applying %v %s: %w", o["kind"], nameOf(o), err)
			}
			continue
		}
		if err := in.cfg.API.Apply(ctx, o); err != nil {
			return nil, fmt.Errorf("applying %v %s: %w", o["kind"], nameOf(o), err)
		}
	}
	sa := kube.Object{"apiVersion": "v1", "kind": "ServiceAccount", "automountServiceAccountToken": false,
		"metadata": map[string]any{"name": s.ServiceAccount, "namespace": s.Namespace}}
	setLabels(sa, labels)
	if traefik == nil {
		return nil, fmt.Errorf("template module-namespace.yaml has no NetworkPolicy %s", policyTraefik)
	}
	return traefik, in.cfg.API.Apply(ctx, sa)
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

// secretKey reads one key of a Secret as the API returns it (base64 under data). A Secret or a key
// that does not exist is ("", nil); any other failure (5xx, timeout, forbidden, a value that is not
// base64) is an error, so that a failed read is never mistaken for "not there yet": generating a
// new value over an existing one would make sealed data undecryptable.
func (in *Installer) secretKey(ctx context.Context, ns, name, key string) (string, error) {
	o, err := in.cfg.API.Get(ctx, "Secret", ns, name)
	if kube.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading Secret %s/%s: %w", ns, name, err)
	}
	d, _ := o["data"].(map[string]any)
	enc, _ := d[key].(string)
	if enc == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("secret %s/%s key %s is not valid base64", ns, name, key)
	}
	return string(b), nil
}

// secretVersionAnnotation on a pod template changes whenever the module's Secrets change, which
// rolls the pods: environment variables are read once at start, so without it a rotated secret
// would sit unused in the Secret until the pod happened to restart.
const secretVersionAnnotation = "can.dev/secret-version"

// secretVersion is a short digest of the Secret data (never the data itself). It is keyed with the
// module's webhook secret: the data includes passwords, some chosen by a person (the SMTP relay's),
// and the digest is readable by anyone who can read pods.
func secretVersion(key string, maps ...map[string]string) string {
	h := hmac.New(sha256.New, []byte(key))
	for _, m := range maps {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, "%d:%s=%d:%s;", len(k), k, len(m[k]), m[k])
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// stampSecretVersion records the Secret version on the pod templates of Deployments and
// StatefulSets. Jobs and CronJobs are left alone: a Job's template cannot be changed in place.
func stampSecretVersion(objs []kube.Object, version string) {
	for _, o := range objs {
		if k := o["kind"]; k != "Deployment" && k != "StatefulSet" {
			continue
		}
		spec := asMap(o["spec"])
		tpl := asMap(spec["template"])
		if tpl == nil {
			continue
		}
		md := asMap(tpl["metadata"])
		if md == nil {
			md = map[string]any{}
			tpl["metadata"] = md
		}
		ann := asMap(md["annotations"])
		if ann == nil {
			ann = map[string]any{}
			md["annotations"] = ann
		}
		ann[secretVersionAnnotation] = version
	}
}

func (in *Installer) ensureSecrets(ctx context.Context, s reconcile.Spec) (string, error) {
	mod := map[string]string{
		"CAN_API_URL": in.cfg.PlatformURL, "CAN_MODULE_ID": s.ID, "CAN_WEBHOOK_SECRET": s.HMACSecret,
		"CAN_TOKEN_FILE": TokenDir + "/token", "CAN_TOKEN_AUDIENCE": "can",
	}
	fresh, err := in.generatedSecrets(ctx, s, mod)
	if err != nil {
		return "", err
	}
	if len(fresh) > 0 {
		// Create-if-absent: two can-core processes enabling the same module at once must not
		// overwrite each other's freshly generated values (a keyring replaced after data was sealed
		// loses that data). The loser of the create adopts what the winner wrote.
		created, err := in.cfg.API.Create(ctx, secret(s.Namespace, secretModule, s.ID, mod))
		if err != nil {
			return "", fmt.Errorf("creating the module Secret: %w", err)
		}
		if !created {
			for _, k := range fresh {
				v, err := in.secretKey(ctx, s.Namespace, secretModule, k)
				if err != nil {
					return "", err
				}
				if v != "" {
					mod[k] = v
				}
			}
		}
	}
	var db map[string]string
	kind := s.Manifest.Requires.Database
	if kind == "postgres" || kind == "mariadb" {
		if err := in.allowDatabaseAccess(ctx, s); err != nil {
			return "", err
		}
		prov := in.cfg.DB
		if prov == nil {
			prov = dbprov.Unconfigured{}
		}
		pw, err := in.secretKey(ctx, s.Namespace, secretDB, "DB_PASSWORD")
		if err != nil {
			return "", err
		}
		if pw == "" {
			// Create-if-absent before the database user is set to it: a concurrent can-core that
			// won the create has its password adopted, so the Secret and the database agree.
			pw = randomPassword()
			created, err := in.cfg.API.Create(ctx, secret(s.Namespace, secretDB, s.ID, map[string]string{"DB_PASSWORD": pw}))
			if err != nil {
				return "", fmt.Errorf("creating the database Secret: %w", err)
			}
			if !created {
				won, err := in.secretKey(ctx, s.Namespace, secretDB, "DB_PASSWORD")
				if err != nil {
					return "", err
				}
				if won != "" {
					pw = won
				}
			}
		}
		c, err := prov.Ensure(ctx, kind, s.ID, pw)
		if err != nil {
			return "", fmt.Errorf("creating %s database: %w", kind, err)
		}
		db = map[string]string{
			"DB_KIND": c.Kind, "DB_HOST": c.Host, "DB_PORT": fmt.Sprint(c.Port), "DB_NAME": c.Database,
			"DB_USER": c.User, "DB_PASSWORD": c.Password, "DATABASE_URL": c.URL(),
		}
		if err := in.cfg.API.Apply(ctx, secret(s.Namespace, secretDB, s.ID, db)); err != nil {
			return "", fmt.Errorf("writing database Secret: %w", err)
		}
	}
	if err := in.cfg.API.Apply(ctx, secret(s.Namespace, secretModule, s.ID, mod)); err != nil {
		return "", err
	}
	smtp, err := in.ensureRelaySecret(ctx, s)
	if err != nil {
		return "", err
	}
	if smtp != nil {
		return secretVersion(s.HMACSecret, mod, db, smtp), nil
	}
	return secretVersion(s.HMACSecret, mod, db), nil
}

// dataNamespace holds the database servers.
const dataNamespace = "can-data"

// allowDatabaseAccess admits this module's namespace to the database ports in can-data. The base
// policy there admits can-core only, so a module that declares no database cannot reach a server.
func (in *Installer) allowDatabaseAccess(ctx context.Context, s reconcile.Spec) error {
	o := kube.Object{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": "allow-db-from-" + s.Namespace, "namespace": dataNamespace},
		"spec": map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{
				"from":  []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": s.Namespace}}}},
				"ports": []any{map[string]any{"protocol": "TCP", "port": 5432}, map[string]any{"protocol": "TCP", "port": 3306}},
			}},
		}}
	setLabels(o, map[string]string{labelManaged: "can-core", labelModule: s.ID})
	if err := in.cfg.API.Apply(ctx, o); err != nil {
		return fmt.Errorf("admitting %s to the database in %s: %w", s.Namespace, dataNamespace, err)
	}
	return nil
}

// ensureRelaySecret writes `can-smtp` for a module that speaks SMTP itself and declares email.send.
// The company must opt the module in (reconcile.RelayOptIn). Otherwise, and without a usable relay, the
// Secret is written empty, so credentials that were withdrawn do not stay behind. It is written as `data` (not stringData) so that server-side apply removes dropped keys.
func (in *Installer) ensureRelaySecret(ctx context.Context, s reconcile.Spec) (map[string]string, error) {
	if !modsecrets.WantsRelay(s.ID) || !slices.Contains(s.Manifest.Requires.Capabilities, "email.send") {
		return nil, nil
	}
	vals, err := in.relayValues(s)
	if err != nil {
		return nil, err
	}
	o := secret(s.Namespace, secretSMTP, s.ID, nil)
	delete(o, "stringData")
	data := map[string]any{}
	for k, v := range vals {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	o["data"] = data
	if err := in.cfg.API.Apply(ctx, o); err != nil {
		return nil, fmt.Errorf("writing the email Secret: %w", err)
	}
	return vals, nil
}

// relayValues is what `can-smtp` holds for s: empty without the opt-in or a usable relay.
func (in *Installer) relayValues(s reconcile.Spec) (map[string]string, error) {
	vals := map[string]string{}
	if in.cfg.Relay == nil || !reconcile.RelayOptIn(s.Email, s.ID) {
		return vals, nil
	}
	r, err := in.cfg.Relay(s)
	if err != nil {
		return nil, fmt.Errorf("reading the email relay settings: %w", err)
	}
	if r != nil {
		vals = map[string]string{"host": r.Host, "port": fmt.Sprint(r.Port), "user": r.Username, "password": r.Password, "from": r.From, "encryption": r.Encryption}
	}
	return vals, nil
}

// RelayFingerprint implements reconcile.RelayAware. It is keyed with the module's own secret, like
// the pod's secret version, so the password cannot be compared across installations.
func (in *Installer) RelayFingerprint(_ context.Context, s reconcile.Spec) (string, error) {
	if !modsecrets.WantsRelay(s.ID) || s.Manifest == nil || !slices.Contains(s.Manifest.Requires.Capabilities, "email.send") {
		return "", nil
	}
	vals, err := in.relayValues(s)
	if err != nil || len(vals) == 0 {
		return "", err
	}
	return secretVersion(s.HMACSecret, vals), nil
}

// checkRotations refuses a rotation request that names a key the module has no rotatable secret
// for, before anything in the cluster is touched.
func (in *Installer) checkRotations(s reconcile.Spec) error {
	for _, k := range s.Rotate {
		if k == modsecrets.WebhookKey {
			continue
		}
		ok := false
		for _, sp := range in.cfg.SecretsFor(s.ID) {
			ok = ok || (sp.Key == k && !sp.NoRotate)
		}
		if !ok {
			return fmt.Errorf("secret %q of module %s cannot be rotated (unknown, or rotating it would damage data)", k, s.ID)
		}
	}
	return nil
}

// generatedSecrets adds the module's generated secrets to the can-module Secret data. A value that
// already exists is kept, so enabling twice, upgrading or disabling and re-enabling never changes
// a credential; only a requested rotation does. Values are never logged.
// It returns the keys that did not exist and were generated fresh (not rotations).
func (in *Installer) generatedSecrets(ctx context.Context, s reconcile.Spec, data map[string]string) ([]string, error) {
	var fresh []string
	rotate := map[string]bool{}
	for _, k := range s.Rotate {
		rotate[k] = true
	}
	for _, sp := range in.cfg.SecretsFor(s.ID) {
		cur, err := in.secretKey(ctx, s.Namespace, secretModule, sp.Key)
		if err != nil {
			return nil, err
		}
		switch {
		case cur != "" && rotate[sp.Key] && sp.Kind == modsecrets.Keyring:
			next, err := modsecrets.RotateKeyring(cur)
			if err != nil {
				return nil, fmt.Errorf("rotating %s of module %s: %w", sp.Key, s.ID, err)
			}
			data[sp.Key] = next
		case cur != "" && !rotate[sp.Key]:
			data[sp.Key] = cur
		default:
			data[sp.Key] = modsecrets.Generate(sp.Kind)
			if cur == "" {
				fresh = append(fresh, sp.Key)
			}
		}
	}
	return fresh, nil
}

// ---- rendering ---------------------------------------------------------------------------------

// moduleDir returns the module's directory, refusing ids or paths that leave ModulesDir.
func (in *Installer) moduleDir(id string) (string, error) {
	for _, d := range []string{id, "_" + id} { // `_id` directories are examples (validate.ManifestPath)
		p := filepath.Join(in.cfg.ModulesDir, d) // id matched idRE: no separators
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return filepath.Abs(p) // the render child runs in a temp directory
		}
	}
	return "", fmt.Errorf("no module directory for %q under %s", id, in.cfg.ModulesDir)
}

// protected object names that only can-core may write in a module namespace.
func (in *Installer) reserved(s reconcile.Spec) map[string]bool {
	r := map[string]bool{
		"Secret/" + secretModule: true, "Secret/" + secretDB: true, "Secret/" + secretRelease: true, "Secret/" + secretSMTP: true,
		"ServiceAccount/" + s.ServiceAccount: true,
		"ResourceQuota/ceiling":              true, "LimitRange/defaults": true,
		"IngressRoute/" + routeWeb: true, "IngressRoute/" + siteRouteName(s.ID): true,
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
	objs, err := in.renderInChild(ctx, in.jobFor(s, dir, chart))
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
	if !s.Expose.PublicOn() {
		// A rollback must not put a public route back.
		prev = slices.DeleteFunc(prev, func(o kube.Object) bool { return o["kind"] == "IngressRoute" })
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
	enc, err := in.secretKey(ctx, s.Namespace, secretRelease, "objects")
	if err != nil {
		return nil, err
	}
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
	have, herr := in.secretKey(ctx, s.Namespace, secretRelease, "hmac")
	if herr != nil {
		return nil, herr
	}
	if !hmac.Equal([]byte(have), []byte(want)) {
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

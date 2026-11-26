package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/backup"
)

// RestoreCluster is backup.Cluster over kubectl, run on the server being restored
// (specs/042-restore). The cluster half of every step happens inside can-core
// (`can-core restore ...`); the rest is applying the same manifests `can up` applies.
type RestoreCluster struct {
	Kube      Kube
	Images    ImageSet    // may be empty when CoreImage is set and already on the node
	Loader    ImageLoader // loads Images' tarballs (nil: nothing to load)
	CoreImage string      // default Images.Ref(ImageCore)
	ACMEEmail string
	Timeouts  Timeouts
	// Out receives progress lines.
	Out   io.Writer
	Sleep func(context.Context, time.Duration)

	core CoreOptions // set by Start
}

var _ backup.Cluster = (*RestoreCluster)(nil)

func (c *RestoreCluster) runner() *runner {
	t := c.Timeouts
	if t == (Timeouts{}) {
		t = DefaultTimeouts()
	}
	return &runner{o: Options{Timeouts: t, ACMEEmail: c.ACMEEmail}, d: Deps{Kube: c.Kube, Out: c.Out, Sleep: c.Sleep}, res: &Result{}}
}

func (c *RestoreCluster) image() string {
	if c.CoreImage != "" {
		return c.CoreImage
	}
	return c.Images.Ref(ImageCore)
}

// imageSHA is the tarball checksum that goes with image(), "" when the image was given by name.
func (c *RestoreCluster) imageSHA() string {
	if c.CoreImage != "" {
		return ""
	}
	return c.Images.Images[ImageCore].SHA256
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Mode implements backup.Cluster.
func (c *RestoreCluster) Mode(ctx context.Context) (string, error) {
	out, err := c.runner().kube(ctx, nil, "-n", SystemNS, "get", "deployment", "can-core", "--ignore-not-found", "-o", "name")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return "", nil
	}
	s, err := c.exec(ctx, nil, "restore", "status")
	if err != nil {
		return "unknown", nil // deployed but not running: nothing can be concluded
	}
	return strings.TrimSpace(s), nil
}

// Inspect implements backup.Cluster.
func (c *RestoreCluster) Inspect(ctx context.Context) ([]string, error) {
	r := c.runner()
	var found []string
	for _, q := range []struct {
		what string
		args []string
	}{
		{"can-core is deployed", []string{"-n", SystemNS, "get", "deployment", "can-core", "--ignore-not-found", "-o", "name"}},
		{"can-core has data", []string{"-n", SystemNS, "get", "persistentvolumeclaim", "can-core-data", "--ignore-not-found", "-o", "name"}},
		{"the shared Postgres has data", []string{"-n", "can-data", "get", "persistentvolumeclaim", "postgres-data", "--ignore-not-found", "-o", "name"}},
	} {
		out, err := r.kube(ctx, nil, q.args...)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(out) != "" {
			found = append(found, q.what)
		}
	}
	out, err := r.kube(ctx, nil, "get", "namespaces", "-o", "name")
	if err != nil {
		return nil, err
	}
	for _, l := range lines(out) {
		if ns := strings.TrimPrefix(l, "namespace/"); strings.HasPrefix(ns, "mod-") {
			found = append(found, "module namespace "+ns+" exists")
		}
	}
	return found, nil
}

// Halt implements backup.Cluster.
func (c *RestoreCluster) Halt(ctx context.Context) error {
	r := c.runner()
	out, err := r.kube(ctx, nil, "-n", SystemNS, "get", "deployment", "can-core", "--ignore-not-found", "-o", "name")
	if err != nil || strings.TrimSpace(out) == "" {
		return err
	}
	if _, err := r.kube(ctx, nil, "-n", SystemNS, "scale", "deployment/can-core", "--replicas=0"); err != nil {
		return err
	}
	return r.poll(ctx, r.o.Timeouts.Rollout, func() error {
		out, err := r.kube(ctx, nil, "-n", SystemNS, "get", "pods", "-l", "app.kubernetes.io/name=can-core", "-o", "name")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "" {
			return errors.New("can-core is still running")
		}
		return nil
	})
}

// idle waits until no running pod uses the claim: on a server being overwritten the module's old
// pods may still be terminating, and they must not write into what is being restored.
func (c *RestoreCluster) idle(ctx context.Context, r *runner, ref backup.VolumeRef) error {
	return r.poll(ctx, r.o.Timeouts.Rollout, func() error {
		out, err := r.kube(ctx, nil, "-n", ref.Namespace, "get", "pods", "--field-selector=status.phase!=Succeeded,status.phase!=Failed",
			"-o", `jsonpath={range .items[*]}{range .spec.volumes[*]}{.persistentVolumeClaim.claimName}{"\n"}{end}{end}`)
		if err != nil {
			return err
		}
		for _, l := range lines(out) {
			if l == ref.Claim {
				return fmt.Errorf("claim %s is still used by a running pod", ref)
			}
		}
		return nil
	})
}

// restoreCompany are the company.yaml values can-core's manifests need.
func restoreCompany(b []byte) (name, domain string, err error) {
	var c struct {
		Name   string `yaml:"name"`
		Domain string `yaml:"domain"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return "", "", fmt.Errorf("company.yaml: %w", err)
	}
	return c.Name, c.Domain, nil
}

// Start implements backup.Cluster.
func (c *RestoreCluster) Start(ctx context.Context, o backup.StartOptions) error {
	r := c.runner()
	name, domain, err := restoreCompany(o.CompanyYAML)
	if err != nil {
		return err
	}
	c.core = CoreOptions{Image: c.image(), ImageSHA256: c.imageSHA(), CompanyName: name, Domain: domain, CookieSecure: true, RestoreID: o.ID}
	if o.Overwrite {
		c.core.RestoreOverwrite = o.ID
	}
	manifests, err := CoreManifests(c.core)
	if err != nil {
		return err
	}
	if err := r.base(ctx); err != nil {
		return fmt.Errorf("platform base: %w", err)
	}
	if c.Loader != nil && len(c.Images.Upload()) > 0 {
		if err := c.Loader.Load(ctx, c.Images, func(s string) { r.say("      %s", s) }); err != nil {
			return fmt.Errorf("container images: %w", err)
		}
	}
	cm, err := CompanyConfigMap(string(o.CompanyYAML))
	if err != nil {
		return err
	}
	if _, err := r.apply(ctx, []byte(cm), true); err != nil {
		return err
	}
	if _, err := r.apply(ctx, manifests, false); err != nil {
		return err
	}
	if err := c.rollout(ctx, r); err != nil {
		return err
	}
	st, err := c.exec(ctx, nil, "restore", "status")
	if err != nil {
		return err
	}
	if !strings.Contains(st, "restoring "+o.ID) {
		return fmt.Errorf("can-core did not enter restore mode (it says %q): refusing to continue", strings.TrimSpace(st))
	}
	return nil
}

func (c *RestoreCluster) rollout(ctx context.Context, r *runner) error {
	_, err := r.kube(ctx, nil, "-n", SystemNS, "rollout", "status", "deploy/can-core", secs(r.o.Timeouts.Rollout))
	return err
}

// exec runs can-core's own command line in its pod.
func (c *RestoreCluster) exec(ctx context.Context, stdin []byte, args ...string) (string, error) {
	full := []string{"-n", SystemNS, "exec"}
	if stdin != nil {
		full = append(full, "-i")
	}
	full = append(append(full, "deploy/can-core", "--", "/can-core"), args...)
	return c.runner().kube(ctx, stdin, full...)
}

// report runs a `can-core restore` operation and decodes the JSON line it prints, which it also
// prints when it fails.
func (c *RestoreCluster) report(ctx context.Context, in, out any, args ...string) error {
	var stdin []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		stdin = b
	}
	s, err := c.exec(ctx, stdin, append([]string{"restore"}, args...)...)
	derr := errors.New("can-core printed no report")
	for _, l := range lines(s) {
		if strings.HasPrefix(l, "{") {
			derr = json.Unmarshal([]byte(l), out)
		}
	}
	if err != nil {
		return err
	}
	return derr
}

// ApplyModules implements backup.Cluster.
func (c *RestoreCluster) ApplyModules(ctx context.Context, overwrite bool) (backup.ApplyReport, error) {
	var rep backup.ApplyReport
	args := []string{"apply"}
	if overwrite {
		args = append(args, "--overwrite")
	}
	err := c.report(ctx, nil, &rep, args...)
	return rep, err
}

// RunHooks implements backup.Cluster.
func (c *RestoreCluster) RunHooks(ctx context.Context, o backup.HookOptions) (backup.HookReport, error) {
	var rep backup.HookReport
	err := c.report(ctx, o, &rep, "hooks")
	return rep, err
}

// Erase implements backup.Cluster.
func (c *RestoreCluster) Erase(ctx context.Context, es []backup.Entry, accept []string) (backup.EraseReport, error) {
	var rep backup.EraseReport
	err := c.report(ctx, map[string]any{"accept": accept, "entries": es}, &rep, "erase")
	return rep, err
}

var reClaim = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// bindPod is a pod that does nothing but use the claim: local-path creates a volume's directory
// only once a pod that needs it is scheduled.
func bindPod(ref backup.VolumeRef, image, name string) ([]byte, error) {
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": ref.Namespace, "labels": map[string]any{"app.kubernetes.io/name": "can-restore-bind", "app.kubernetes.io/part-of": "companyinacan"}},
		"spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "bind", "image": image, "imagePullPolicy": "IfNotPresent", "args": []any{"version"},
				"resources":       map[string]any{"requests": map[string]any{"cpu": "5m", "memory": "8Mi"}, "limits": map[string]any{"memory": "32Mi"}},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}},
				"volumeMounts":    []any{map[string]any{"name": "v", "mountPath": "/v"}},
			}},
			"volumes": []any{map[string]any{"name": "v", "persistentVolumeClaim": map[string]any{"claimName": ref.Claim}}},
		},
	})
}

// Volumes implements backup.Cluster. Claims that do not exist are left out.
func (c *RestoreCluster) Volumes(ctx context.Context, want []backup.VolumeRef) ([]backup.Volume, error) {
	r := c.runner()
	var out []backup.Volume
	for i, ref := range want {
		if !reClaim.MatchString(ref.Namespace) || !reClaim.MatchString(ref.Claim) {
			return nil, fmt.Errorf("volume %s has a name Kubernetes would not accept", ref)
		}
		have, err := r.kube(ctx, nil, "-n", ref.Namespace, "get", "persistentvolumeclaim", ref.Claim, "--ignore-not-found", "-o", "name")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(have) == "" {
			continue
		}
		if err := c.idle(ctx, r, ref); err != nil {
			return nil, err
		}
		name := fmt.Sprintf("can-restore-bind-%d", i)
		pod, err := bindPod(ref, c.image(), name)
		if err != nil {
			return nil, err
		}
		remove := func() error {
			_, err := r.kube(ctx, nil, "-n", ref.Namespace, "delete", "pod", name, "--ignore-not-found", "--wait=true", secs(r.o.Timeouts.Rollout))
			return err
		}
		if err := remove(); err != nil { // left by an interrupted run
			return nil, err
		}
		if _, err := r.apply(ctx, pod, false); err != nil {
			return nil, fmt.Errorf("bind %s: %w", ref, err)
		}
		var volume string
		err = r.poll(ctx, r.o.Timeouts.Rollout, func() error {
			s, err := r.kube(ctx, nil, "-n", ref.Namespace, "get", "persistentvolumeclaim", ref.Claim, "-o", "jsonpath={.status.phase} {.spec.volumeName}")
			if err != nil {
				return err
			}
			f := strings.Fields(s)
			if len(f) != 2 || f[0] != "Bound" {
				return fmt.Errorf("claim %s is %q", ref, s)
			}
			volume = f[1]
			return nil
		})
		if rerr := remove(); err == nil {
			err = rerr
		}
		if err != nil {
			return nil, fmt.Errorf("create the volume of %s: %w", ref, err)
		}
		dir, err := r.kube(ctx, nil, "get", "persistentvolume", volume, "-o", "jsonpath={.spec.hostPath.path}{.spec.local.path}")
		if err != nil {
			return nil, err
		}
		dir = strings.TrimSpace(dir)
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || len(strings.Split(strings.Trim(dir, "/"), "/")) < 3 {
			return nil, fmt.Errorf("volume %s of %s is not a local directory (%q): only local-path volumes can be restored", volume, ref, dir)
		}
		out = append(out, backup.Volume{VolumeRef: ref, Dir: dir})
	}
	return out, nil
}

// Complete implements backup.Cluster.
func (c *RestoreCluster) Complete(ctx context.Context) error {
	r := c.runner()
	status, err := c.exec(ctx, nil, "restore", "status")
	if err != nil {
		return err
	}
	var rep struct {
		Modules []string `json:"modules"`
	}
	if strings.Contains(status, "restoring") {
		if err := c.report(ctx, nil, &rep, "complete"); err != nil {
			return err
		}
	}
	if c.core.Image == "" { // resumed: Start ran in an earlier process
		out, err := r.kube(ctx, nil, "-n", SystemNS, "get", "configmap", "company", "-o", `jsonpath={.data.company\.yaml}`)
		if err != nil {
			return err
		}
		name, domain, err := restoreCompany([]byte(out))
		if err != nil {
			return err
		}
		c.core = CoreOptions{Image: c.image(), ImageSHA256: c.imageSHA(), CompanyName: name, Domain: domain, CookieSecure: true}
	}
	c.core.RestoreID, c.core.RestoreOverwrite = "", ""
	manifests, err := CoreManifests(c.core)
	if err != nil {
		return err
	}
	if _, err := r.apply(ctx, manifests, false); err != nil {
		return err
	}
	// Always restart: when the manifests are already the normal ones (a resumed restore, or
	// someone re-applied them) applying changes nothing, and the old process would stay in
	// restore mode behind a healthy rollout.
	if _, err := r.kube(ctx, nil, "-n", SystemNS, "rollout", "restart", "deploy/can-core"); err != nil {
		return err
	}
	if err := c.rollout(ctx, r); err != nil {
		return err
	}
	if err := r.poll(ctx, r.o.Timeouts.Rollout, func() error {
		status, err := c.exec(ctx, nil, "restore", "status")
		if err != nil {
			return err
		}
		serving, err := c.exec(ctx, nil, "restore", "serving")
		if err != nil {
			return err
		}
		if !strings.HasPrefix(strings.TrimSpace(status), "normal") || strings.TrimSpace(serving) != "normal" {
			return fmt.Errorf("can-core is still in restore mode (latch %q, serving %q)", strings.TrimSpace(status), strings.TrimSpace(serving))
		}
		return nil
	}); err != nil {
		return fmt.Errorf("can-core did not come back in normal mode: %w", err)
	}
	// The reconcile that now runs puts the modules' workloads and routes back. A module that does
	// not come up is reported, not fatal: the data is restored and can-core keeps retrying it.
	sort.Strings(rep.Modules)
	for _, id := range rep.Modules {
		ns := "mod-" + id
		err := r.poll(ctx, r.o.Timeouts.Module, func() error {
			s, err := r.kube(ctx, nil, "-n", ns, "get", "deployments,statefulsets", "-o", `jsonpath={range .items[*]}{.spec.replicas}/{.status.readyReplicas}{"\n"}{end}`)
			if err != nil {
				return err
			}
			for _, l := range lines(s) {
				want, ready, _ := strings.Cut(l, "/")
				if want == "0" || want != ready {
					return fmt.Errorf("workloads of %s are not ready (%s)", id, l)
				}
			}
			return nil
		})
		if err != nil {
			r.say("WARNING: module %q has not come up yet (%v): check `kubectl -n %s get pods`", id, err, ns)
			continue
		}
		r.say("      module %q is running", id)
	}
	return nil
}

// LocalImageLoader imports image tarballs into the k3s of this machine.
type LocalImageLoader struct{}

// Load implements ImageLoader.
func (LocalImageLoader) Load(ctx context.Context, set ImageSet, progress func(string)) error {
	if err := os.MkdirAll(ImagesDir, 0o755); err != nil { //nolint:gosec // k3s image directory
		return err
	}
	for _, img := range set.Upload() {
		b, err := img.Read() // checksum verified
		if err != nil {
			return err
		}
		dest := filepath.Join(ImagesDir, img.File)
		if err := os.WriteFile(dest, b, 0o600); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "k3s", "ctr", "-n", "k8s.io", "images", "import", dest).CombinedOutput() //nolint:gosec // validated file name
		if err != nil {
			return fmt.Errorf("import %s into k3s: %w: %s", img.Name, err, strings.TrimSpace(string(out)))
		}
		progress("imported " + img.Name)
	}
	return nil
}

package installer

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

const stubDeploy = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: stub
spec:
  replicas: {{ .Values.replicas }}
  selector: {matchLabels: {app: stub}}
  template:
    metadata: {labels: {app: stub}}
    spec:
      containers:
        - name: app
          image: {{ .Values.image }}
`

func fixture(t *testing.T, extra map[string]string) (*Installer, *kube.Fake, *dbprov.Fake, reconcile.Spec) {
	t.Helper()
	files := map[string]string{
		"stub/chart/values.yaml":           "image: busybox:1.37.0\nreplicas: 1\n",
		"stub/chart/templates/deploy.yaml": stubDeploy,
		"stub/chart/templates/svc.yaml":    "apiVersion: v1\nkind: Service\nmetadata:\n  name: stub\nspec:\n  selector: {app: stub}\n  ports: [{port: 80}]\n",
		"stub/chart/templates/pvc.yaml":    "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: data\nspec:\n  accessModes: [ReadWriteOnce]\n  resources: {requests: {storage: 1Gi}}\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	dir := writeChart(t, files)
	api := kube.NewFake()
	db := dbprov.NewFake()
	in := New(Config{API: api, ModulesDir: dir, PlatformURL: "http://can-core.can-system.svc.cluster.local:8090", NodeIP: "172.18.0.2",
		DB: db, RolloutTimeout: 200 * time.Millisecond, Poll: 5 * time.Millisecond, ChildAddressSpaceMB: -1})
	man := &config.Module{ID: "stub", Version: "0.1.0"}
	man.Runtime.Chart = "./chart"
	man.Resources.RAMMB, man.Resources.RAMLimitMB = 20, 40
	man.Requires.Database = "none"
	return in, api, db, reconcile.Spec{ID: "stub", Version: "0.1.0", Namespace: "mod-stub", ServiceAccount: "stub", Manifest: man, HMACSecret: "s3cret", Settings: map[string]any{"greeting": "hi"}}
}

func get(t *testing.T, api *kube.Fake, kind, ns, name string) map[string]any {
	t.Helper()
	o, err := api.Get(context.Background(), kind, ns, name)
	if err != nil {
		t.Fatalf("%s %s/%s: %v", kind, ns, name, err)
	}
	return o
}

func dig(o map[string]any, path ...string) any {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func TestEnableCreatesNamespaceRBACPoliciesAndWorkload(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	ns := get(t, api, "Namespace", "", "mod-stub")
	if dig(ns, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" || dig(ns, "metadata", "labels", "can.dev/namespace-kind") != "module" {
		t.Fatalf("namespace labels: %v", ns)
	}
	rb := get(t, api, "RoleBinding", "mod-stub", "can-core-module")
	if dig(rb, "roleRef", "name") != "can-core-module" {
		t.Fatalf("%v", rb)
	}
	for _, n := range []string{"default-deny", "allow-ingress-from-traefik", "allow-ingress-from-can-core", "module-egress"} {
		get(t, api, "NetworkPolicy", "mod-stub", n)
	}
	eg := get(t, api, "NetworkPolicy", "mod-stub", "module-egress")
	if !strings.Contains(toJSON(eg), "172.18.0.2/32") {
		t.Fatal("node IP not substituted into the egress policy")
	}
	q := get(t, api, "ResourceQuota", "mod-stub", "ceiling")
	if dig(q, "spec", "hard", "requests.memory") == "${QUOTA_REQUESTS_MEMORY}" || dig(q, "spec", "hard", "limits.memory") == nil {
		t.Fatalf("quota not substituted: %v", q)
	}
	get(t, api, "ServiceAccount", "mod-stub", "stub")
	sa := get(t, api, "ServiceAccount", "mod-stub", "stub")
	if dig(sa, "automountServiceAccountToken") != false {
		t.Fatalf("SA must not automount: %v", sa)
	}
	sec := get(t, api, "Secret", "mod-stub", "can-module")
	data := secretData(sec)
	if data["CAN_API_URL"] != "http://can-core.can-system.svc.cluster.local:8090" || data["CAN_WEBHOOK_SECRET"] != "s3cret" || data["CAN_MODULE_ID"] != "stub" {
		t.Fatalf("%v", data)
	}
}

func TestWorkloadIsHardenedAndInjected(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	d := get(t, api, "Deployment", "mod-stub", "stub")
	if dig(d, "metadata", "namespace") != "mod-stub" || dig(d, "metadata", "labels", "can.dev/module") != "stub" || dig(d, "metadata", "labels", "app.kubernetes.io/managed-by") != "can-core" {
		t.Fatalf("labels/ns: %v", d["metadata"])
	}
	ps := dig(d, "spec", "template", "spec").(map[string]any)
	if ps["automountServiceAccountToken"] != false || ps["serviceAccountName"] != "stub" {
		t.Fatalf("%v", ps)
	}
	vols := ps["volumes"].([]any)
	var tok map[string]any
	for _, v := range vols {
		if v.(map[string]any)["name"] == "can-token" {
			tok = v.(map[string]any)
		}
	}
	src := dig(tok, "projected", "sources").([]any)[0].(map[string]any)["serviceAccountToken"].(map[string]any)
	if src["audience"] != "can" || src["path"] != "token" {
		t.Fatalf("token volume: %v", tok)
	}
	c := ps["containers"].([]any)[0].(map[string]any)
	if !strings.Contains(toJSON(c["volumeMounts"]), "/var/run/secrets/can") || !strings.Contains(toJSON(c["envFrom"]), "can-module") {
		t.Fatalf("container not injected: %v", c)
	}
	if dig(d, "spec", "replicas") != float64(1) {
		t.Fatalf("replicas %v", dig(d, "spec", "replicas"))
	}
}

func TestChartCannotEscapeItsNamespaceOrKinds(t *testing.T) {
	cases := map[string]string{
		"other namespace":  "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: kube-system}\n",
		"namespace kind":   "apiVersion: v1\nkind: Namespace\nmetadata: {name: evil}\n",
		"rolebinding kind": "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata: {name: x}\n",
		"unsupported kind": "apiVersion: v1\nkind: Node\nmetadata: {name: x}\n",
		"reserved name":    "apiVersion: v1\nkind: Secret\nmetadata: {name: can-module}\n",
		"reserved release": "apiVersion: v1\nkind: Secret\nmetadata: {name: can-release}\n",
		"reserved sa":      "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: stub}\n",
		"policy by chart":  "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: default-deny}\nspec: {podSelector: {}}\n",
		"quota by chart":   "apiVersion: v1\nkind: ResourceQuota\nmetadata: {name: ceiling}\n",
		"limitrange":       "apiVersion: v1\nkind: LimitRange\nmetadata: {name: defaults}\n",
		"pod directly":     "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\n",
		"hostNetwork":      "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: h}\nspec: {template: {spec: {hostNetwork: true, containers: []}}}\n",
		"privileged":       "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: h}\nspec: {template: {spec: {containers: [{name: a, image: x, securityContext: {privileged: true}}]}}}\n",
		"hostPath":         "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: h}\nspec: {template: {spec: {volumes: [{name: v, hostPath: {path: /}}], containers: []}}}\n",
		"other sa":         "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: h}\nspec: {template: {spec: {serviceAccountName: default, containers: []}}}\n",
	}
	for name, body := range cases {
		in, api, _, spec := fixture(t, map[string]string{"stub/chart/templates/evil.yaml": body})
		err := in.Enable(context.Background(), spec)
		if err == nil {
			t.Errorf("%s: chart object accepted", name)
			continue
		}
		for k := range api.Objects {
			if strings.HasPrefix(k, "Deployment/") || strings.HasPrefix(k, "Service/") {
				t.Errorf("%s: workload applied before validation failed (%s)", name, k)
			}
		}
	}
}

func TestEnableIsIdempotentAndKeepsSecretStable(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	for i := 0; i < 2; i++ {
		if err := in.Enable(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
	}
	creates := 0
	for _, l := range api.Log {
		if strings.HasPrefix(l, "create Namespace") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("namespace created %d times", creates)
	}
}

func TestValuesMapping(t *testing.T) {
	in, api, _, spec := fixture(t, map[string]string{
		"stub/values.tmpl.yaml": "replicas: {{ .Settings.replicas }}\nimage: {{ .Settings.image | quote }}\nwebhook: {{ .Can.Namespace }}\n",
	})
	spec.Manifest.Runtime.Values = "./values.tmpl.yaml"
	spec.Settings = map[string]any{"replicas": 2, "image": "busybox:1.37.0"}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if dig(get(t, api, "Deployment", "mod-stub", "stub"), "spec", "replicas") != float64(2) {
		t.Fatal("values.tmpl.yaml not applied")
	}
}

func TestFailedFirstInstallRemovesWorkloadsButKeepsData(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	api.NotReady["stub"] = true
	err := in.Enable(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("%v", err)
	}
	if _, e := api.Get(context.Background(), "Deployment", "mod-stub", "stub"); !kube.IsNotFound(e) {
		t.Fatal("workload left running after a failed first install")
	}
	get(t, api, "PersistentVolumeClaim", "mod-stub", "data") // data is never deleted
}

func TestFailedUpgradeRollsBackToPreviousRelease(t *testing.T) {
	in, api, _, spec := fixture(t, map[string]string{"stub/values.tmpl.yaml": "image: {{ .Settings.image | default \"busybox:1.37.0\" | quote }}\n"})
	spec.Manifest.Runtime.Values = "./values.tmpl.yaml"
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	spec.Settings = map[string]any{"image": "busybox:bad"}
	api.NotReady["stub"] = true
	err := in.Enable(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("%v", err)
	}
	api.NotReady["stub"] = false
	d := get(t, api, "Deployment", "mod-stub", "stub")
	img := dig(d, "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)["image"]
	if img != "busybox:1.37.0" {
		t.Fatalf("previous release not restored, image=%v", img)
	}
}

func TestUpgradePrunesRemovedObjectsButNeverVolumesOrSecrets(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// Next version of the chart drops the Service and the PVC.
	dir := in.cfg.ModulesDir
	writeFiles(t, dir, map[string]string{"stub/chart/templates/svc.yaml": "", "stub/chart/templates/pvc.yaml": ""})
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, e := api.Get(context.Background(), "Service", "mod-stub", "stub"); !kube.IsNotFound(e) {
		t.Fatal("removed Service not pruned")
	}
	get(t, api, "PersistentVolumeClaim", "mod-stub", "data")
}

func TestDisableScalesToZeroKeepsEverythingElse(t *testing.T) {
	in, api, _, spec := fixture(t, map[string]string{
		"stub/chart/templates/cron.yaml": "apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: tick}\nspec:\n  schedule: '* * * * *'\n  jobTemplate: {spec: {template: {spec: {restartPolicy: Never, containers: [{name: a, image: x}]}}}}\n",
	})
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := in.Disable(context.Background(), "stub"); err != nil {
		t.Fatal(err)
	}
	if dig(get(t, api, "Deployment", "mod-stub", "stub"), "spec", "replicas") != float64(0) {
		t.Fatal("deployment not scaled to zero")
	}
	if dig(get(t, api, "CronJob", "mod-stub", "tick"), "spec", "suspend") != true {
		t.Fatal("cronjob not suspended")
	}
	get(t, api, "PersistentVolumeClaim", "mod-stub", "data")
	get(t, api, "Secret", "mod-stub", "can-module")
	for _, l := range api.Log {
		if strings.HasPrefix(l, "delete PersistentVolumeClaim") || strings.HasPrefix(l, "delete Secret") || strings.HasPrefix(l, "delete Namespace") {
			t.Fatalf("disable deleted data: %s", l)
		}
	}
	// Re-enable brings the replicas back even though the chart default never mentioned suspend.
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if dig(get(t, api, "Deployment", "mod-stub", "stub"), "spec", "replicas") != float64(1) {
		t.Fatal("re-enable did not restore replicas")
	}
	if dig(get(t, api, "CronJob", "mod-stub", "tick"), "spec", "suspend") != false {
		t.Fatal("re-enable did not resume the cronjob")
	}
}

func TestDisableOfUnknownModuleIsANoop(t *testing.T) {
	in, _, _, _ := fixture(t, nil)
	if err := in.Disable(context.Background(), "never-installed"); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseProvisioningInjectsCredentialsAndKeepsPassword(t *testing.T) {
	in, api, db, spec := fixture(t, nil)
	spec.Manifest.Requires.Database = "postgres"
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	sec := get(t, api, "Secret", "mod-stub", "can-db")
	data := secretData(sec)
	pw, _ := data["DB_PASSWORD"].(string)
	if data["DB_NAME"] != "mod_stub" || data["DB_USER"] != "mod_stub" || len(pw) < 32 || !strings.HasPrefix(data["DATABASE_URL"].(string), "postgres://mod_stub:") {
		t.Fatalf("%v", data)
	}
	if db.Databases["postgres/mod_stub"] != pw {
		t.Fatal("provisioner got a different password than the Secret")
	}
	c := dig(get(t, api, "Deployment", "mod-stub", "stub"), "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)
	if !strings.Contains(toJSON(c["envFrom"]), "can-db") {
		t.Fatal("DB secret not injected")
	}
	// The existing Secret is read back, so a re-enable keeps the password.
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if db.Databases["postgres/mod_stub"] != pw {
		t.Fatal("password rotated on re-enable")
	}
}

func TestDatabaseFailureAbortsBeforeWorkloads(t *testing.T) {
	in, api, db, spec := fixture(t, nil)
	spec.Manifest.Requires.Database = "postgres"
	db.Fail = errors.New("postgres down")
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("expected failure")
	}
	if _, e := api.Get(context.Background(), "Deployment", "mod-stub", "stub"); !kube.IsNotFound(e) {
		t.Fatal("workload applied without a database")
	}
}

func TestRBACPropagationIsRetried(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.Poll = time.Millisecond
	api.ForbiddenFirst = 3
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatalf("a briefly forbidden first write must be retried: %v", err)
	}
	in, api, _, spec = fixture(t, nil)
	in.cfg.Poll, in.cfg.RBACWait = time.Millisecond, 20*time.Millisecond
	api.ForbiddenFirst = 1 << 30
	if err := in.Enable(context.Background(), spec); !kube.IsForbidden(err) && (err == nil || !strings.Contains(err.Error(), "Forbidden")) {
		t.Fatalf("a permanently forbidden write must fail: %v", err)
	}
}

func secretData(o map[string]any) map[string]any {
	out := map[string]any{}
	d, _ := o["data"].(map[string]any)
	for k, v := range d {
		b, _ := base64.StdEncoding.DecodeString(v.(string))
		out[k] = string(b)
	}
	return out
}

func TestQuotaFigures(t *testing.T) {
	req, lim := quota(20, 40)
	if req != "62Mi" || lim != "124Mi" {
		t.Fatalf("%s %s", req, lim)
	}
	if req, lim = quota(100, 0); req != "182Mi" || lim != "289Mi" {
		t.Fatalf("%s %s", req, lim)
	}
}

func TestInvalidInputsRefused(t *testing.T) {
	in, _, _, spec := fixture(t, nil)
	spec.Namespace = "kube-system"
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("namespace outside mod-<id> accepted")
	}
	in, _, _, spec = fixture(t, nil)
	spec.ID, spec.Namespace = "Bad_ID", "mod-Bad_ID"
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("bad id accepted")
	}
}

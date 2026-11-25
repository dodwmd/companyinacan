package installer

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMatchRuleParser(t *testing.T) {
	const host = "stub.a.example"
	ok := []string{
		"Host(`stub.a.example`)",
		"host(`stub.a.example`) && pathprefix(`/x`)",
		"Host(`stub.a.example`) && PathPrefix(`/a`) && Method(`GET`, `POST`)",
		"PathPrefix(`/a`) && Host(`stub.a.example`) && Headers(`X-A`, `b`)",
	}
	for _, m := range ok {
		if err := checkMatch(m, host); err != nil {
			t.Errorf("%q refused: %v", m, err)
		}
	}
	bad := []string{
		"Host(`stub.a.example`) || PathPrefix(`/`)",
		"PathPrefix(`/`) || Host(`stub.a.example`)",
		"Host(`stub.a.example`) || HeaderRegexp(`Host`, `.*`)",
		"Host(`stub.a.example`) || ClientIP(`0.0.0.0/0`)",
		"Host(`stub.a.example`) | PathPrefix(`/`)",
		"!Host(`other.a.example`)",
		"Host(`stub.a.example`) && !PathPrefix(`/x`)",
		"(Host(`stub.a.example`))",
		"(Host(`stub.a.example`) && PathPrefix(`/`))",
		"hostregexp(`.+`)",
		"HOSTREGEXP(`.+`) && Host(`stub.a.example`)",
		"Host(`stub.a.example`) && HostRegexp(`.+`)",
		"HostSNI(`*`)",
		"Host(`stub.a.example`) && ClientIP(`0.0.0.0/0`)",
		"Host(`stub.a.example`) && Query(`a=b`)",
		"Host(`stub.a.example`) && Host(`stub.a.example`)",
		"Host(`stub.a.example`, `evil.example`)",
		"Host(`evil.example`)",
		"Host(`STUB.a.example`)",
		"PathPrefix(`/`)",
		"Host(`stub.a.example`) &&",
		"Host(`stub.a.example`) && PathPrefix(`/a`) Host(`evil.example`)",
		"Host(`stub.a.example`) && PathPrefix(`/a`",
		"Host(`stub.a.example`)\x00",
		"",
	}
	for _, m := range bad {
		if err := checkMatch(m, host); err == nil {
			t.Errorf("%q accepted", m)
		}
	}
}

func TestIngressRouteEntryPointsAndNames(t *testing.T) {
	route := func(extra, svc string) string {
		return "apiVersion: traefik.io/v1alpha1\nkind: IngressRoute\nmetadata: {name: r}\nspec:\n" + extra +
			"  routes:\n    - match: \"Host(`stub.a.example`)\"\n      kind: Rule\n      services: [{name: " + svc + ", port: 80}]\n"
	}
	if err := enableWith(t, "a.example", route("  entryPoints: [web, websecure]\n", "stub")); err != nil {
		t.Fatalf("%v", err)
	}
	if err := enableWith(t, "a.example", route("  entryPoints: [traefik]\n", "stub")); err == nil {
		t.Error("entryPoint traefik (the dashboard) accepted")
	}
	if err := enableWith(t, "a.example", route("", "api@internal")); err == nil {
		t.Error("service name with @ accepted")
	}
}

func TestRenderCPUBoundNestedRange(t *testing.T) {
	big := make([]any, 200000)
	for i := range big {
		big[i] = i
	}
	body := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata:\n  x: \"{{ range .Values.l }}{{ range $.Values.l }}{{ end }}{{ end }}\"\n"
	dir := writeChart(t, map[string]string{"templates/a.yaml": body})
	done := make(chan error, 1)
	go func() {
		_, err := RenderChart(dir, map[string]any{"l": big}, ReleaseInfo{Name: "x", Namespace: "n"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "iterations") {
			t.Fatalf("%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("nested range over 200k elements did not stop")
	}
	// a range hidden in a define reached through include is counted too
	body2 := "{{- define \"r\" -}}{{ range .Values.l }}{{ range $.Values.l }}{{ end }}{{ end }}{{- end -}}apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata: {x: \"{{ include \"r\" . }}\"}\n"
	dir2 := writeChart(t, map[string]string{"templates/a.yaml": body2})
	if _, err := RenderChart(dir2, map[string]any{"l": big}, ReleaseInfo{Name: "x", Namespace: "n"}); err == nil {
		t.Fatal("range inside include accepted")
	}
}

func TestEnableRenderHonoursContext(t *testing.T) {
	big := make([]any, 200000)
	for i := range big {
		big[i] = i
	}
	in, _, _, spec := fixture(t, map[string]string{
		"stub/chart/templates/loop.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: loop}\ndata:\n  x: \"{{ range .Values.settings.l }}{{ range $.Values.settings.l }}{{ end }}{{ end }}\"\n",
	})
	spec.Settings = map[string]any{"l": big}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := in.Enable(ctx, spec); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("Enable did not return promptly")
	}
}

func TestPodTemplateAllowlists(t *testing.T) {
	dep := func(podSpec string) string {
		return "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: w}\nspec:\n  selector: {matchLabels: {a: b}}\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n" + podSpec
	}
	bad := map[string]string{
		"hostPath":           dep("      volumes: [{name: v, hostPath: {path: /}}]\n      containers: [{name: c, image: x}]\n"),
		"nfs":                dep("      volumes: [{name: v, nfs: {server: s, path: /}}]\n      containers: [{name: c, image: x}]\n"),
		"downwardAPI":        dep("      volumes: [{name: v, downwardAPI: {}}]\n      containers: [{name: c, image: x}]\n"),
		"secret release":     dep("      volumes: [{name: v, secret: {secretName: can-release}}]\n      containers: [{name: c, image: x}]\n"),
		"foreign secret":     dep("      volumes: [{name: v, secret: {secretName: someone-elses}}]\n      containers: [{name: c, image: x}]\n"),
		"foreign pvc":        dep("      volumes: [{name: v, persistentVolumeClaim: {claimName: other}}]\n      containers: [{name: c, image: x}]\n"),
		"foreign configmap":  dep("      volumes: [{name: v, configMap: {name: kube-root-ca.crt}}]\n      containers: [{name: c, image: x}]\n"),
		"two sources":        dep("      volumes: [{name: v, emptyDir: {}, hostPath: {path: /}}]\n      containers: [{name: c, image: x}]\n"),
		"projected other":    dep("      volumes: [{name: v, projected: {sources: [{serviceAccountToken: {audience: api, path: t}}]}}]\n      containers: [{name: c, image: x}]\n"),
		"projected secret":   dep("      volumes: [{name: v, projected: {sources: [{secret: {name: x}}]}}]\n      containers: [{name: c, image: x}]\n"),
		"hostPort":           dep("      containers: [{name: c, image: x, ports: [{containerPort: 80, hostPort: 80}]}]\n"),
		"ephemeral":          dep("      ephemeralContainers: [{name: e, image: x}]\n      containers: [{name: c, image: x}]\n"),
		"shareProcess":       dep("      shareProcessNamespace: true\n      containers: [{name: c, image: x}]\n"),
		"shareProcess str":   dep("      shareProcessNamespace: \"true\"\n      containers: [{name: c, image: x}]\n"),
		"hostNetwork str":    dep("      hostNetwork: \"True\"\n      containers: [{name: c, image: x}]\n"),
		"privileged str":     dep("      containers: [{name: c, image: x, securityContext: {privileged: \"true\"}}]\n"),
		"serviceAccount":     dep("      serviceAccount: default\n      containers: [{name: c, image: x}]\n"),
		"env foreign secret": dep("      containers: [{name: c, image: x, env: [{name: A, valueFrom: {secretKeyRef: {name: can-release, key: objects}}}]}]\n"),
		"envFrom foreign":    dep("      containers: [{name: c, image: x, envFrom: [{secretRef: {name: other}}]}]\n"),
	}
	for name, body := range bad {
		if err := enableWith(t, "a.example", body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := dep("      volumes: [{name: tmp, emptyDir: {}}, {name: can-token, projected: {sources: [{serviceAccountToken: {audience: can, path: token}}]}}]\n      containers: [{name: c, image: x, volumeMounts: [{name: can-token, mountPath: /var/run/secrets/can}]}]\n")
	if err := enableWith(t, "a.example", ok); err != nil {
		t.Fatalf("a chart that declares its own `can` token volume (the starter charts do) must work: %v", err)
	}
}

func TestChartCanTokenVolumeIsNotDuplicated(t *testing.T) {
	in, api, _, spec := fixture(t, map[string]string{"stub/chart/templates/deploy2.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: two}\nspec:\n  selector: {matchLabels: {a: b}}\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n      volumes: [{name: can-token, projected: {sources: [{serviceAccountToken: {audience: can, path: token}}]}}]\n      containers: [{name: c, image: x, volumeMounts: [{name: can-token, mountPath: /var/run/secrets/can}]}]\n"})
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	d := get(t, api, "Deployment", "mod-stub", "two")
	ps := dig(d, "spec", "template", "spec").(map[string]any)
	n := 0
	for _, v := range ps["volumes"].([]any) {
		if v.(map[string]any)["name"] == "can-token" {
			n++
		}
	}
	m := 0
	for _, x := range ps["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any) {
		if x.(map[string]any)["name"] == "can-token" {
			m++
		}
	}
	if n != 1 || m != 1 {
		t.Fatalf("duplicate can-token volume (%d) or mount (%d) would be rejected by the API server", n, m)
	}
}

func TestPVCAndMetadataRules(t *testing.T) {
	pvc := func(spec string) string {
		return "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: d2}\nspec:\n  accessModes: [ReadWriteOnce]\n  resources: {requests: {storage: 1Gi}}\n" + spec
	}
	bad := map[string]string{
		"volumeName":       pvc("  volumeName: pv-of-another-tenant\n"),
		"selector":         pvc("  selector: {matchLabels: {a: b}}\n"),
		"dataSource":       pvc("  dataSource: {kind: PersistentVolumeClaim, name: x}\n"),
		"dataSourceRef":    pvc("  dataSourceRef: {kind: PersistentVolumeClaim, name: x}\n"),
		"storageClass":     pvc("  storageClassName: premium\n"),
		"wrong apiVersion": "apiVersion: apps/v1beta1\nkind: Deployment\nmetadata: {name: w}\nspec: {}\n",
		"ownerRef":         "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, ownerReferences: [{apiVersion: v1, kind: Node, name: n, uid: u}]}\n",
		"finalizer":        "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, finalizers: [x/y]}\n",
		"statefulset vct":  "apiVersion: apps/v1\nkind: StatefulSet\nmetadata: {name: s}\nspec:\n  selector: {matchLabels: {a: b}}\n  volumeClaimTemplates: [{metadata: {name: d}, spec: {volumeName: pv-x}}]\n  template: {metadata: {labels: {a: b}}, spec: {containers: [{name: c, image: x}]}}\n",
	}
	for name, body := range bad {
		if err := enableWith(t, "a.example", body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := enableWith(t, "a.example", pvc("  storageClassName: local-path\n")); err != nil {
		t.Fatalf("local-path claim refused: %v", err)
	}
}

func TestReleaseRecordIsBoundToTheModuleSecret(t *testing.T) {
	in, _, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	spec.HMACSecret = "a-different-secret"
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "HMAC") {
		t.Fatalf("a record written under another secret was replayed: %v", err)
	}
}

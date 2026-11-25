package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeChart(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRenderChartSubset(t *testing.T) {
	dir := writeChart(t, map[string]string{
		"values.yaml":            "image: busybox\nreplicas: 2\nlabels:\n  tier: web\n",
		"templates/_helpers.tpl": `{{- define "x.name" -}}{{ .Release.Name }}-app{{- end -}}`,
		"templates/deploy.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "x.name" . }}
  labels:
    {{- toYaml .Values.labels | nindent 4 }}
spec:
  replicas: {{ .Values.replicas }}
  template:
    spec:
      containers:
        - name: app
          image: {{ .Values.image | quote }}
          args: {{ toJson .Values.args }}
{{- if .Values.extra }}
          env: []
{{- end }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Values.missing | default "svc" }}
`,
		"templates/NOTES.txt": "not yaml {{",
	})
	objs, err := RenderChart(dir, map[string]any{"args": []any{"a", "b"}, "replicas": 3}, ReleaseInfo{Name: "stub", Namespace: "mod-stub"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("%d objects: %v", len(objs), objs)
	}
	d := objs[0]
	md := d["metadata"].(map[string]any)
	if md["name"] != "stub-app" || md["labels"].(map[string]any)["tier"] != "web" {
		t.Fatalf("%v", md)
	}
	spec := d["spec"].(map[string]any)
	if spec["replicas"] != 3 { // caller values override the chart defaults
		t.Fatalf("replicas %v", spec["replicas"])
	}
	c := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["image"] != "busybox" || len(c["args"].([]any)) != 2 {
		t.Fatalf("%v", c)
	}
	if _, ok := c["env"]; ok {
		t.Fatal("falsy if branch rendered")
	}
	if objs[1]["metadata"].(map[string]any)["name"] != "svc" {
		t.Fatalf("%v", objs[1])
	}
}

func TestRenderChartErrors(t *testing.T) {
	cases := map[string]string{
		"required":  `{{ required "image is required" .Values.nothing }}`,
		"fail":      `{{ fail "boom" }}`,
		"badyaml":   "kind: [",
		"nokind":    "metadata:\n  name: x\n",
		"undefined": `{{ nosuchfunc 1 }}`,
	}
	for name, body := range cases {
		dir := writeChart(t, map[string]string{"templates/a.yaml": body})
		if _, err := RenderChart(dir, nil, ReleaseInfo{Name: "x", Namespace: "n"}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRenderChartNoTemplateDir(t *testing.T) {
	if _, err := RenderChart(t.TempDir(), nil, ReleaseInfo{Name: "x", Namespace: "n"}); err == nil || !strings.Contains(err.Error(), "templates") {
		t.Fatalf("%v", err)
	}
}

func TestRenderChartRefusesPathEscape(t *testing.T) {
	dir := writeChart(t, map[string]string{"templates/a.yaml": "kind: ConfigMap\napiVersion: v1\nmetadata: {name: a}\n"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.yaml"), []byte("kind: Secret\napiVersion: v1\nmetadata: {name: leak}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "x.yaml"), filepath.Join(dir, "templates", "link.yaml")); err != nil {
		t.Skip("symlinks unavailable")
	}
	objs, err := RenderChart(dir, nil, ReleaseInfo{Name: "x", Namespace: "n"})
	if err != nil {
		return // refusing is fine
	}
	if len(objs) != 1 {
		t.Fatalf("symlinked template outside the chart was rendered: %v", objs)
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

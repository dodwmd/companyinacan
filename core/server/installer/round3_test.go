package installer

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// exponentialChain: 45 defines, each calling the next twice, an empty leaf. No range, no include.
func exponentialChain() string {
	var b strings.Builder
	for i := 0; i < 45; i++ {
		fmt.Fprintf(&b, "{{- define \"d%d\" -}}{{ template \"d%d\" . }}{{ template \"d%d\" . }}{{- end -}}\n", i, i+1, i+1)
	}
	b.WriteString("{{- define \"d45\" -}}{{- end -}}\n")
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata: {x: \"{{ template \"d0\" . }}\"}\n")
	return b.String()
}

func goroutinesSettle(t *testing.T, base int) int {
	t.Helper()
	n := 0
	for i := 0; i < 40; i++ {
		time.Sleep(50 * time.Millisecond)
		if n = runtime.NumGoroutine(); n <= base+1 {
			return n
		}
	}
	return n
}

func TestExponentialTemplateFanOutStopsFast(t *testing.T) {
	base := runtime.NumGoroutine()
	dir := writeChart(t, map[string]string{"templates/a.yaml": exponentialChain()})
	start := time.Now()
	_, err := RenderChart(dir, nil, ReleaseInfo{Name: "x", Namespace: "n"})
	if err == nil || !strings.Contains(err.Error(), "iterations") {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > raceBudget(2*time.Second) {
		t.Fatalf("took %s", d)
	}
	if n := goroutinesSettle(t, base); n > base+1 {
		t.Fatalf("goroutines %d, baseline %d", n, base)
	}
}

func TestRepeatedReconcilesDoNotStackRenders(t *testing.T) {
	base := runtime.NumGoroutine()
	in, _, _, spec := fixture(t, map[string]string{"stub/chart/templates/bomb.yaml": exponentialChain()})
	// Behaviour, not wall clock: every reconcile must fail (the renderer's own limits end each
	// one) and the checks below show nothing is left running. A time budget here only measured
	// how loaded the machine was.
	for i := 0; i < 6; i++ {
		if err := in.Enable(context.Background(), spec); err == nil {
			t.Fatal("hostile chart installed")
		}
	}
	if n := goroutinesSettle(t, base); n > base+1 {
		t.Fatalf("renders stacked up: goroutines %d, baseline %d", n, base)
	}
	in.mu.Lock()
	left := len(in.rendering)
	in.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d renders still marked in flight", left)
	}
}

func TestOnlyOneRenderPerModuleAtATime(t *testing.T) {
	in, _, _, spec := fixture(t, nil)
	in.mu.Lock()
	in.rendering[spec.ID] = true
	in.mu.Unlock()
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("%v", err)
	}
}

func TestSecretNamesCannotComeFromChartValues(t *testing.T) {
	// The chart points a Secret reference at an arbitrary name through a value.
	in, _, _, spec := fixture(t, map[string]string{
		"stub/chart/templates/peek.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: peek}\nspec:\n  selector: {matchLabels: {a: b}}\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n      containers: [{name: c, image: x, env: [{name: A, valueFrom: {secretKeyRef: {name: \"{{ .Values.secrets.other }}\", key: k}}}]}]\n",
		"stub/chart/values.yaml":         "image: busybox:1.37.0\nreplicas: 1\nsecrets: {other: some-tenant-secret}\n",
	})
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("a Secret named by a chart value was accepted")
	}
}

func TestCanTokenVolumeNameIsReserved(t *testing.T) {
	body := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: w}\nspec:\n  selector: {matchLabels: {a: b}}\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n      volumes: [{name: can-token, emptyDir: {}}]\n      containers: [{name: c, image: x}]\n"
	if err := enableWith(t, "a.example", body); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("%v", err)
	}
}

func TestIngressRouteTLSIsLimited(t *testing.T) {
	route := func(tls string) string {
		return "apiVersion: traefik.io/v1alpha1\nkind: IngressRoute\nmetadata: {name: r}\nspec:\n  routes:\n    - match: \"Host(`stub.a.example`)\"\n      kind: Rule\n      services: [{name: stub, port: 80}]\n" + tls
	}
	bad := []string{
		"  tls: {domains: [{main: other.example}], certResolver: letsencrypt}\n",
		"  tls: {secretName: someones-cert}\n",
		"  tls: {certResolver: other}\n",
		"  tls: {options: {name: x}}\n",
	}
	for _, tls := range bad {
		if err := enableWith(t, "a.example", route(tls)); err == nil {
			t.Errorf("accepted %q", tls)
		}
	}
	if err := enableWith(t, "a.example", route("  tls: {certResolver: letsencrypt}\n")); err != nil {
		t.Fatal(err)
	}
}

// raceBudget widens a wall-clock bound when the race detector slows everything down several times.
func raceBudget(d time.Duration) time.Duration {
	if raceEnabled {
		return d * 5
	}
	return d
}

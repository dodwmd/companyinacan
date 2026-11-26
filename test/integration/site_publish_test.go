//go:build integration

package integration

// The website editor's admission policies (specs/064-site-editor, deploy/base/admission-policy.yaml):
// what can-core needs to publish is allowed, everything else its RoleBinding in can-system would let
// it do to the site is refused. Every request is a server-side dry run: admission runs, nothing is
// stored. Needs only `make dev`.

import (
	"strings"
	"testing"
	"time"
)

const siteFixture = `apiVersion: apps/v1
kind: Deployment
metadata: {name: site, namespace: can-system}
spec:
  replicas: 0
  selector: {matchLabels: {app.kubernetes.io/name: site}}
  template:
    metadata:
      labels: {app.kubernetes.io/name: site}
      annotations: {can.dev/site-sha256: "none"}
    spec:
      containers:
        - {name: site, image: "registry.invalid/site:1"}
`

const siteContentFixture = `apiVersion: v1
kind: ConfigMap
metadata: {name: site-content, namespace: can-system}
binaryData: {site.tar.gz: YQ==}
`

func TestAdmissionPolicyBoundsSitePublishing(t *testing.T) {
	waitForPolicy(t)
	mustKubectl(t, "get", "validatingadmissionpolicy", "can-core-site", "can-core-site-content")
	for kind, yaml := range map[string]string{"deployment": siteFixture, "configmap": siteContentFixture} {
		name := map[string]string{"deployment": "site", "configmap": "site-content"}[kind]
		if _, err := kubectl("-n", "can-system", "get", kind, name); err == nil {
			continue // a real one is there; dry runs do not touch it
		}
		if out, err := kubectlStdin(yaml, "create", "-f", "-"); err != nil {
			t.Fatalf("fixture: %s", out)
		}
		t.Cleanup(func() { _, _ = kubectl("-n", "can-system", "delete", kind, name, "--ignore-not-found") })
	}
	patch := func(kind, name, body string) (string, error) {
		return asCanCore("-n", "can-system", "patch", kind, name, "--type=merge", "-p", body, "--dry-run=server")
	}
	const roll = `{"spec":{"template":{"metadata":{"annotations":{"can.dev/site-sha256":"abc"}}}}}`
	eventually(t, 60*time.Second, "the site policy to be enforced", func() (bool, string) {
		out, err := patch("deployment", "site", `{"spec":{"replicas":3}}`)
		return err != nil && strings.Contains(out, "may not change what the site runs"), out
	})
	if out, err := patch("deployment", "site", roll); err != nil {
		t.Errorf("can-core cannot roll the site: %s", out)
	}
	if out, err := patch("configmap", "site-content", `{"binaryData":{"site.tar.gz":"Yg=="},"metadata":{"annotations":{"can.dev/site-sha256":"abc"}}}`); err != nil {
		t.Errorf("can-core cannot write the site content: %s", out)
	}
	for _, c := range []struct{ kind, name, body, want string }{
		{"deployment", "site", `{"spec":{"template":{"spec":{"containers":[{"name":"site","image":"evil.invalid/x"}]}}}}`, "may not change what the site runs"},
		{"deployment", "site", `{"spec":{"template":{"spec":{"serviceAccountName":"can-core"}}}}`, "may not change what the site runs"},
		{"deployment", "site", `{"spec":{"template":{"metadata":{"labels":{"app.kubernetes.io/name":"site","app":"can-core"}}}}}`, "may not change what the site runs"},
		{"deployment", "site", `{"spec":{"template":{"metadata":{"annotations":{"other":"x"}}}}}`, "may only change the can.dev/site-sha256"},
		{"configmap", "site-content", `{"data":{"extra":"x"}}`, "holds the site archive and nothing else"},
		{"configmap", "site-content", `{"binaryData":{"other":"YQ=="}}`, "holds the site archive and nothing else"},
	} {
		if out, err := patch(c.kind, c.name, c.body); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s %s %s: err=%v %s", c.kind, c.name, c.body, err, out)
		}
	}
	for _, kind := range []string{"deployment site", "configmap site-content"} {
		k, name, _ := strings.Cut(kind, " ")
		if out, err := asCanCore("-n", "can-system", "delete", k, name, "--dry-run=server"); err == nil || !strings.Contains(out, "can-core may only update") {
			t.Errorf("delete %s: err=%v %s", kind, err, out)
		}
	}
	if out, err := asCanCoreStdin(strings.Replace(siteFixture, "name: site, namespace", "name: other, namespace", 1), "create", "-f", "-", "--dry-run=server"); err == nil || !strings.Contains(out, "may only update Deployment site") {
		t.Errorf("create another workload: err=%v %s", err, out)
	}
	// Administrators (and `can up`) are not affected.
	mustKubectl(t, "-n", "can-system", "patch", "deployment", "site", "--type=merge", "-p", `{"spec":{"revisionHistoryLimit":10}}`, "--dry-run=server")
}

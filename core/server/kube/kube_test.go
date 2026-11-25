package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type rec struct {
	method, path, query, ctype, auth string
	body                             string
}

func server(t *testing.T, status int, resp string, got *rec) *Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(b)}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(ts.Close)
	return &Client{Server: ts.URL, HTTP: ts.Client(), Token: func() (string, error) { return "tok", nil }}
}

func TestApplyIsServerSideApplyWithForce(t *testing.T) {
	var got rec
	c := server(t, 200, `{}`, &got)
	obj := Object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "x", "namespace": "mod-a"}}
	if err := c.Apply(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	if got.method != "PATCH" || got.path != "/apis/apps/v1/namespaces/mod-a/deployments/x" {
		t.Fatalf("%+v", got)
	}
	if got.ctype != "application/apply-patch+yaml" || !strings.Contains(got.query, "fieldManager=can-core") || !strings.Contains(got.query, "force=true") {
		t.Fatalf("%+v", got)
	}
	if got.auth != "Bearer tok" {
		t.Fatal(got.auth)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got.body), &back); err != nil || back["kind"] != "Deployment" {
		t.Fatalf("body %q", got.body)
	}
}

func TestClusterScopedAndCoreGroupPaths(t *testing.T) {
	var got rec
	c := server(t, 201, `{}`, &got)
	if _, err := c.Create(context.Background(), Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "mod-a"}}); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/api/v1/namespaces" {
		t.Fatalf("%+v", got)
	}
	if _, err := c.Create(context.Background(), Object{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": map[string]any{"name": "b", "namespace": "mod-a"}}); err != nil {
		t.Fatal(err)
	}
	if got.path != "/apis/rbac.authorization.k8s.io/v1/namespaces/mod-a/rolebindings" {
		t.Fatal(got.path)
	}
}

func TestCreateConflictIsNotAnError(t *testing.T) {
	var got rec
	c := server(t, 409, `{"kind":"Status","reason":"AlreadyExists","code":409,"message":"exists"}`, &got)
	created, err := c.Create(context.Background(), Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "mod-a"}})
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
}

func TestStatusErrors(t *testing.T) {
	var got rec
	c := server(t, 403, `{"kind":"Status","reason":"Forbidden","code":403,"message":"nope"}`, &got)
	_, err := c.Get(context.Background(), "ConfigMap", "ns", "x")
	if !IsForbidden(err) || IsNotFound(err) || !strings.Contains(err.Error(), "nope") {
		t.Fatal(err)
	}
	c = server(t, 404, `{"kind":"Status","code":404,"message":"gone"}`, &got)
	if _, err := c.Get(context.Background(), "ConfigMap", "ns", "x"); !IsNotFound(err) {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), "ConfigMap", "ns", "x"); err != nil {
		t.Fatalf("delete of a missing object must succeed: %v", err)
	}
}

func TestListUsesLabelSelector(t *testing.T) {
	var got rec
	c := server(t, 200, `{"items":[{"kind":"Deployment","metadata":{"name":"a"}}]}`, &got)
	items, err := c.List(context.Background(), "Deployment", "mod-a", "can.dev/module=a")
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	if got.path != "/apis/apps/v1/namespaces/mod-a/deployments" || !strings.Contains(got.query, "labelSelector=can.dev%2Fmodule%3Da") {
		t.Fatalf("%+v", got)
	}
}

func TestMergePatch(t *testing.T) {
	var got rec
	c := server(t, 200, `{}`, &got)
	if err := c.MergePatch(context.Background(), "Deployment", "mod-a", "x", Object{"spec": map[string]any{"replicas": 0}}); err != nil {
		t.Fatal(err)
	}
	if got.method != "PATCH" || got.ctype != "application/merge-patch+json" || got.body != `{"spec":{"replicas":0}}` {
		t.Fatalf("%+v", got)
	}
}

func TestUnknownKindRefused(t *testing.T) {
	var got rec
	c := server(t, 200, `{}`, &got)
	if err := c.Apply(context.Background(), Object{"apiVersion": "v1", "kind": "ClusterRoleBinding", "metadata": map[string]any{"name": "x"}}); err == nil {
		t.Fatal("unlisted kind must be refused client-side")
	}
}

func TestFakeBehavesLikeTheClient(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	o := Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c", "namespace": "n", "labels": map[string]any{"a": "b"}}}
	if err := f.Apply(ctx, o); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Get(ctx, "ConfigMap", "n", "c"); err != nil || got == nil {
		t.Fatal(err)
	}
	if l, _ := f.List(ctx, "ConfigMap", "n", "a=b"); len(l) != 1 {
		t.Fatal(l)
	}
	if l, _ := f.List(ctx, "ConfigMap", "n", "a=zzz"); len(l) != 0 {
		t.Fatal(l)
	}
	if err := f.Delete(ctx, "ConfigMap", "n", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(ctx, "ConfigMap", "n", "c"); !IsNotFound(err) {
		t.Fatal(err)
	}
}

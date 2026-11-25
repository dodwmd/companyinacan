// Package kube is a deliberately small Kubernetes API client for can-core: server-side apply,
// create, get, list, delete and merge-patch for a fixed table of kinds, over net/http.
//
// Why not client-go and the Helm SDK: measured in specs/005-can-core-mvp/research.md R16, importing
// the Helm action package adds about 15 MB of binary and 19 MB of resident memory at start-up
// (scheme registration), against a declared can-core budget of 48 MB. This client costs a few
// hundred lines and no new dependency.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// FieldManager is the server-side-apply field manager name.
const FieldManager = "can-core"

// Object is an unstructured Kubernetes object.
type Object = map[string]any

type kindInfo struct {
	apiVersion string // as in the manifest
	prefix     string // /api/v1 or /apis/<group>/<version>
	plural     string
	namespaced bool
}

var kinds = map[string]kindInfo{
	"Namespace":             {"v1", "/api/v1", "namespaces", false},
	"ServiceAccount":        {"v1", "/api/v1", "serviceaccounts", true},
	"Service":               {"v1", "/api/v1", "services", true},
	"ConfigMap":             {"v1", "/api/v1", "configmaps", true},
	"Secret":                {"v1", "/api/v1", "secrets", true},
	"PersistentVolumeClaim": {"v1", "/api/v1", "persistentvolumeclaims", true},
	"ResourceQuota":         {"v1", "/api/v1", "resourcequotas", true},
	"LimitRange":            {"v1", "/api/v1", "limitranges", true},
	"Pod":                   {"v1", "/api/v1", "pods", true},
	"Deployment":            {"apps/v1", "/apis/apps/v1", "deployments", true},
	"StatefulSet":           {"apps/v1", "/apis/apps/v1", "statefulsets", true},
	"Job":                   {"batch/v1", "/apis/batch/v1", "jobs", true},
	"CronJob":               {"batch/v1", "/apis/batch/v1", "cronjobs", true},
	"NetworkPolicy":         {"networking.k8s.io/v1", "/apis/networking.k8s.io/v1", "networkpolicies", true},
	"Ingress":               {"networking.k8s.io/v1", "/apis/networking.k8s.io/v1", "ingresses", true},
	"RoleBinding":           {"rbac.authorization.k8s.io/v1", "/apis/rbac.authorization.k8s.io/v1", "rolebindings", true},
	"IngressRoute":          {"traefik.io/v1alpha1", "/apis/traefik.io/v1alpha1", "ingressroutes", true},
	"Middleware":            {"traefik.io/v1alpha1", "/apis/traefik.io/v1alpha1", "middlewares", true},
	// Read-only use: can-core verifies at start that its admission policies exist.
	"ValidatingAdmissionPolicy":        {"admissionregistration.k8s.io/v1", "/apis/admissionregistration.k8s.io/v1", "validatingadmissionpolicies", false},
	"ValidatingAdmissionPolicyBinding": {"admissionregistration.k8s.io/v1", "/apis/admissionregistration.k8s.io/v1", "validatingadmissionpolicybindings", false},
}

// Known reports whether the client can handle kind.
func Known(kind string) bool { _, ok := kinds[kind]; return ok }

// API is what the installer needs from a cluster. Client talks to a real API server, Fake is for tests.
type API interface {
	// Apply is a forced server-side apply of one object.
	Apply(ctx context.Context, obj Object) error
	// Create creates obj; created is false when it already exists (not an error).
	Create(ctx context.Context, obj Object) (created bool, err error)
	Get(ctx context.Context, kind, namespace, name string) (Object, error)
	List(ctx context.Context, kind, namespace, labelSelector string) ([]Object, error)
	// Delete removes an object; a missing object is not an error.
	Delete(ctx context.Context, kind, namespace, name string) error
	MergePatch(ctx context.Context, kind, namespace, name string, patch Object) error
}

// StatusError is a non-2xx API response.
type StatusError struct {
	Code    int
	Reason  string
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kubernetes API %d %s: %s", e.Code, e.Reason, e.Message)
}

func statusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return statusCode(err) == http.StatusNotFound }

// IsForbidden reports a 403 (RBAC or an admission policy).
func IsForbidden(err error) bool { return statusCode(err) == http.StatusForbidden }

// IsConflict reports a 409.
func IsConflict(err error) bool { return statusCode(err) == http.StatusConflict }

// Client implements API over net/http.
type Client struct {
	Server string       // https://kubernetes.default.svc
	HTTP   *http.Client // trusts the cluster CA
	// Token returns the bearer token (re-read per request: projected tokens rotate).
	Token func() (string, error)
}

// NewInCluster builds a Client from the pod's ServiceAccount files (token re-read per request, CA pinned).
func NewInCluster(server, tokenFile, caFile string) (*Client, error) {
	ca, err := os.ReadFile(caFile) //nolint:gosec // fixed in-cluster path from configuration
	if err != nil {
		return nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("cluster CA file holds no certificate")
	}
	return &Client{
		Server: server,
		HTTP:   &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
		Token: func() (string, error) {
			b, err := os.ReadFile(tokenFile) //nolint:gosec // fixed in-cluster path from configuration
			return strings.TrimSpace(string(b)), err
		},
	}, nil
}

func meta(obj Object) (name, ns string) {
	m, _ := obj["metadata"].(map[string]any)
	name, _ = m["name"].(string)
	ns, _ = m["namespace"].(string)
	return
}

func info(kind string) (kindInfo, error) {
	k, ok := kinds[kind]
	if !ok {
		return kindInfo{}, fmt.Errorf("kube: kind %q is not supported", kind)
	}
	return k, nil
}

func (k kindInfo) path(ns, name string) (string, error) {
	p := k.prefix
	if k.namespaced {
		if ns == "" {
			return "", errors.New("kube: namespace required")
		}
		p += "/namespaces/" + url.PathEscape(ns)
	}
	p += "/" + k.plural
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p, nil
}

func (c *Client) do(ctx context.Context, method, path, query, ctype string, body []byte) ([]byte, error) {
	u := strings.TrimRight(c.Server, "/") + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	tok, err := c.Token()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	cl := c.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		se := &StatusError{Code: resp.StatusCode, Reason: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(raw))}
		var st struct{ Reason, Message string }
		if json.Unmarshal(raw, &st) == nil && st.Message != "" {
			se.Message = st.Message
			if st.Reason != "" {
				se.Reason = st.Reason
			}
		}
		return nil, se
	}
	return raw, nil
}

// Apply implements API.
func (c *Client) Apply(ctx context.Context, obj Object) error {
	kind, _ := obj["kind"].(string)
	k, err := info(kind)
	if err != nil {
		return err
	}
	name, ns := meta(obj)
	if name == "" {
		return errors.New("kube: object has no name")
	}
	p, err := k.path(ns, name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPatch, p, "fieldManager="+FieldManager+"&force=true", "application/apply-patch+yaml", b)
	return err
}

// Create implements API.
func (c *Client) Create(ctx context.Context, obj Object) (bool, error) {
	kind, _ := obj["kind"].(string)
	k, err := info(kind)
	if err != nil {
		return false, err
	}
	_, ns := meta(obj)
	p, err := k.path(ns, "")
	if err != nil {
		return false, err
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return false, err
	}
	_, err = c.do(ctx, http.MethodPost, p, "fieldManager="+FieldManager, "application/json", b)
	if IsConflict(err) {
		return false, nil
	}
	return err == nil, err
}

// Get implements API.
func (c *Client) Get(ctx context.Context, kind, namespace, name string) (Object, error) {
	k, err := info(kind)
	if err != nil {
		return nil, err
	}
	p, err := k.path(namespace, name)
	if err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, http.MethodGet, p, "", "", nil)
	if err != nil {
		return nil, err
	}
	var o Object
	return o, json.Unmarshal(raw, &o)
}

// List implements API.
func (c *Client) List(ctx context.Context, kind, namespace, labelSelector string) ([]Object, error) {
	k, err := info(kind)
	if err != nil {
		return nil, err
	}
	p, err := k.path(namespace, "")
	if err != nil {
		return nil, err
	}
	q := ""
	if labelSelector != "" {
		q = "labelSelector=" + url.QueryEscape(labelSelector)
	}
	raw, err := c.do(ctx, http.MethodGet, p, q, "", nil)
	if err != nil {
		return nil, err
	}
	var l struct {
		Items []Object `json:"items"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	for _, it := range l.Items { // list items omit kind
		if it["kind"] == nil {
			it["kind"] = kind
		}
	}
	return l.Items, nil
}

// Delete implements API.
func (c *Client) Delete(ctx context.Context, kind, namespace, name string) error {
	k, err := info(kind)
	if err != nil {
		return err
	}
	p, err := k.path(namespace, name)
	if err != nil {
		return err
	}
	// Background propagation: dependents (a Job's pods) go with the object instead of lingering.
	_, err = c.do(ctx, http.MethodDelete, p, "propagationPolicy=Background", "", nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// MergePatch implements API.
func (c *Client) MergePatch(ctx context.Context, kind, namespace, name string, patch Object) error {
	k, err := info(kind)
	if err != nil {
		return err
	}
	p, err := k.path(namespace, name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPatch, p, "", "application/merge-patch+json", b)
	return err
}

// ---- Fake ------------------------------------------------------------------------------------

// Fake is an in-memory API for tests. Workloads become ready on apply unless listed in NotReady.
type Fake struct {
	mu       sync.Mutex
	Objects  map[string]Object // kind/namespace/name
	Log      []string          // "apply Kind ns/name", "delete ...", "patch ..."
	NotReady map[string]bool   // workload names that never become ready
	// FailApply makes Apply fail for "Kind/name".
	FailApply map[string]error
	// ForbiddenCreates fails this many Create/Apply calls with a 403 first (RBAC propagation).
	ForbiddenFirst int
}

// NewFake returns an empty fake.
func NewFake() *Fake {
	return &Fake{Objects: map[string]Object{}, NotReady: map[string]bool{}, FailApply: map[string]error{}}
}

func key(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

func clone(o Object) Object {
	b, _ := json.Marshal(o)
	var out Object
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *Fake) forbid() error {
	if f.ForbiddenFirst > 0 {
		f.ForbiddenFirst--
		return &StatusError{Code: 403, Reason: "Forbidden", Message: "not yet"}
	}
	return nil
}

// Apply implements API.
func (f *Fake) Apply(_ context.Context, obj Object) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind, _ := obj["kind"].(string)
	if !Known(kind) {
		return fmt.Errorf("kube: kind %q is not supported", kind)
	}
	name, ns := meta(obj)
	if kind == "NetworkPolicy" { // the first namespaced write after a RoleBinding is created
		if err := f.forbid(); err != nil {
			return err
		}
	}
	if err := f.FailApply[kind+"/"+name]; err != nil {
		return err
	}
	o := foldStringData(clone(obj))
	if kind == "Deployment" || kind == "StatefulSet" {
		spec, _ := o["spec"].(map[string]any)
		rep := 1.0
		if v, ok := spec["replicas"].(float64); ok {
			rep = v
		}
		ready := rep
		if f.NotReady[name] {
			ready = 0
		}
		o["status"] = map[string]any{"observedGeneration": 1.0, "replicas": rep, "readyReplicas": ready, "updatedReplicas": ready}
		o["metadata"].(map[string]any)["generation"] = 1.0
	}
	f.Objects[key(kind, ns, name)] = o
	f.Log = append(f.Log, "apply "+kind+" "+ns+"/"+name)
	return nil
}

// foldStringData does what the API server does with a Secret's stringData: it becomes data.
func foldStringData(o Object) Object {
	if sd, ok := o["stringData"].(map[string]any); ok {
		data, _ := o["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
		}
		for k, v := range sd {
			data[k] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(v)))
		}
		o["data"] = data
		delete(o, "stringData")
	}
	return o
}

// Create implements API.
func (f *Fake) Create(_ context.Context, obj Object) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind, _ := obj["kind"].(string)
	name, ns := meta(obj)
	k := key(kind, ns, name)
	if _, ok := f.Objects[k]; ok {
		return false, nil
	}
	f.Objects[k] = foldStringData(clone(obj))
	f.Log = append(f.Log, "create "+kind+" "+ns+"/"+name)
	return true, nil
}

// Get implements API.
func (f *Fake) Get(_ context.Context, kind, ns, name string) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.Objects[key(kind, ns, name)]
	if !ok {
		return nil, &StatusError{Code: 404, Reason: "NotFound", Message: kind + " " + name}
	}
	return clone(o), nil
}

func matches(o Object, selector string) bool {
	if selector == "" {
		return true
	}
	m, _ := o["metadata"].(map[string]any)
	labels, _ := m["labels"].(map[string]any)
	for _, kv := range strings.Split(selector, ",") {
		k, v, _ := strings.Cut(kv, "=")
		if got, _ := labels[k].(string); got != v {
			return false
		}
	}
	return true
}

// List implements API.
func (f *Fake) List(_ context.Context, kind, ns, selector string) ([]Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Object
	for k, o := range f.Objects {
		if !strings.HasPrefix(k, kind+"/"+ns+"/") || !matches(o, selector) {
			continue
		}
		out = append(out, clone(o))
	}
	return out, nil
}

// Delete implements API.
func (f *Fake) Delete(_ context.Context, kind, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Objects, key(kind, ns, name))
	f.Log = append(f.Log, "delete "+kind+" "+ns+"/"+name)
	return nil
}

func merge(dst, patch map[string]any) {
	for k, v := range patch {
		pm, ok := v.(map[string]any)
		dm, dok := dst[k].(map[string]any)
		if ok && dok {
			merge(dm, pm)
			continue
		}
		dst[k] = v
	}
}

// MergePatch implements API.
func (f *Fake) MergePatch(_ context.Context, kind, ns, name string, patch Object) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.Objects[key(kind, ns, name)]
	if !ok {
		return &StatusError{Code: 404, Reason: "NotFound", Message: name}
	}
	merge(o, clone(patch))
	f.Log = append(f.Log, "patch "+kind+" "+ns+"/"+name)
	return nil
}

// APIVersion returns the canonical apiVersion of a supported kind ("" if unknown).
func APIVersion(kind string) string { return kinds[kind].apiVersion }

package launch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/deploy"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/schema"
	"github.com/dodwmd/companyinacan/core/site"
)

const (
	// SystemNS is the namespace of can-core and the site pod.
	SystemNS = "can-system"
	// FormsNS is the namespace can-core gives the forms module.
	FormsNS = "mod-forms"
	// MaxSiteArchive is the largest site tarball `can up` publishes: a ConfigMap is limited to
	// 1 MiB and binaryData is base64 (+33%), so this leaves room for the object's own overhead.
	MaxSiteArchive = 700 << 10
)

var (
	reHost  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	reEmail = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,200}$`)
)

func marshalDocs(docs []map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	for i, d := range docs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		b, err := yaml.Marshal(d)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
	}
	return buf.Bytes(), nil
}

func parseDocs(b []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if m != nil {
			out = append(out, m)
		}
	}
}

// BaseOptions tunes the platform base manifests.
type BaseOptions struct {
	// ACMEEmail is added to Traefik's Let's Encrypt resolver (optional; certificates are only
	// requested when a route asks for TLS).
	ACMEEmail string
}

// BaseManifests returns deploy/base as one multi-document stream, in the order its
// kustomization.yaml lists the files (namespaces first), so `kubectl apply -f -` works without
// kustomize and without a checkout.
func BaseManifests(o BaseOptions) ([]byte, error) {
	if o.ACMEEmail != "" && !reEmail.MatchString(o.ACMEEmail) {
		return nil, fmt.Errorf("acme email %q is not a plain address", o.ACMEEmail)
	}
	kb, err := deploy.Stack.ReadFile("base/kustomization.yaml")
	if err != nil {
		return nil, err
	}
	var k struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(kb, &k); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, name := range k.Resources {
		b, err := deploy.Stack.ReadFile(path.Join("base", name))
		if err != nil {
			return nil, fmt.Errorf("base/%s listed in kustomization.yaml: %w", name, err)
		}
		if name == "traefik-config.yaml" && o.ACMEEmail != "" {
			if b, err = withACMEEmail(b, o.ACMEEmail); err != nil {
				return nil, err
			}
		}
		buf.WriteString("---\n")
		buf.Write(b)
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}

func withACMEEmail(doc []byte, email string) ([]byte, error) {
	var m map[string]any
	if err := yaml.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	spec, _ := m["spec"].(map[string]any)
	vc, _ := spec["valuesContent"].(string)
	var values map[string]any
	if err := yaml.Unmarshal([]byte(vc), &values); err != nil {
		return nil, fmt.Errorf("traefik values: %w", err)
	}
	acme := dig(values, "certificatesResolvers", "letsencrypt", "acme")
	acme["email"] = email
	nv, err := yaml.Marshal(values)
	if err != nil {
		return nil, err
	}
	spec["valuesContent"] = string(nv)
	return yaml.Marshal(m)
}

// dig returns the nested mapping at keys, creating it as needed.
func dig(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	return m
}

func validImageRef(ref string) bool {
	return reDigestRef.MatchString(ref) || reLocalRef.MatchString(ref)
}

// imageAnnotation on a pod template carries the checksum of the uploaded image tarball. A source
// checkout rebuilds under the same tag (can-core:dev), so without it applying the manifest again
// changes nothing and the old pod keeps running, with its old module manifests and charts.
const imageAnnotation = "can.dev/image-sha256"

// stampImage sets imageAnnotation; sha is "" for a release image, whose reference is a digest.
func stampImage(dep map[string]any, sha string) {
	if sha != "" {
		dig(dep, "spec", "template", "metadata", "annotations")[imageAnnotation] = sha
	}
}

// CoreOptions are the per-company values patched into deploy/can-core.
type CoreOptions struct {
	Image string
	// ImageSHA256 is the checksum of the image's tarball, for an image shipped by tag.
	ImageSHA256 string
	CompanyName string
	Domain      string
	// CookieSecure must be true whenever the admin UI is reached over HTTPS.
	CookieSecure bool
	// RestoreID deploys can-core in restore mode (specs/042-restore); RestoreOverwrite, the same
	// id, lets it replace an existing state database.
	RestoreID, RestoreOverwrite string
}

func setEnv(c map[string]any, name, value string) {
	env, _ := c["env"].([]any)
	for _, e := range env {
		if m, ok := e.(map[string]any); ok && m["name"] == name {
			m["value"] = value
			delete(m, "valueFrom")
			return
		}
	}
	c["env"] = append(env, map[string]any{"name": name, "value": value})
}

func containersOf(dep map[string]any, key string) []any {
	spec, _ := dep["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	c, _ := ps[key].([]any)
	return c
}

func isDeployment(d map[string]any, name string) bool {
	md, _ := d["metadata"].(map[string]any)
	return d["kind"] == "Deployment" && md["name"] == name
}

// CoreManifests returns deploy/can-core with the image and company values patched in.
func CoreManifests(o CoreOptions) ([]byte, error) {
	if !validImageRef(o.Image) {
		return nil, fmt.Errorf("can-core image reference %q is not valid", o.Image)
	}
	raw, err := deploy.Stack.ReadFile("can-core/can-core.yaml")
	if err != nil {
		return nil, err
	}
	docs, err := parseDocs(raw)
	if err != nil {
		return nil, err
	}
	patched := false
	for _, d := range docs {
		if !isDeployment(d, "can-core") {
			continue
		}
		stampImage(d, o.ImageSHA256)
		for _, c := range containersOf(d, "containers") {
			cm := c.(map[string]any)
			cm["image"] = o.Image
			setEnv(cm, "CAN_COOKIE_SECURE", fmt.Sprint(o.CookieSecure))
			if o.CompanyName != "" {
				setEnv(cm, "CAN_COMPANY_NAME", o.CompanyName)
			}
			if o.Domain != "" {
				setEnv(cm, "CAN_COMPANY_DOMAIN", o.Domain)
			}
			if o.RestoreID != "" {
				setEnv(cm, "CAN_RESTORE", o.RestoreID)
				if o.RestoreOverwrite != "" {
					setEnv(cm, "CAN_RESTORE_OVERWRITE", o.RestoreOverwrite)
				}
				// The restore operations run as a second process in the pod (chart rendering).
				cm["resources"] = map[string]any{"requests": map[string]any{"memory": "48Mi", "cpu": "20m"}, "limits": map[string]any{"memory": "384Mi"}}
			}
			patched = true
		}
	}
	if !patched {
		return nil, errors.New("deploy/can-core has no can-core Deployment")
	}
	return marshalDocs(docs)
}

// SiteManifests returns deploy/site with the can-static image and the archive digest patched in;
// imageSHA as CoreOptions.ImageSHA256.
func SiteManifests(image, archiveSHA, imageSHA string) ([]byte, error) {
	if !validImageRef(image) {
		return nil, fmt.Errorf("can-static image reference %q is not valid", image)
	}
	if !reHex64.MatchString(archiveSHA) {
		return nil, fmt.Errorf("site archive digest %q is not 64 hex characters", archiveSHA)
	}
	raw, err := deploy.Stack.ReadFile("site/site.yaml")
	if err != nil {
		return nil, err
	}
	docs, err := parseDocs(raw)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if !isDeployment(d, "site") {
			continue
		}
		for _, key := range []string{"initContainers", "containers"} {
			for _, c := range containersOf(d, key) {
				c.(map[string]any)["image"] = image
			}
		}
		stampImage(d, imageSHA)
		tpl := d["spec"].(map[string]any)["template"].(map[string]any)
		dig(tpl, "metadata", "annotations")["can.dev/site-sha256"] = archiveSHA
	}
	return marshalDocs(docs)
}

// RouteOptions describes how traffic reaches the company.
type RouteOptions struct {
	// SiteHosts serve the public site; AdminHosts serve the admin UI (can-core).
	SiteHosts  []string
	AdminHosts []string
	// CatchAll serves the site for any host (the plain-HTTP-on-the-IP fallback). It never routes
	// the admin UI: over plain HTTP that is reachable only through the SSH tunnel.
	CatchAll bool
	TLS      bool
	// CertResolver defaults to letsencrypt, the resolver deploy/base configures.
	CertResolver string
	// NoEdgeLimit leaves the edge limits off, here and (through the routing facts) on module
	// routes: behind a proxy or CDN every client shares its addresses.
	NoEdgeLimit bool
	// NoEdgeLimitReason is recorded in the routing facts (routing.ReasonFlag, ReasonProxied).
	NoEdgeLimitReason string
}

// sitePriority is the site's catch-all; can-core's /_forms route outranks it (routing.SitePriority).
const sitePriority = 1

func (o RouteOptions) validate() error {
	if len(o.SiteHosts) == 0 && !o.CatchAll {
		return errors.New("routes: no site host and no catch-all; nothing would be served")
	}
	for _, h := range append(append([]string(nil), o.SiteHosts...), o.AdminHosts...) {
		if !reHost.MatchString(h) || len(h) > 253 {
			return fmt.Errorf("routes: host %q is not a plain lower-case DNS name", h)
		}
	}
	return nil
}

func (o RouteOptions) entry(spec map[string]any) {
	if o.TLS {
		spec["entryPoints"] = []string{"websecure"}
		cr := o.CertResolver
		if cr == "" {
			cr = "letsencrypt"
		}
		spec["tls"] = map[string]any{"certResolver": cr}
		return
	}
	spec["entryPoints"] = []string{"web"}
}

func ingressRoute(name, ns string, routes []any, o RouteOptions) map[string]any {
	spec := map[string]any{"routes": routes}
	o.entry(spec)
	return map[string]any{
		"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRoute",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}},
		"spec":     spec,
	}
}

func rule(match string, priority int, service string, port int, middlewares ...string) map[string]any {
	r := map[string]any{
		"match": match, "kind": "Rule", "priority": priority,
		"services": []any{map[string]any{"name": service, "port": port}},
	}
	if len(middlewares) > 0 {
		var ms []any
		for _, m := range middlewares {
			ms = append(ms, map[string]any{"name": m})
		}
		r["middlewares"] = ms
	}
	return r
}

// SiteRoutes returns the IngressRoutes in can-system: the site on its hosts and the admin UI on
// the admin hosts.
func SiteRoutes(o RouteOptions) ([]byte, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	var docs []map[string]any
	var siteLimit, loginLimit []string
	if !o.NoEdgeLimit {
		var m []map[string]any
		m, siteLimit = edgeMiddlewares("public", routing.PublicLimit)
		docs = append(docs, m...)
		if len(o.AdminHosts) > 0 {
			m, loginLimit = edgeMiddlewares("login", routing.LoginLimit)
			docs = append(docs, m...)
		}
	}
	var site []any
	for _, h := range o.SiteHosts {
		site = append(site, rule("Host(`"+h+"`)", sitePriority, "site", 8081, siteLimit...))
	}
	if o.CatchAll {
		site = append(site, rule("PathPrefix(`/`)", sitePriority, "site", 8081, siteLimit...))
	}
	docs = append(docs, ingressRoute("site", SystemNS, site, o))
	if len(o.AdminHosts) > 0 {
		var admin []any
		for _, h := range o.AdminHosts {
			admin = append(admin, rule("Host(`"+h+"`)", sitePriority, "can-core", 8080))
			// The rest of the admin UI and its API need a session and are not limited here.
			if len(loginLimit) > 0 {
				for _, p := range adminLoginRules {
					admin = append(admin, rule("Host(`"+h+"`) && "+p, loginPriority, "can-core", 8080, loginLimit...))
				}
			}
		}
		docs = append(docs, ingressRoute("admin", SystemNS, admin, o))
	}
	return marshalDocs(docs)
}

// CompanyConfigMap renders company.yaml as the ConfigMap can-core mounts and reconciles.
func CompanyConfigMap(content string) (string, error) {
	b, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "company", "namespace": SystemNS, "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}},
		"data":     map[string]any{"company.yaml": content},
	})
	return string(b), err
}

// WithDefaultBundle makes sure the forms module is part of the company that is pushed: `can up`
// promises a live site with working lead capture. It only adds the module when company.yaml does
// not mention it at all; an operator who wrote `forms: {enabled: false}` keeps that. The file on
// disk is never modified; the caller says so.
func WithDefaultBundle(src []byte) (out []byte, added bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, false, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, errors.New("company.yaml is not a mapping")
	}
	root := doc.Content[0]
	var modules *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "modules" {
			modules = root.Content[i+1]
		}
	}
	empty := func() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle} }
	switch {
	case modules == nil:
		modules = empty()
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "modules", Tag: "!!str"}, modules)
	case modules.Kind == yaml.ScalarNode && modules.Tag == "!!null":
		*modules = *empty()
	case modules.Kind != yaml.MappingNode:
		return nil, false, errors.New("modules: is not a mapping")
	}
	for i := 0; i+1 < len(modules.Content); i += 2 {
		if modules.Content[i].Value == "forms" {
			return src, false, nil
		}
	}
	modules.Content = append(modules.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "forms", Tag: "!!str"}, empty())
	if modules.Style == yaml.FlowStyle && len(modules.Content) > 2 {
		modules.Style = 0
	}
	b, err := yaml.Marshal(&doc)
	return b, err == nil, err
}

// SiteArchive is the built site, packed for the site ConfigMap.
type SiteArchive struct {
	TarGz  []byte
	SHA256 string
	Files  int
}

// BuildSiteArchive validates <dir>/company.yaml, renders its site (`can site build`) and packs the
// output as a deterministic tar.gz: sorted names, zero timestamps, a fixed gzip header, so an
// unchanged company yields the same digest and a re-run does not restart the site pod.
func BuildSiteArchive(dir, baseURL string, maxBytes int) (SiteArchive, error) {
	doc, ps := config.Load(filepath.Join(dir, "company.yaml"))
	if doc == nil {
		return SiteArchive{}, fmt.Errorf("cannot read %s/company.yaml", dir)
	}
	ps = append(ps, schema.Validate(schema.Company, doc)...)
	if problem.HasErrors(ps) {
		problem.Sort(ps)
		return SiteArchive{}, fmt.Errorf("company.yaml is invalid: %s", ps[0].String())
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		return SiteArchive{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return SiteArchive{}, err
	}
	defer func() { _ = root.Close() }()
	out, err := site.Render(c, site.Options{BaseURL: baseURL, Assets: root.FS()})
	if err != nil {
		return SiteArchive{}, fmt.Errorf("render site: %w", err)
	}
	tmp, err := os.MkdirTemp("", "can-site-")
	if err != nil {
		return SiteArchive{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := out.Write(tmp, site.WriteOptions{Compress: true}); err != nil {
		return SiteArchive{}, fmt.Errorf("write site: %w", err)
	}
	var names []string
	err = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(tmp, p)
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return SiteArchive{}, err
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(n))) //nolint:gosec // walked from our own temp dir
		if err != nil {
			return SiteArchive{}, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b)), Format: tar.FormatPAX}); err != nil {
			return SiteArchive{}, err
		}
		if _, err := tw.Write(b); err != nil {
			return SiteArchive{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return SiteArchive{}, err
	}
	if err := gz.Close(); err != nil {
		return SiteArchive{}, err
	}
	if buf.Len() > maxBytes {
		return SiteArchive{}, fmt.Errorf("the built site is %d KiB compressed, over the %d KiB that fits in a Kubernetes ConfigMap; shrink the logo or other assets in company.yaml's site section (or serve large assets from elsewhere)", buf.Len()>>10, maxBytes>>10)
	}
	h := sha256.Sum256(buf.Bytes())
	return SiteArchive{TarGz: buf.Bytes(), SHA256: hex.EncodeToString(h[:]), Files: len(names)}, nil
}

// SiteContentConfigMap renders the ConfigMap the site pod unpacks.
func SiteContentConfigMap(a SiteArchive) string {
	b, _ := yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "site-content", "namespace": SystemNS,
			"labels":      map[string]any{"app.kubernetes.io/part-of": "companyinacan"},
			"annotations": map[string]any{"can.dev/site-sha256": a.SHA256},
		},
		"binaryData": map[string]any{"site.tar.gz": base64.StdEncoding.EncodeToString(a.TarGz)},
	})
	return string(b)
}

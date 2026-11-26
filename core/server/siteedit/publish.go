package siteedit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/site"
)

// ErrNotReady means publishing cannot work in this installation as it is.
var ErrNotReady = errors.New("publishing is not possible")

// ErrNoSite means there is no published site to replace.
var ErrNoSite = errors.New("no site has been published to this installation yet: run `can up` first")

// Published describes the site that is live now.
type Published struct {
	SHA256, BaseURL, SourceSHA256 string
	LogoName                      string
	Logo                          []byte
	version                       string // concurrency token of the publisher
}

// Release is a site to publish.
type Release struct {
	Output                        *site.Output
	Archive                       []byte
	SHA256, BaseURL, SourceSHA256 string
}

// Publisher replaces the public website.
type Publisher interface {
	// Current describes the live site; ErrNoSite when there is none.
	Current(ctx context.Context) (Published, error)
	// Ready reports why publishing cannot work, before anything is changed.
	Ready(ctx context.Context) error
	// Publish replaces the site prev describes.
	Publish(ctx context.Context, prev Published, r Release) error
}

// The objects of deploy/site that publishing touches.
const (
	siteNamespace  = "can-system"
	siteConfigMap  = "site-content"
	siteDeployment = "site"
	archiveKey     = "site.tar.gz"
)

// AdmissionPolicies must be in force before can-core publishes: they refuse its other changes to
// that ConfigMap and to long-running workloads of can-system. They do not confine it there
// altogether (deploy/base/admission-policy.yaml says what is left).
var AdmissionPolicies = []string{"can-core-site", "can-core-site-content"}

// KubePublisher publishes the way `can up` does: the archive into ConfigMap can-system/site-content,
// then its digest onto the site Deployment's pod template, which rolls the pod. Nothing else.
type KubePublisher struct{ API kube.API }

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// Current implements Publisher.
func (k KubePublisher) Current(ctx context.Context) (Published, error) {
	o, err := k.API.Get(ctx, "ConfigMap", siteNamespace, siteConfigMap)
	if kube.IsNotFound(err) {
		return Published{}, ErrNoSite
	}
	if err != nil {
		return Published{}, err
	}
	md := asMap(o["metadata"])
	an := asMap(md["annotations"])
	p := Published{SHA256: str(an[site.AnnotationSHA]), BaseURL: str(an[site.AnnotationBaseURL]), SourceSHA256: str(an[site.AnnotationSource]), version: str(md["resourceVersion"])}
	b, err := base64.StdEncoding.DecodeString(str(asMap(o["binaryData"])[archiveKey]))
	if err != nil || len(b) == 0 {
		return Published{}, ErrNoSite
	}
	if p.LogoName, p.Logo, err = site.ArchiveLogo(b); err != nil {
		return Published{}, fmt.Errorf("the published site cannot be read: %w", err)
	}
	return p, nil
}

// Ready implements Publisher.
func (k KubePublisher) Ready(ctx context.Context) error {
	notReady := func(what string, err error) error {
		return fmt.Errorf("%w: %s (%w); run `can up` to update the server", ErrNotReady, what, err)
	}
	for _, name := range AdmissionPolicies {
		if _, err := k.API.Get(ctx, "ValidatingAdmissionPolicy", "", name); err != nil {
			return notReady("the admission policy "+name+" is missing", err)
		}
		b, err := k.API.Get(ctx, "ValidatingAdmissionPolicyBinding", "", name)
		if err != nil {
			return notReady("the admission policy "+name+" is not bound", err)
		}
		actions, _ := asMap(b["spec"])["validationActions"].([]any)
		if !slices.Contains(actions, any("Deny")) {
			return notReady("the admission policy "+name+" does not deny", errors.New("check its binding"))
		}
	}
	if _, err := k.API.Get(ctx, "Deployment", siteNamespace, siteDeployment); err != nil {
		return notReady("the site is not deployed", err)
	}
	return nil
}

// Publish implements Publisher.
func (k KubePublisher) Publish(ctx context.Context, prev Published, r Release) error {
	md := kube.Object{"annotations": map[string]any{site.AnnotationSHA: r.SHA256, site.AnnotationBaseURL: r.BaseURL, site.AnnotationSource: r.SourceSHA256}}
	if prev.version != "" {
		md["resourceVersion"] = prev.version // `can up` may be publishing right now
	}
	err := k.API.MergePatch(ctx, "ConfigMap", siteNamespace, siteConfigMap, kube.Object{
		"metadata": md, "binaryData": map[string]any{archiveKey: base64.StdEncoding.EncodeToString(r.Archive)},
	})
	if kube.IsConflict(err) {
		return errors.New("the published site changed meanwhile; publish again")
	}
	if err != nil {
		return fmt.Errorf("writing the site content: %w", err)
	}
	err = k.API.MergePatch(ctx, "Deployment", siteNamespace, siteDeployment, kube.Object{
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{site.AnnotationSHA: r.SHA256}}}},
	})
	if err != nil {
		return fmt.Errorf("restarting the site: %w", err)
	}
	return nil
}

// DirPublisher writes the site into a directory a can-static serves (Docker Compose and other
// setups without Kubernetes). The directory must already hold a built site.
type DirPublisher struct{ Dir string }

// metaFile is hidden: can-static serves no dot files.
const metaFile = ".can-published.json"

type dirMeta struct {
	SHA256       string `json:"sha256"`
	BaseURL      string `json:"base_url"`
	SourceSHA256 string `json:"source_sha256"`
}

// Current implements Publisher.
func (d DirPublisher) Current(context.Context) (Published, error) {
	if _, err := os.Stat(filepath.Join(d.Dir, "index.html")); errors.Is(err, fs.ErrNotExist) {
		return Published{}, ErrNoSite
	} else if err != nil {
		return Published{}, err
	}
	var m dirMeta
	b, err := os.ReadFile(filepath.Join(d.Dir, metaFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Published{}, err
	default:
		if err := json.Unmarshal(b, &m); err != nil {
			return Published{}, err
		}
	}
	p := Published{SHA256: m.SHA256, BaseURL: m.BaseURL, SourceSHA256: m.SourceSHA256}
	entries, err := os.ReadDir(filepath.Join(d.Dir, "assets"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Published{}, err
	}
	for _, e := range entries {
		name := path.Join("assets", e.Name())
		if !site.IsLogoPath(name) || strings.HasSuffix(name, ".gz") {
			continue
		}
		if p.Logo, err = os.ReadFile(filepath.Join(d.Dir, "assets", e.Name())); err != nil { //nolint:gosec // our own site directory
			return Published{}, err
		}
		p.LogoName = name
		break
	}
	return p, nil
}

// Ready implements Publisher.
func (d DirPublisher) Ready(context.Context) error {
	if _, err := os.Stat(filepath.Join(d.Dir, site.MarkerFile)); err != nil {
		return fmt.Errorf("%w: %s holds no built site: %w", ErrNotReady, d.Dir, err)
	}
	return nil
}

// Publish implements Publisher.
func (d DirPublisher) Publish(_ context.Context, _ Published, r Release) error {
	if err := r.Output.Write(d.Dir, site.WriteOptions{Compress: true}); err != nil {
		return err
	}
	b, err := json.Marshal(dirMeta{SHA256: r.SHA256, BaseURL: r.BaseURL, SourceSHA256: r.SourceSHA256})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d.Dir, metaFile), b, 0o644) //nolint:gosec // public content digests
}

// Package modimages knows the container images a company needs: the in-house images this
// repository builds (images.txt: the one source of their names and versions) and the
// images the charts and hooks of the enabled modules name.
package modimages

import (
	_ "embed" // images.txt
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
)

//go:embed images.txt
var imagesTxt string

// Registry is where the release workflow publishes the in-house images and where the charts expect them.
const Registry = "ghcr.io/dodwmd"

// Image is one in-house image. Version is the tag the charts and hooks pull it by, "-" for the
// images no chart pulls.
type Image struct{ Name, Dockerfile, Context, Version string }

// All lists the in-house images in the order of images.txt.
func All() []Image {
	var out []Image
	for _, l := range strings.Split(imagesTxt, "\n") {
		if f := strings.Fields(l); len(f) == 4 && !strings.HasPrefix(l, "#") {
			out = append(out, Image{Name: f[0], Dockerfile: f[1], Context: f[2], Version: f[3]})
		}
	}
	return out
}

// Names lists the in-house image names.
func Names() []string {
	var out []string
	for _, i := range All() {
		out = append(out, i.Name)
	}
	return out
}

// Ref is the reference `make images` records for the image: what its chart pulls, or a local one
// for an image deployed by the reference in the images index. "" for a name that is not built.
func Ref(name string) string {
	for _, i := range All() {
		switch {
		case i.Name != name:
		case i.Version == "-":
			return name + ":dev"
		default:
			return Registry + "/" + name + ":" + i.Version
		}
	}
	return ""
}

// Required maps each image the enabled modules need to the ids that need it: every image of the
// module's chart values (a section with `enabled: false` is not deployed) and the image a script
// backup hook runs in. A module without chart templates installs nothing and needs nothing.
// Modules whose manifest or values cannot be read are returned in unknown.
func Required(modulesDir string, ids []string) (refs map[string][]string, unknown []string) {
	return RequiredFS(os.DirFS(modulesDir), ids)
}

// RequiredFS is Required over a modules directory given as a file system.
func RequiredFS(modules fs.FS, ids []string) (refs map[string][]string, unknown []string) {
	refs = map[string][]string{}
	ids = slices.Clone(ids)
	sort.Strings(ids)
	for _, id := range ids {
		list, err := forModule(modules, id)
		if err != nil {
			unknown = append(unknown, fmt.Sprintf("module %q: %v", id, err))
			continue
		}
		for _, r := range list {
			refs[r] = append(refs[r], id)
		}
	}
	return refs, unknown
}

// ForModule lists the images of one module (see Required).
func ForModule(modulesDir, id string) ([]string, error) {
	return forModule(os.DirFS(modulesDir), id)
}

func forModule(modules fs.FS, id string) ([]string, error) {
	dir := path.Clean("/" + id)[1:]
	if _, err := fs.Stat(modules, dir); errors.Is(err, fs.ErrNotExist) {
		dir = "_" + dir // `_id` directories are examples
	}
	manifest := path.Join(dir, "module.yaml")
	data, err := fs.ReadFile(modules, manifest)
	if err != nil {
		return nil, fmt.Errorf("module.yaml cannot be read: %w", err)
	}
	doc, ps := config.LoadBytes(manifest, data)
	if doc == nil {
		return nil, fmt.Errorf("module.yaml cannot be read: %v", ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		return nil, fmt.Errorf("module.yaml: %w", err)
	}
	inside := func(p string) string { return path.Join(dir, path.Clean("/"+p)) }
	chart := inside(m.Runtime.Chart)
	if _, err := fs.Stat(modules, path.Join(chart, "templates")); err != nil {
		return nil, nil
	}
	values := path.Join(chart, "values.yaml")
	if m.Runtime.Values != "" {
		values = inside(m.Runtime.Values)
	}
	b, err := fs.ReadFile(modules, values)
	if err != nil {
		return nil, fmt.Errorf("chart values: %w", err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("chart values: %w", err)
	}
	var out []string
	walk(v, &out)
	// The same condition as dumps.KubeHooks: only a script hook runs in hooks.image.
	if h := m.Hooks; h.Backup != "" && h.Command == "" && h.Builtin == nil && h.Image != "" {
		out = append(out, h.Image)
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// walk collects every `{repository, tag, digest}` of the values, as the charts' mod.image template
// writes it, skipping sections that are switched off. `config` is the application's own settings.
func walk(v map[string]any, out *[]string) {
	if on, ok := v["enabled"].(bool); ok && !on {
		return
	}
	if repo, _ := v["repository"].(string); repo != "" {
		ref := repo
		if tag := v["tag"]; tag != nil && fmt.Sprint(tag) != "" {
			ref += ":" + fmt.Sprint(tag)
		}
		if d, _ := v["digest"].(string); d != "" && !strings.Contains(d, "REPLACE_WITH_") {
			ref += "@" + d
		}
		*out = append(*out, ref)
	}
	for k, child := range v {
		if m, ok := child.(map[string]any); ok && k != "config" {
			walk(m, out)
		}
	}
}

// Normalize writes a reference the way containerd records it, so that references can be compared:
// with the registry, `library/` for Docker Hub's official images, and only the digest when there
// is one (otherwise a tag, `latest` by default).
func Normalize(ref string) string {
	name, digest, _ := strings.Cut(ref, "@")
	tag := ""
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
	}
	host, rest, ok := strings.Cut(name, "/")
	if !ok || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, rest = "docker.io", name
	}
	if host == "docker.io" && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	if digest != "" {
		return host + "/" + rest + "@" + digest
	}
	if tag == "" {
		tag = "latest"
	}
	return host + "/" + rest + ":" + tag
}

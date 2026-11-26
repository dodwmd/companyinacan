package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/deploy"
)

// Content is what a module is installed from, by content rather than by version number: a change of
// any of it is an upgrade, whether or not the manifest's version changed (a can-core upgrade
// replaces the modules directory and the templates it carries).
type Content struct {
	// Manifest is the hash of module.yaml as parsed: comments and layout do not count.
	Manifest string `json:"manifest"`
	// Chart is the hash of the chart's files (documentation aside) and the values file.
	Chart string `json:"chart,omitempty"`
	// Platform is the hash of what can-core itself puts around the module: the namespace template
	// and the list of secrets it generates.
	Platform string `json:"platform,omitempty"`
	// Images are the image references of the chart values, for the preview (Chart covers them).
	Images []string `json:"images,omitempty"`
}

// Bounds on what is read from a module directory.
const (
	maxContentFiles = 500
	maxContentBytes = 1 << 20
)

// writePart length-prefixes p, so that moving bytes between parts changes the hash.
func writePart(w io.Writer, p []byte) {
	_, _ = fmt.Fprintf(w, "%d:", len(p))
	_, _ = w.Write(p)
}

func digest(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		writePart(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// manifestContent is the Content of a manifest held in memory only.
func manifestContent(m *config.Module) Content {
	c := *m
	c.Runtime.RoutedUIHost = "" // can-core's own addition to the recorded copy
	b, _ := json.Marshal(&c)    // struct order and sorted map keys: stable
	return Content{Manifest: digest(b)}
}

// ContentOf reads the Content of the module whose manifest file is manifestPath.
func ContentOf(manifestPath string, m *config.Module) (Content, error) {
	raw, err := os.ReadFile(manifestPath) //nolint:gosec // inside the modules directory
	if err != nil {
		return Content{}, err
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Content{}, err
	}
	canonical, err := json.Marshal(doc)
	if err != nil { // a mapping with non-string keys: the bytes themselves
		canonical = raw
	}
	c := Content{Manifest: digest(canonical), Platform: platformContent(m.ID)}
	if m.Runtime.Chart == "" {
		return c, nil
	}
	dir := filepath.Dir(manifestPath)
	inside := func(rel string) string { return filepath.Join(dir, filepath.Clean("/"+rel)) }
	chart := inside(m.Runtime.Chart)
	h := sha256.New()
	files := 0
	add := func(path string) error {
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return err // symlinks are not followed; the renderer refuses them
		}
		if files++; files > maxContentFiles || fi.Size() > maxContentBytes {
			return fmt.Errorf("module %s: the chart is too large", m.ID)
		}
		b, err := os.ReadFile(path) //nolint:gosec // inside the module directory
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		writePart(h, []byte(filepath.ToSlash(rel)))
		writePart(h, b)
		return nil
	}
	if _, err := os.Stat(chart); err == nil {
		// WalkDir visits in lexical order, so the hash does not depend on the filesystem.
		err := filepath.WalkDir(chart, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if path != chart && strings.HasPrefix(name, ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || strings.HasSuffix(name, ".md") || strings.HasSuffix(name, "~") {
				return nil
			}
			return add(path)
		})
		if err != nil {
			return Content{}, err
		}
	}
	if m.Runtime.Values != "" {
		if v := inside(m.Runtime.Values); !strings.HasPrefix(v, chart+string(filepath.Separator)) {
			if err := add(v); err != nil && !os.IsNotExist(err) {
				return Content{}, err
			}
		}
	}
	c.Chart = hex.EncodeToString(h.Sum(nil))
	c.Images = chartImages(filepath.Join(chart, "values.yaml"))
	return c, nil
}

func platformContent(id string) string {
	tpl, _ := deploy.Templates.ReadFile("templates/module-namespace.yaml")
	specs, _ := json.Marshal(modsecrets.For(id))
	return digest(tpl, specs)
}

// chartImages lists the `{repository, tag, digest}` sections of a values file, skipping sections
// that are switched off and the application's own `config`.
func chartImages(path string) []string {
	b, err := os.ReadFile(path) //nolint:gosec // inside the module directory
	if err != nil {
		return nil
	}
	var v map[string]any
	if yaml.Unmarshal(b, &v) != nil {
		return nil
	}
	var out []string
	var walk func(map[string]any)
	walk = func(v map[string]any) {
		if on, ok := v["enabled"].(bool); ok && !on {
			return
		}
		if repo, _ := v["repository"].(string); repo != "" {
			if tag := v["tag"]; tag != nil && fmt.Sprint(tag) != "" {
				repo += ":" + fmt.Sprint(tag)
			}
			if d, _ := v["digest"].(string); d != "" && !strings.Contains(d, "REPLACE_WITH_") {
				repo += "@" + d
			}
			out = append(out, repo)
		}
		for k, child := range v {
			if m, ok := child.(map[string]any); ok && k != "config" {
				walk(m)
			}
		}
	}
	walk(v)
	sort.Strings(out)
	return slices.Compact(out)
}

// changes says what differs from the recorded content, for the plan's reason.
func (c Content) changes(rec Content) []string {
	var out []string
	if c.Manifest != rec.Manifest {
		out = append(out, "manifest changed")
	}
	if c.Chart != rec.Chart {
		added, removed := missing(c.Images, rec.Images), missing(rec.Images, c.Images)
		if len(added)+len(removed) > 0 {
			out = append(out, fmt.Sprintf("image %s -> %s", orNone(removed), orNone(added)))
		} else {
			out = append(out, "chart changed")
		}
	}
	if c.Platform != rec.Platform {
		out = append(out, "can-core's module templates changed")
	}
	return out
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "(none)"
	}
	return strings.Join(l, ", ")
}

// missing returns the elements of a that b does not have.
func missing(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func (c Content) key() string { return c.Manifest + c.Chart + c.Platform }

func (c Content) encode() string {
	b, _ := json.Marshal(c)
	return string(b)
}

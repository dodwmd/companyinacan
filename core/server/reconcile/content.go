package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/validate"
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
	// Scripts are the hook scripts, so that a backup of a module whose upgrade is held runs the
	// scripts of the release that is installed.
	Scripts map[string]string `json:"scripts,omitempty"`
	// Egress says whether the module was installed with internet access. nil: installed by a
	// can-core whose manifests had no say in it, and every module had access.
	Egress *bool `json:"egress,omitempty"`
}

// internetEgress reads runtime.internet_egress by name, so that this works whether or not this
// build's manifest type declares it (known is false when it does not).
func internetEgress(m *config.Module) (declared, known bool) {
	v := reflect.ValueOf(m.Runtime).FieldByName("InternetEgress")
	if !v.IsValid() || v.Kind() != reflect.Bool {
		return false, false
	}
	return v.Bool(), true
}

func egressOf(m *config.Module) *bool {
	if on, known := internetEgress(m); known {
		return &on
	}
	return nil
}

// newInternetAccess reports whether next gives internet access to a module recorded without it. A
// record from before the manifest had the field had access, so asking for it then is nothing new.
func newInternetAccess(recorded string, next *config.Module) bool {
	if on, _ := internetEgress(next); !on {
		return false
	}
	var was Content
	return recorded != "" && json.Unmarshal([]byte(recorded), &was) == nil && was.Egress != nil && !*was.Egress
}

// inModule is a manifest's relative path as a name in the module's file system; it cannot leave it.
func inModule(rel string) string {
	if p := strings.TrimPrefix(path.Clean("/"+rel), "/"); p != "" {
		return p
	}
	return "."
}

// readRegular reads a file of at most limit bytes; ok is false for anything else (a symlink is not
// followed: the renderer refuses them).
func readRegular(module fs.FS, name string, limit int64) (b []byte, ok bool, err error) {
	entries, err := fs.ReadDir(module, path.Dir(name))
	if err != nil {
		return nil, false, nil //nolint:nilerr // no such directory: no such file
	}
	for _, e := range entries {
		if e.Name() != path.Base(name) || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, false, err
		}
		if fi.Size() > limit {
			return nil, false, fmt.Errorf("%s is larger than %d bytes", name, limit)
		}
		b, err := fs.ReadFile(module, name)
		return b, err == nil, err
	}
	return nil, false, nil
}

// hookScripts reads the script files module.yaml's hooks name, keyed by the path as written.
func hookScripts(module fs.FS, m *config.Module) map[string]string {
	if m.Hooks.Command != "" {
		return nil // subcommands of the module's own image
	}
	out := map[string]string{}
	for _, rel := range []string{m.Hooks.Install, m.Hooks.Backup, m.Hooks.Restore, m.Hooks.Purge, m.Hooks.Export, m.Hooks.Import} {
		if rel == "" {
			continue
		}
		if b, ok, err := readRegular(module, inModule(rel), maxScriptBytes); ok && err == nil {
			out[rel] = string(b)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Bounds on what is read from a module directory.
const (
	maxContentFiles = 500
	maxContentBytes = 1 << 20
	maxScriptBytes  = 64 << 10
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
	return Content{Manifest: digest(b), Egress: egressOf(m)}
}

// ContentOf reads the Content of the module whose manifest is at manifestPath, as
// validate.ManifestPath returns it: a file on disk, or one of the modules built into `can`. Both
// hash the same files (modules/embed.go embeds everything that is hashed), so a company without a
// modules directory sees what can-core, which has the full one, sees.
func ContentOf(manifestPath string, m *config.Module) (Content, error) {
	module, err := validate.ModuleFS(manifestPath)
	if err != nil {
		return Content{}, err
	}
	raw, err := fs.ReadFile(module, path.Base(filepath.ToSlash(manifestPath)))
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
	c := Content{Manifest: digest(canonical), Platform: platformContent(m.ID), Scripts: hookScripts(module, m), Egress: egressOf(m)}
	if m.Runtime.Chart == "" {
		return c, nil
	}
	chart := inModule(m.Runtime.Chart)
	h := sha256.New()
	files := 0
	add := func(name string) error {
		b, ok, err := readRegular(module, name, maxContentBytes)
		if err != nil || !ok {
			return err
		}
		if files++; files > maxContentFiles {
			return fmt.Errorf("module %s: the chart has too many files", m.ID)
		}
		writePart(h, []byte(name))
		writePart(h, b)
		return nil
	}
	if _, err := fs.Stat(module, chart); err == nil {
		// WalkDir visits in lexical order, so the hash does not depend on the filesystem.
		err := fs.WalkDir(module, chart, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if p != chart && strings.HasPrefix(name, ".") {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() || strings.HasSuffix(name, ".md") || strings.HasSuffix(name, "~") {
				return nil
			}
			return add(p)
		})
		if err != nil {
			return Content{}, err
		}
	}
	if m.Runtime.Values != "" {
		if v := inModule(m.Runtime.Values); !strings.HasPrefix(v, chart+"/") {
			if err := add(v); err != nil {
				return Content{}, err
			}
		}
	}
	c.Chart = hex.EncodeToString(h.Sum(nil))
	c.Images = chartImages(module, path.Join(chart, "values.yaml"))
	return c, nil
}

func platformContent(id string) string {
	tpl, _ := deploy.Templates.ReadFile("templates/module-namespace.yaml")
	specs, _ := json.Marshal(modsecrets.For(id))
	return digest(tpl, specs)
}

// chartImages lists the `{repository, tag, digest}` sections of a values file, skipping sections
// that are switched off and the application's own `config`.
func chartImages(module fs.FS, name string) []string {
	b, err := fs.ReadFile(module, name)
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
	if c.scriptsKey() != rec.scriptsKey() {
		out = append(out, "hook scripts changed")
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

func (c Content) scriptsKey() string {
	b, _ := json.Marshal(c.Scripts) // sorted keys
	return digest(b)
}

func (c Content) key() string { return c.Manifest + c.Chart + c.Platform + c.scriptsKey() }

func (c Content) encode() string {
	b, _ := json.Marshal(c)
	return string(b)
}

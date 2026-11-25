// Package modmgr is the module-management logic behind the admin UI (epic E12b): the catalogue of
// manifests, the footprint meter and capability graph, settings forms generated from each module's
// JSON Schema, company.yaml patches with optimistic concurrency, change previews and applies, the
// onboarding wizard state and the health snapshot. It has no HTTP in it; core/server/adminui and
// core/server/adminapi render it. Cluster work stays behind the interfaces in this package and in
// core/server/reconcile.
package modmgr

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/validate"
)

// Bounds on what is read from CAN_MODULES_DIR (baked into the image, but still parsed defensively).
const (
	MaxModules       = 200
	MaxManifestBytes = 256 << 10
	MaxSchemaBytes   = 64 << 10
)

// Entry is one module of the catalogue.
type Entry struct {
	ID       string
	Dir      string
	Manifest *config.Module
	// Schema is the compiled settings schema; nil when the module has none or it is unusable.
	Schema        *SettingsSchema
	SchemaProblem string
	// Invalid is why the manifest cannot be enabled (parse or validation errors); the entry is
	// still listed so the operator sees why.
	Invalid string
}

// Status is ready (the default), experimental or not-deployable.
func (e *Entry) Status() string {
	if e.Manifest == nil || e.Manifest.Status == "" {
		return "ready"
	}
	return e.Manifest.Status
}

// Deployable reports whether the module may be enabled: a not-deployable status or an invalid
// manifest never is.
func (e *Entry) Deployable() bool { return e.Status() != "not-deployable" && e.Invalid == "" }

// SourceAvailable reports licence_class: source-available (enabling needs an explicit opt-in).
func (e *Entry) SourceAvailable() bool {
	return e.Manifest != nil && e.Manifest.Provider.LicenceClass == "source-available"
}

// Catalogue is every manifest found in the modules directory.
type Catalogue struct {
	Entries []*Entry // ordered by id
	// Truncated is set when the directory held more than MaxModules modules.
	Truncated bool
	byID      map[string]*Entry
}

// Get returns the entry for id, or nil.
func (c *Catalogue) Get(id string) *Entry { return c.byID[id] }

// Manifests maps id to manifest for every valid entry.
func (c *Catalogue) Manifests() map[string]*config.Module {
	out := map[string]*config.Module{}
	for _, e := range c.Entries {
		if e.Manifest != nil {
			out[e.ID] = e.Manifest
		}
	}
	return out
}

// LoadCatalogue reads every <dir>/<id>/module.yaml. Directories that start with "_" or "." and
// symbolic links are skipped; reading is bounded in count and size.
func LoadCatalogue(dir string) (*Catalogue, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	c := &Catalogue{byID: map[string]*Entry{}}
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || de.Type()&os.ModeSymlink != 0 || !de.IsDir() {
			continue
		}
		if len(c.Entries) >= MaxModules {
			c.Truncated = true
			break
		}
		e := loadEntry(filepath.Join(dir, name), name)
		c.Entries = append(c.Entries, e)
		c.byID[e.ID] = e
	}
	sort.Slice(c.Entries, func(i, j int) bool { return c.Entries[i].ID < c.Entries[j].ID })
	return c, nil
}

func loadEntry(dir, name string) *Entry {
	e := &Entry{ID: name, Dir: dir}
	path := filepath.Join(dir, "module.yaml")
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		e.Invalid = "no module.yaml"
		return e
	case !fi.Mode().IsRegular():
		e.Invalid = "module.yaml is not a regular file"
		return e
	case fi.Size() > MaxManifestBytes:
		e.Invalid = fmt.Sprintf("module.yaml is larger than %d KiB", MaxManifestBytes>>10)
		return e
	}
	doc, ps := config.Load(path)
	if doc == nil {
		e.Invalid = firstProblem(ps)
		return e
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		e.Invalid = "module.yaml cannot be read"
		return e
	}
	e.Manifest = m
	if m.ID != name {
		e.Invalid = fmt.Sprintf("module id %q does not match its directory %q", m.ID, name)
		return e
	}
	if ps := validate.ModuleFile(path); problem.HasErrors(ps) {
		e.Invalid = firstProblem(ps)
	}
	if m.SettingsSchema != "" {
		e.Schema, e.SchemaProblem = loadSchema(dir, m.SettingsSchema)
	}
	return e
}

func firstProblem(ps []problem.Problem) string {
	for _, p := range ps {
		if p.Severity == problem.Error {
			return p.Message
		}
	}
	return "invalid manifest"
}

// loadSchema reads a settings schema that must stay inside the module directory.
func loadSchema(dir, rel string) (*SettingsSchema, string) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, "settings_schema must stay inside the module directory"
	}
	path := filepath.Join(dir, clean)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, "settings schema file not found"
	}
	if fi.Size() > MaxSchemaBytes {
		return nil, fmt.Sprintf("settings schema is larger than %d KiB", MaxSchemaBytes>>10)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // inside the module directory, checked above
	if err != nil {
		return nil, "settings schema cannot be read"
	}
	s, err := compileSettingsSchema(raw)
	if err != nil {
		return nil, "settings schema is not usable: " + err.Error()
	}
	return s, ""
}

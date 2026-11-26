// Package lazytmpl parses HTML page templates on first use. Parsing every page at start costs a
// few MiB of heap in a service budgeted at tens of MiB (docs/measurements/footprint.md), and most
// installations never open most pages. Each UI has a test calling ParseAll, so a broken template
// still fails the build rather than a request.
package lazytmpl

import (
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"strings"
	"sync"
)

// Set is the pages of one UI: every *.html in Dir except layout.html and partials.html, each
// parsed together with those two.
type Set struct {
	FS    fs.FS
	Dir   string
	Funcs template.FuncMap

	mu    sync.Mutex
	cache map[string]*template.Template
}

func shared(name string) bool { return name == "layout.html" || name == "partials.html" }

// Get returns the page template for the file name (for example "login.html").
func (s *Set) Get(name string) (*template.Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.cache[name]; ok {
		return t, nil
	}
	if shared(name) || strings.ContainsAny(name, `/\*?[`) {
		return nil, fmt.Errorf("template %q: %w", name, fs.ErrNotExist)
	}
	t, err := template.New(name).Funcs(s.Funcs).ParseFS(s.FS,
		path.Join(s.Dir, "layout.html"), path.Join(s.Dir, "partials.html"), path.Join(s.Dir, name))
	if err != nil {
		return nil, err
	}
	if s.cache == nil {
		s.cache = map[string]*template.Template{}
	}
	s.cache[name] = t
	return t, nil
}

// ParseAll parses every page; tests use it.
func (s *Set) ParseAll() error {
	entries, err := fs.ReadDir(s.FS, s.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if shared(e.Name()) {
			continue
		}
		if _, err := s.Get(e.Name()); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
	}
	return nil
}

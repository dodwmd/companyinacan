package installer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"
)

// Sandbox limits of the renderer. A chart is reviewed repository code, but a hostile or buggy one
// must not be able to take can-core down (stack overflow is fatal in Go, a huge allocation is an
// OOM kill), so every amplification path is bounded.
const (
	maxIncludeDepth = 20
	maxIndent       = 1024
	maxStringBytes  = 1 << 20 // any single string a template function produces
	maxObjectBytes  = 1 << 20 // rendered output of one template file
	maxTotalBytes   = 8 << 20 // rendered output of the whole chart
	maxFileBytes    = 1 << 20 // any chart file read from disk
	renderTimeout   = 10 * time.Second
)

// ReleaseInfo is the .Release object of a chart.
type ReleaseInfo struct {
	Name, Namespace string
	Chart, Version  string
}

type renderState struct {
	depth    int
	deadline time.Time
}

// limitWriter fails once more than max bytes were written.
type limitWriter struct {
	buf bytes.Buffer
	max int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		return 0, fmt.Errorf("rendered output exceeds %d bytes", w.max)
	}
	return w.buf.Write(p)
}

// readInside reads a chart file, refusing symlinks and anything that resolves outside root (the
// resolved chart directory) or is larger than maxFileBytes. A missing file is fs.ErrNotExist.
func readInside(root, path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refused", filepath.Base(path))
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s resolves outside the chart; refused", filepath.Base(path))
	}
	if info.Size() > maxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes; refused", filepath.Base(path), maxFileBytes)
	}
	return os.ReadFile(resolved) //nolint:gosec // resolved inside the chart directory
}

// RenderChart renders chartDir/templates with a Helm-compatible subset of the template language and
// returns the objects. Supported: .Values, .Release.{Name,Namespace}, .Chart.{Name,Version},
// named templates (define/include/template from *.tpl and *.yaml), and the functions in funcs below.
// Not supported (a chart using them fails with an "undefined function" error): lookup, tpl,
// subcharts, hooks, .Capabilities, .Files. This is not Helm: see research R16 for the list and the
// sandbox limits (include depth, output size, string size, indentation, wall-clock).
func RenderChart(chartDir string, overrides map[string]any, rel ReleaseInfo) (out []map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("chart rendering panicked: %v", r)
		}
	}()
	root, err := filepath.EvalSymlinks(chartDir)
	if err != nil {
		return nil, fmt.Errorf("chart directory: %w", err)
	}
	tdir := filepath.Join(root, "templates")
	if info, err := os.Lstat(tdir); err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("chart has no templates directory (or it is a symlink)")
	}
	ents, err := os.ReadDir(tdir)
	if err != nil {
		return nil, fmt.Errorf("chart has no templates directory: %w", err)
	}
	values := map[string]any{}
	switch b, err := readInside(root, filepath.Join(root, "values.yaml")); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("values.yaml: %w", err)
	default:
		// yaml errors can quote the offending text; do not echo file contents.
		if err := yaml.Unmarshal(b, &values); err != nil {
			return nil, errors.New("values.yaml is not valid YAML")
		}
	}
	merged := deepMerge(values, overrides)
	if rel.Chart == "" {
		rel.Chart, rel.Version = chartMeta(root)
	}
	st := &renderState{deadline: time.Now().Add(renderTimeout)}
	rt := template.New("chart").Option("missingkey=zero")
	rt.Funcs(funcs(rt, st))
	var names []string
	for _, e := range ents {
		n := e.Name()
		templated := strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".tpl")
		if e.IsDir() || !templated {
			continue
		}
		b, err := readInside(root, filepath.Join(tdir, n))
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", n, err)
		}
		if _, err := rt.New(n).Parse(string(b)); err != nil {
			return nil, fmt.Errorf("template %s does not parse", n)
		}
		if !strings.HasPrefix(n, "_") && !strings.HasSuffix(n, ".tpl") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	data := map[string]any{
		"Values":  merged,
		"Release": map[string]any{"Name": rel.Name, "Namespace": rel.Namespace, "Service": "can-core"},
		"Chart":   map[string]any{"Name": rel.Chart, "Version": rel.Version},
	}
	total := 0
	for _, n := range names {
		w := &limitWriter{max: maxObjectBytes}
		st.depth = 0
		if err := rt.ExecuteTemplate(w, n, data); err != nil {
			return nil, fmt.Errorf("rendering %s: %w", n, err)
		}
		if total += w.buf.Len(); total > maxTotalBytes {
			return nil, fmt.Errorf("rendered chart exceeds %d bytes", maxTotalBytes)
		}
		text := strings.ReplaceAll(w.buf.String(), "<no value>", "")
		dec := yaml.NewDecoder(strings.NewReader(text))
		for {
			var doc map[string]any
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s is not valid YAML after rendering", n)
			}
			if len(doc) == 0 {
				continue
			}
			if k, _ := doc["kind"].(string); k == "" {
				return nil, fmt.Errorf("%s: a rendered document has no kind", n)
			}
			if v, _ := doc["apiVersion"].(string); v == "" {
				return nil, fmt.Errorf("%s: a rendered document has no apiVersion", n)
			}
			out = append(out, doc)
		}
	}
	return out, nil
}

func chartMeta(root string) (name, version string) {
	b, err := readInside(root, filepath.Join(root, "Chart.yaml"))
	if err != nil {
		return "", ""
	}
	var c struct{ Name, Version string }
	_ = yaml.Unmarshal(b, &c)
	return c.Name, c.Version
}

// deepMerge returns base overlaid with over (maps merge, everything else is replaced).
func deepMerge(base, over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := v.(map[string]any)
		if bok && ook {
			out[k] = deepMerge(bm, om)
			continue
		}
		out[k] = v
	}
	return out
}

func funcs(root *template.Template, st *renderState) template.FuncMap {
	toYAML := func(v any) string {
		b, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(b), "\n")
	}
	indent := func(n int, s string) (string, error) {
		if n < 0 || n > maxIndent {
			return "", fmt.Errorf("indent %d is outside 0..%d", n, maxIndent)
		}
		// Every line gains n bytes: bound the result before building it.
		if (strings.Count(s, "\n")+1)*n+len(s) > maxStringBytes {
			return "", fmt.Errorf("indent would produce a string over %d bytes", maxStringBytes)
		}
		pad := strings.Repeat(" ", n)
		return pad + strings.ReplaceAll(s, "\n", "\n"+pad), nil
	}
	truthy := func(v any) bool {
		switch x := v.(type) {
		case nil:
			return false
		case string:
			return x != ""
		case bool:
			return x
		case int:
			return x != 0
		case int64:
			return x != 0
		case float64:
			return x != 0
		case []any:
			return len(x) > 0
		case map[string]any:
			return len(x) > 0
		}
		return true
	}
	return template.FuncMap{
		"toYaml": toYAML,
		"toJson": func(v any) string { b, _ := json.Marshal(v); return string(b) },
		"indent": indent,
		"nindent": func(n int, s string) (string, error) {
			r, err := indent(n, s)
			return "\n" + r, err
		},
		"quote":  func(v any) string { return fmt.Sprintf("%q", fmt.Sprint(v)) },
		"squote": func(v any) string { return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'" },
		"default": func(def, v any) any {
			if truthy(v) {
				return v
			}
			return def
		},
		"empty": func(v any) bool { return !truthy(v) },
		"required": func(msg string, v any) (any, error) {
			if !truthy(v) {
				return nil, errors.New(msg)
			}
			return v, nil
		},
		"fail":       func(msg string) (string, error) { return "", errors.New(msg) },
		"trim":       strings.TrimSpace,
		"trimSuffix": func(suf, s string) string { return strings.TrimSuffix(s, suf) },
		"trimPrefix": func(pre, s string) string { return strings.TrimPrefix(s, pre) },
		"trunc": func(n int, s string) (string, error) {
			if n < 0 {
				return "", errors.New("trunc: negative length")
			}
			if n < len(s) {
				return s[:n], nil
			}
			return s, nil
		},
		"lower": strings.ToLower,
		"upper": strings.ToUpper,
		"replace": func(old, new, s string) (string, error) {
			if old != "" && len(new) > len(old) && strings.Count(s, old)*(len(new)-len(old))+len(s) > maxStringBytes {
				return "", fmt.Errorf("replace would produce a string over %d bytes", maxStringBytes)
			}
			return strings.ReplaceAll(s, old, new), nil
		},
		"contains":  func(sub, s string) bool { return strings.Contains(s, sub) },
		"hasPrefix": func(pre, s string) bool { return strings.HasPrefix(s, pre) },
		"hasSuffix": func(suf, s string) bool { return strings.HasSuffix(s, suf) },
		"b64enc":    func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
		"toString":  func(v any) string { return fmt.Sprint(v) },
		"list":      func(v ...any) []any { return v },
		"join": func(sep string, v []any) string {
			parts := make([]string, len(v))
			for i, x := range v {
				parts[i] = fmt.Sprint(x)
			}
			return strings.Join(parts, sep)
		},
		"hasKey": func(m map[string]any, k string) bool { _, ok := m[k]; return ok },
		"ternary": func(a, b any, cond bool) any {
			if cond {
				return a
			}
			return b
		},
		"include": func(name string, data any) (string, error) {
			if root == nil || st == nil {
				return "", errors.New("include is not available here")
			}
			// Recursion through include restarts the template engine's own depth counter, and
			// a Go stack overflow is fatal, so the depth is capped here.
			if st.depth >= maxIncludeDepth {
				return "", fmt.Errorf("include nested deeper than %d", maxIncludeDepth)
			}
			if time.Now().After(st.deadline) {
				return "", fmt.Errorf("rendering took longer than %s", renderTimeout)
			}
			st.depth++
			defer func() { st.depth-- }()
			w := &limitWriter{max: maxStringBytes}
			if err := root.ExecuteTemplate(w, name, data); err != nil {
				return "", err
			}
			return w.buf.String(), nil
		},
	}
}

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

	"go.yaml.in/yaml/v3"
)

// ReleaseInfo is the .Release object of a chart.
type ReleaseInfo struct {
	Name, Namespace string
	Chart, Version  string
}

// RenderChart renders chartDir/templates with a Helm-compatible subset of the template language and
// returns the objects. Supported: .Values, .Release.{Name,Namespace}, .Chart.{Name,Version},
// named templates (define/include/template from *.tpl and *.yaml), and the functions in funcs below.
// Not supported (a chart using them fails with an "undefined function" error): lookup, tpl,
// subcharts, hooks, .Capabilities, .Files. Why not the real Helm engine: research R16.
func RenderChart(chartDir string, overrides map[string]any, rel ReleaseInfo) ([]map[string]any, error) {
	tdir := filepath.Join(chartDir, "templates")
	ents, err := os.ReadDir(tdir)
	if err != nil {
		return nil, fmt.Errorf("chart has no templates directory: %w", err)
	}
	values := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(chartDir, "values.yaml")); err == nil { //nolint:gosec // module chart path
		if err := yaml.Unmarshal(b, &values); err != nil {
			return nil, fmt.Errorf("values.yaml: %w", err)
		}
	}
	merged := deepMerge(values, overrides)
	if rel.Chart == "" {
		rel.Chart, rel.Version = chartMeta(chartDir)
	}
	root := template.New("chart").Option("missingkey=zero")
	root.Funcs(funcs(root))
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !(strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".tpl")) {
			continue
		}
		info, err := os.Lstat(filepath.Join(tdir, n))
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("template %s is a symlink; refused", n)
		}
		b, err := os.ReadFile(filepath.Join(tdir, n)) //nolint:gosec // module chart path
		if err != nil {
			return nil, err
		}
		if _, err := root.New(n).Parse(string(b)); err != nil {
			return nil, fmt.Errorf("template %s: %w", n, err)
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
	var out []map[string]any
	for _, n := range names {
		var buf bytes.Buffer
		if err := root.ExecuteTemplate(&buf, n, data); err != nil {
			return nil, fmt.Errorf("rendering %s: %w", n, err)
		}
		text := strings.ReplaceAll(buf.String(), "<no value>", "")
		dec := yaml.NewDecoder(strings.NewReader(text))
		for {
			var doc map[string]any
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s is not valid YAML after rendering: %w", n, err)
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

func chartMeta(dir string) (name, version string) {
	b, err := os.ReadFile(filepath.Join(dir, "Chart.yaml")) //nolint:gosec // module chart path
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

func funcs(root *template.Template) template.FuncMap {
	toYAML := func(v any) string {
		b, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(b), "\n")
	}
	indent := func(n int, s string) string {
		pad := strings.Repeat(" ", n)
		return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
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
		"nindent": func(n int, s string) string {
			return "\n" + indent(n, s)
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
		"trunc": func(n int, s string) string {
			if n < len(s) {
				return s[:n]
			}
			return s
		},
		"lower":     strings.ToLower,
		"upper":     strings.ToUpper,
		"replace":   func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
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
			if root == nil {
				return "", errors.New("include is not available here")
			}
			var buf bytes.Buffer
			if err := root.ExecuteTemplate(&buf, name, data); err != nil {
				return "", err
			}
			return buf.String(), nil
		},
	}
}

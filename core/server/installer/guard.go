package installer

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// maxValueNodes bounds how many nodes a serialising helper will walk. Template lists are shared by
// pointer, so `$l = list $l $l` repeated 40 times is a cheap DAG that expands to 2^40 nodes when it is
// written out. The real defence is the child process (child.go); this makes the common helpers fail
// with an error instead of reaching it.
const maxValueNodes = 100000

// checkValue walks v counting every node it would serialise (shared children are counted each time
// they are reached, which is what serialising does) and fails past maxValueNodes.
func checkValue(v any) error {
	n := 0
	var walk func(any) bool
	walk = func(x any) bool {
		if n++; n > maxValueNodes {
			return false
		}
		switch t := x.(type) {
		case []any:
			for _, e := range t {
				if !walk(e) {
					return false
				}
			}
		case map[string]any:
			for _, e := range t {
				if !walk(e) {
					return false
				}
			}
		}
		return true
	}
	if !walk(v) {
		return fmt.Errorf("value is too large to serialise (over %d nodes)", maxValueNodes)
	}
	return nil
}

func checkAll(vs ...any) error {
	for _, v := range vs {
		if err := checkValue(v); err != nil {
			return err
		}
	}
	return nil
}

// guardSerialisers replaces the helpers that turn values into text with versions that refuse
// oversized values first, and overrides the builtin print family the same way.
func guardSerialisers(fm template.FuncMap, toYAML func(any) string) {
	fm["toYaml"] = func(v any) (string, error) {
		if err := checkValue(v); err != nil {
			return "", err
		}
		return toYAML(v), nil
	}
	fm["toJson"] = func(v any) (string, error) {
		if err := checkValue(v); err != nil {
			return "", err
		}
		b, _ := json.Marshal(v)
		return string(b), nil
	}
	fm["quote"] = func(v any) (string, error) {
		if err := checkValue(v); err != nil {
			return "", err
		}
		return fmt.Sprintf("%q", fmt.Sprint(v)), nil
	}
	fm["squote"] = func(v any) (string, error) {
		if err := checkValue(v); err != nil {
			return "", err
		}
		return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'", nil
	}
	fm["toString"] = func(v any) (string, error) {
		if err := checkValue(v); err != nil {
			return "", err
		}
		return fmt.Sprint(v), nil
	}
	fm["join"] = func(sep string, v []any) (string, error) {
		if err := checkValue(v); err != nil {
			return "", err
		}
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = fmt.Sprint(x)
		}
		return capStr(strings.Join(parts, sep))
	}
	fm["print"] = func(a ...any) (string, error) {
		if err := checkAll(a...); err != nil {
			return "", err
		}
		return capStr(fmt.Sprint(a...))
	}
	fm["println"] = func(a ...any) (string, error) {
		if err := checkAll(a...); err != nil {
			return "", err
		}
		return capStr(fmt.Sprintln(a...))
	}
	fm["printf"] = func(format string, a ...any) (string, error) {
		if err := checkAll(a...); err != nil {
			return "", err
		}
		return capStr(fmt.Sprintf(format, a...))
	}
}

func capStr(s string) (string, error) {
	if len(s) > maxStringBytes {
		return "", fmt.Errorf("template function produced a string over %d bytes", maxStringBytes)
	}
	return s, nil
}

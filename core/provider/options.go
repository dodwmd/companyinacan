package provider

import (
	"fmt"
	"strings"
)

// opts is a typed reader over company.yaml provider.options.
type opts map[string]any

func (o opts) str(key, def string) string {
	if s, ok := o[key].(string); ok && s != "" {
		return s
	}
	return def
}

func (o opts) integer(key string, def int) int {
	switch v := o[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

// ResolveSecret resolves a secret reference: "env:NAME" or "file:/path".
// The result is registered with the redactor so it never reaches output.
func (d *Deps) ResolveSecret(ref string) (string, error) {
	var val string
	switch {
	case strings.HasPrefix(ref, "env:"):
		val = d.Getenv(strings.TrimPrefix(ref, "env:"))
		if val == "" {
			return "", fmt.Errorf("secret reference %q: environment variable is empty or unset", ref)
		}
	case strings.HasPrefix(ref, "file:"):
		b, err := d.ReadFile(strings.TrimPrefix(ref, "file:"))
		if err != nil {
			return "", fmt.Errorf("secret reference %q: cannot read file", ref)
		}
		val = strings.TrimSpace(string(b))
		if val == "" {
			return "", fmt.Errorf("secret reference %q: file is empty", ref)
		}
	default:
		return "", fmt.Errorf("secret reference %q: want env:NAME or file:PATH", ref)
	}
	d.Redactor.Add(val)
	return val, nil
}

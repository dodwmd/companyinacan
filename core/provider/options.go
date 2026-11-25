package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// expandHome expands a leading "~/" (or a bare "~") to the user's home directory.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// opts is a typed reader over company.yaml provider.options.
type opts map[string]any

func (o opts) str(key, def string) string {
	if s, ok := o[key].(string); ok && s != "" {
		return s
	}
	return def
}

// integerE is integer that rejects a present value that is not a whole number
// (a quoted "22", 22.5, a list), instead of silently using the default.
func (o opts) integerE(key string, def int) (int, error) {
	v, ok := o[key]
	if !ok || v == nil {
		return def, nil
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if n == float64(int(n)) {
			return int(n), nil
		}
	}
	return 0, fmt.Errorf("provider.options.%s must be a whole number, got %v", key, v)
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
			return "", fmt.Errorf("secret reference %q is not usable (the environment variable is empty or unset)", ref)
		}
	case strings.HasPrefix(ref, "file:"):
		b, err := d.ReadFile(expandHome(strings.TrimPrefix(ref, "file:")))
		if err != nil {
			return "", fmt.Errorf("secret reference %q is not usable (cannot read the file)", ref)
		}
		val = strings.TrimSpace(string(b))
		if val == "" {
			return "", fmt.Errorf("secret reference %q is not usable (the file is empty)", ref)
		}
	default:
		// The value is not echoed: a literal secret pasted here must not reach output.
		return "", fmt.Errorf("secret reference is not valid: want env:NAME or file:PATH (a secret value must never be written in company.yaml)")
	}
	d.Redactor.Add(val)
	return val, nil
}

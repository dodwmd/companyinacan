package provider

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
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

// operatorIPString returns operator_ip, or an error if it is set but not a string
// (an unquoted number would otherwise be ignored and leave SSH open to the world).
func operatorIPString(o map[string]any) (string, error) {
	v, ok := o["operator_ip"]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("provider.options.operator_ip must be a string (an IP address or CIDR), got %v", v)
	}
	return s, nil
}

// CheckOperatorIP validates provider.options.operator_ip: an IP or CIDR. A
// match-everything value (0.0.0.0/0, ::/0) is refused: leave the option unset instead.
func CheckOperatorIP(s string) error {
	if s == "" {
		return nil
	}
	var pfx netip.Prefix
	if p, err := netip.ParsePrefix(s); err == nil {
		pfx = p
	} else if a, err := netip.ParseAddr(s); err == nil {
		pfx = netip.PrefixFrom(a, a.BitLen())
	} else {
		return fmt.Errorf("provider.options.operator_ip %q is not an IP address or CIDR", s)
	}
	if pfx.Bits() == 0 {
		return fmt.Errorf("provider.options.operator_ip %q matches every address; leave operator_ip unset instead (SSH is then open to the world)", s)
	}
	return nil
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

var reEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// ValidSecretRef reports whether ref is env:NAME (NAME upper case, as environment
// variables conventionally are, which also keeps a pasted token such as
// env:dop_v1_... from being accepted and echoed) or file:PATH.
func ValidSecretRef(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "env:"):
		return reEnvName.MatchString(strings.TrimPrefix(ref, "env:"))
	case strings.HasPrefix(ref, "file:"):
		p := strings.TrimPrefix(ref, "file:")
		if p == "" || len(p) > 4096 || strings.TrimSpace(p) != p {
			return false
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
		return true
	}
	return false
}

// ResolveSecret resolves a secret reference: "env:NAME" or "file:/path".
// The result is registered with the redactor so it never reaches output.
func (d *Deps) ResolveSecret(ref string) (string, error) {
	var val string
	if !ValidSecretRef(ref) {
		// The value is not echoed: a literal secret pasted here must not reach output.
		return "", fmt.Errorf("secret reference is not valid: want env:NAME (upper case) or file:PATH (a secret value must never be written in company.yaml)")
	}
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

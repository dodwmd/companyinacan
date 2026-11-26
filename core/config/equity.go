package config

// The equity settings of company.yaml that can-core acts on (modules.equity.settings,
// specs/057-module-capability-wiring).

import (
	"fmt"
	"time"
)

// Identity modes of equity (specs/051-holder-identity).
const (
	IdentityAttestation = "attestation" // the default
	IdentityDirectory   = "directory"   // opt-in: callers and holders are checked against the people directory
)

// Settings keys of modules.equity.settings.
const (
	SettingIdentity   = "identity"
	SettingBreakGlass = "break_glass_until"
)

// maxBreakGlass is how far ahead a break-glass window may end: it is for one task, not a mode.
const maxBreakGlass = 7 * 24 * time.Hour

// EquitySettings are the settings of equity that can-core acts on.
type EquitySettings struct {
	Identity string // never empty
	// BreakGlassUntil is the end of the break-glass window as can-equity takes it, "" for none.
	BreakGlassUntil string
}

// ReadEquitySettings reads them. A value that is not understood is an error, never the default:
// a mistyped identity must not leave the weaker mode in place unnoticed. Whether the break-glass
// time is acceptable is ParseBreakGlass's question.
func ReadEquitySettings(settings map[string]any) (EquitySettings, error) {
	out := EquitySettings{Identity: IdentityAttestation}
	switch v := settings[SettingIdentity]; v {
	case nil, "", IdentityAttestation:
	case IdentityDirectory:
		out.Identity = IdentityDirectory
	default:
		return out, fmt.Errorf("%s must be %s or %s (got %v)", SettingIdentity, IdentityAttestation, IdentityDirectory, v)
	}
	switch v := settings[SettingBreakGlass].(type) {
	case nil:
	case string:
		out.BreakGlassUntil = v
	case time.Time: // YAML reads an unquoted date as a time
		if v.Equal(v.Truncate(24 * time.Hour)) {
			out.BreakGlassUntil = v.UTC().Format("2006-01-02")
		} else {
			out.BreakGlassUntil = v.UTC().Format(time.RFC3339)
		}
	default:
		return out, fmt.Errorf("%s must be a date (YYYY-MM-DD) or an RFC 3339 time", SettingBreakGlass)
	}
	return out, nil
}

// ParseBreakGlass returns the end of a break-glass window written as YYYY-MM-DD (inclusive, UTC) or
// RFC 3339, "" meaning none. name is what the value is called in the error. A past time is fine
// (the window is over); one more than 7 days ahead is refused.
func ParseBreakGlass(name, v string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		if t, err = time.Parse("2006-01-02", v); err != nil {
			return time.Time{}, fmt.Errorf("%s must be YYYY-MM-DD or an RFC 3339 time", name)
		}
		t = t.AddDate(0, 0, 1)
	}
	if t.After(now.Add(maxBreakGlass)) {
		return time.Time{}, fmt.Errorf("%s must end within 7 days from now (a date counts to its end, so at most 6 days from today): break-glass is for one task, not a mode", name)
	}
	return t, nil
}

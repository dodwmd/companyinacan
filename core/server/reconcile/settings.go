package reconcile

// What of company.yaml's settings and company facts reaches a module (specs/064-module-settings):
// part of what it is installed from, and, where it widens what the module does, of what an upgrade
// waits for approval for.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modsettings"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// values are the chart values module id is given from company.yaml, by dotted path.
func (d Desired) values(id string) map[string]any {
	s := d.Schemas[id]
	if s == nil {
		return nil
	}
	r := s.Map(d.Company.Modules[id].Settings, modsettings.FactsOf(d.Company.Name, d.Company.Organisation))
	if len(r.Values) == 0 {
		return nil
	}
	return r.Values
}

// valuesHash adds them to a spec hash, so that a changed fact, or a release that maps a setting
// differently, is an upgrade.
func (d Desired) valuesHash(id, hash string) string {
	v := d.values(id)
	if v == nil {
		return hash
	}
	b, _ := json.Marshal(v) // sorted keys
	sum := sha256.Sum256(append([]byte(hash+"\x00values\x00"), b...))
	return hex.EncodeToString(sum[:])
}

// settingWidening is what the new values let the module do that what it was installed with did not.
// A record without values (nothing was mapped, or it is older than the mapping) ran with the
// module's defaults, so a setting that was written and ignored until now counts when it widens.
func (d Desired) settingWidening(id string, recorded store.Module) []string {
	schema := d.Schemas[id]
	values := d.values(id)
	was := recordedValues(schema, ParseApplied(recorded.Applied), d.Company.Modules[id].Settings, values)
	var out []string
	for _, c := range schema.Widening(was, values) {
		out = append(out, c.String())
	}
	return out
}

// restored is the approval a restore records (restoreops.Complete): the apply that starts the
// modules again waits for none, and for that reason applies no setting that widens.
const restored = "*"

// ParseApplied reads store.Module.Applied; anything unreadable is the zero value, under which the
// module ran with its defaults.
func ParseApplied(s string) Applied {
	var a Applied
	if json.Unmarshal([]byte(s), &a) != nil {
		return Applied{}
	}
	a.Values = modsettings.Normalise(a.Values)
	return a
}

// recordedValues are the mapped values a module installed as was ran with. A release before the
// mapping gave it a few settings all the same: those count while the settings are unchanged.
func recordedValues(schema *modsettings.Schema, was Applied, settings, values map[string]any) map[string]any {
	if !was.Mapped && was.Settings == settingsDigest(settings) {
		return schema.PreviouslyApplied(values)
	}
	return modsettings.Normalise(was.Values)
}

func settingsDigest(settings map[string]any) string {
	b, _ := json.Marshal(settings) // sorted keys
	return digest(b)
}

// WithinValues is values with every setting that widens what was had left as it was.
func WithinValues(schema *modsettings.Schema, was Applied, settings, values map[string]any) map[string]any {
	if schema == nil {
		return values
	}
	out := schema.Within(recordedValues(schema, was, settings, values), values)
	if len(out) == 0 {
		return nil
	}
	return out
}

// WithinBreakGlass is the break-glass window to install when company.yaml asks for until: a new or
// longer one is not what was.
func WithinBreakGlass(was Applied, until string) string {
	if until == "" || until == was.BreakGlass || (was.BreakGlass != "" && windowEnd(until).Before(windowEnd(was.BreakGlass))) {
		return until
	}
	return was.BreakGlass
}

// keepOptIns adds the connections the company had opted into and now turns off, while the provider
// is still enabled: leaving such a mode stops a check, which waits for approval.
func keepOptIns(d Desired, recorded store.Module, wanted []config.Link) ([]config.Link, bool) {
	kept := false
	for _, l := range recorded.Manifest.Runtime.Wired {
		g, ok := modsecrets.GrantFor(recorded.ID, l.Provider)
		if _, on := d.enabled()[l.Provider]; ok && on && g.OptInKey != "" && !linksTo(wanted, l.Provider) {
			wanted, kept = append(wanted, l), true
		}
	}
	return wanted, kept
}

// keptAfterRestore: a line of linkWidening about something the apply after a restore keeps back.
func keptAfterRestore(line string) bool {
	return strings.HasPrefix(line, "equity break-glass") || strings.HasPrefix(line, "disconnects from")
}

func sameValues(a, b map[string]any) bool {
	x, _ := json.Marshal(modsettings.Normalise(a))
	y, _ := json.Marshal(modsettings.Normalise(b))
	return string(x) == string(y) || (len(a) == 0 && len(b) == 0)
}

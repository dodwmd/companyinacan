package reconcile

// What of company.yaml's settings and company facts reaches a module (specs/064-module-settings):
// part of what it is installed from, and, where it widens what the module does, of what an upgrade
// waits for approval for.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/dodwmd/companyinacan/core/modsettings"
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
	var was Applied
	_ = json.Unmarshal([]byte(recorded.Applied), &was)
	var out []string
	for _, c := range d.Schemas[id].Widening(modsettings.Normalise(was.Values), d.values(id)) {
		out = append(out, c.String())
	}
	return out
}

func sameValues(a, b map[string]any) bool {
	x, _ := json.Marshal(modsettings.Normalise(a))
	y, _ := json.Marshal(modsettings.Normalise(b))
	return string(x) == string(y) || (len(a) == 0 && len(b) == 0)
}

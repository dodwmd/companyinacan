package reconcile_test

import (
	"os"
	"testing"

	"github.com/dodwmd/companyinacan/core/modsettings"
)

// Naming a form for an installed module is a new flow of personal data: the upgrade that gives the
// module the setting is held. Taking it out again is not.
func TestNamingALeadFormWaitsForApproval(t *testing.T) {
	for id, key := range map[string]string{"support": "tickets_from_forms", "newsletter": "subscribe_from_forms"} {
		schema, err := modsettings.LoadModule(os.DirFS("../../../modules/"+id), "./settings.schema.json")
		if err != nil {
			t.Fatal(err)
		}
		on := schema.Map(map[string]any{key: []any{"contact"}}, modsettings.Facts{}).Values
		off := schema.Map(map[string]any{key: []any{}}, modsettings.Facts{}).Values
		if got := schema.Widening(nil, on); len(got) != 1 || got[0].Setting != key {
			t.Errorf("%s: adding a form widens %v", id, got)
		}
		if got := schema.Widening(on, off); len(got) != 0 {
			t.Errorf("%s: removing a form widens %v", id, got)
		}
	}
}

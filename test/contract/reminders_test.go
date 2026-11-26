package contract

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/notify"
)

// The reminders of specs/060-reminders: who each event reaches, as the shipped manifests say it. The
// timesheet, expense and time off events have no rule since specs/070-approvals-inbox: can-core's
// approval engine tells the approvers and the requester, and a rule would tell them twice.
var reminderRules = map[string]string{
	"finance invoice.overdue.v1":     "role:finance",
	"finance contract.expiring.v1":   "role:finance",
	"finance contract.signed.v1":     "role:finance",
	"finance contract.declined.v1":   "role:finance",
	"jobs application.received.v1":   "field:hiring_manager role:recruiter",
	"work work.issue.p0.v1":          "field:assignee role:manager",
	"entity entity.filing.due.v1":    "role:secretary",
}

// Every rule names an event its module publishes, a role it defines and a data field of that event,
// and its notification has the project's own wording and a preference in the admin UI.
func TestReminderRulesAreComplete(t *testing.T) {
	prefs, err := os.ReadFile(filepath.Join(root, "core", "server", "adminui", "platform.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, id := range []string{"finance", "jobs", "work", "people", "entity"} {
		m := readYAML(t, "modules/"+id+"/module.yaml")
		published := map[string]bool{}
		for _, p := range asList(m["publishes"]) {
			published[asStr(asMap(p)["event"])] = true
		}
		for _, n := range asList(m["notifications"]) {
			event := asStr(asMap(n)["event"])
			if !published[event] {
				t.Errorf("%s: %s is not published", id, event)
			}
			var to []string
			for _, r := range asList(asMap(n)["recipients"]) {
				rcp := asStr(r)
				to = append(to, rcp)
				kind, val, _ := strings.Cut(rcp, ":")
				switch kind {
				case "role":
					if asMap(m["roles"])[val] == nil {
						t.Errorf("%s: %s names the undefined role %s", id, event, val)
					}
				case "field":
					raw, err := os.ReadFile(filepath.Join(root, "schemas", "events", event+".json"))
					if err != nil || !strings.Contains(string(raw), `"`+val+`"`) {
						t.Errorf("%s: %s has no data field %s", id, event, val)
					}
				}
			}
			got[id+" "+event] = strings.Join(to, " ")

			typ := event[:strings.LastIndex(event, ".")]
			if r := notify.Render(typ, map[string]string{}); strings.HasPrefix(r.Subject, "Notification:") {
				t.Errorf("%s has no wording of its own", typ)
			}
			cut := strings.LastIndex(typ, ".")
			if !strings.Contains(string(prefs), `"`+typ[:cut]+`" + "`+typ[cut:]+`"`) {
				t.Errorf("%s has no preference row", typ)
			}
		}
	}
	var keys []string
	for k := range reminderRules {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if got[k] != reminderRules[k] {
			t.Errorf("%s: recipients %q, want %q", k, got[k], reminderRules[k])
		}
	}
	if len(got) != len(reminderRules) {
		t.Errorf("rules %v", got)
	}
}

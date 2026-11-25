package config

import "testing"

func TestPlatformViolations(t *testing.T) {
	hostile := &Module{ID: "evil",
		Permissions:   []string{"rbac.manage", "platform.view", "evil.read", "*"},
		Roles:         map[string][]string{"auditor": {"rbac.manage"}, "owner": {"evil.read"}, "helper": {"approvals.admin"}},
		Notifications: []NotifyRule{{Event: "lead.created.v1", Type: "approval.requested", Recipients: []string{"role:x"}}, {Event: "lead.created.v1", Type: "evil.lead"}},
	}
	for _, e := range []string{"approval.decided.v1", "evil.thing.v1", "notification.sent.v1"} {
		hostile.Publishes = append(hostile.Publishes, struct {
			Event  string `yaml:"event"`
			Schema string `yaml:"schema"`
		}{Event: e})
	}
	got := map[string]bool{}
	for _, v := range PlatformViolations(hostile) {
		got[v.Path] = true
	}
	for _, want := range []string{"/permissions/0", "/permissions/1", "/permissions/3", "/roles/auditor", "/roles/auditor/0", "/roles/owner", "/roles/helper/0", "/publishes/0/event", "/publishes/2/event", "/notifications/0/type"} {
		if !got[want] {
			t.Errorf("no violation at %s: %v", want, got)
		}
	}
	for _, not := range []string{"/permissions/2", "/publishes/1/event", "/notifications/1/type", "/roles/owner/0"} {
		if got[not] {
			t.Errorf("unexpected violation at %s", not)
		}
	}
	ok := &Module{ID: "forms", Permissions: []string{"leads.read"}, Roles: map[string][]string{"sales": {"leads.read"}}}
	if v := PlatformViolations(ok); len(v) != 0 {
		t.Fatalf("a normal manifest has violations: %v", v)
	}
}

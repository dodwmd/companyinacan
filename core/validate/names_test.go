package validate

import "testing"

func TestCapabilitiesLoad(t *testing.T) {
	c := Capabilities()
	for _, name := range []string{"contacts", "tickets", "email.send", "approvals", "reports", "portal.view"} {
		if _, ok := c[name]; !ok {
			t.Errorf("capability %q missing from names.yaml", name)
		}
	}
	if !c["reports"].Aggregate || !c["portal.view"].Aggregate || c["contacts"].Aggregate {
		t.Error("aggregate flags wrong")
	}
	if !c["approvals"].Platform || c["contacts"].Platform {
		t.Error("platform flags wrong")
	}
}

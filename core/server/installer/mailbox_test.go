package installer

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// The mailbox of company.yaml reaches the support module: settings as environment, credentials in
// can-imap only, and a change of the password restarts the pod.
func TestSupportMailboxReachesTheModule(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir, in.cfg.Domain, in.cfg.DB = "../../../modules", "acme.example", dbprov.NewFake()
	account := &IMAPAccount{Host: "imap.example.net", Port: 993, Username: "u", Password: "imap-pass-one", Encryption: "ssl", Folder: "INBOX", VerifyTLS: true}
	in.cfg.IMAP = func(reconcile.Spec) (*IMAPAccount, error) { return account, nil }
	spec.ID, spec.Namespace, spec.ServiceAccount, spec.Domain = "support", "mod-support", "support", "acme.example"
	spec.Manifest = loadManifest(t, "../../../modules/support/module.yaml")
	spec.Settings = map[string]any{"support_address": "help@acme.example", "mailbox_name": "Acme Help",
		"inbound": map[string]any{"imap": map[string]any{"host": "imap.example.net"}}}
	enable := func() string {
		t.Helper()
		if err := in.Enable(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		return toJSON(api.Objects["Deployment/mod-support/support"]["spec"])
	}
	first := enable()
	for _, want := range []string{`"CAN_SUPPORT_ADDRESS","value":"help@acme.example"`, `"CAN_SUPPORT_NAME","value":"Acme Help"`, `"CAN_IMAP_WANTED","value":"1"`, "can-support.php"} {
		if !strings.Contains(strings.ReplaceAll(first, `"name":`, ""), want) {
			t.Errorf("the deployment lacks %s", want)
		}
	}
	if strings.Contains(first, "imap-pass-one") {
		t.Error("the password is in the pod template")
	}
	got := decoded(t, api, "mod-support", "can-imap")
	if got["password"] != "imap-pass-one" || got["host"] != "imap.example.net" || got["port"] != "993" || got["verify"] != "true" || got["user"] != "u" {
		t.Errorf("can-imap: %v keys", len(got))
	}
	account.Password = "imap-pass-two"
	if enable() == first {
		t.Error("a new password does not restart the pod")
	}
	account = nil
	spec.Settings = map[string]any{}
	after := enable()
	if got := decoded(t, api, "mod-support", "can-imap"); len(got) != 0 {
		t.Errorf("can-imap keeps %d withdrawn keys", len(got))
	}
	if strings.Contains(after, `"name":"CAN_IMAP_WANTED"`) {
		t.Error("inbound mail is still asked for")
	}
}

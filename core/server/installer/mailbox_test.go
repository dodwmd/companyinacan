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
	for _, want := range []string{`"CAN_SUPPORT_ADDRESS","value":"help@acme.example"`, `"CAN_SUPPORT_NAME","value":"Acme Help"`, `"CAN_IMAP_WANTED","value":"1"`, "can-support.php", "verify_peer", "can-mail-upstream.php"} {
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

// Settings are templated into environment values, where Kubernetes expands $(VAR): a mailbox name
// must not become the database password. A value can-core refuses reaches nothing.
func TestSupportSettingsAreNotExpandedOrAppliedWhenInvalid(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir, in.cfg.Domain, in.cfg.DB = "../../../modules", "acme.example", dbprov.NewFake()
	spec.ID, spec.Namespace, spec.ServiceAccount, spec.Domain = "support", "mod-support", "support", "acme.example"
	spec.Manifest = loadManifest(t, "../../../modules/support/module.yaml")
	env := func(settings map[string]any) map[string]string {
		t.Helper()
		spec.Settings = settings
		if err := in.Enable(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		ps := podSpec(api.Objects["Deployment/mod-support/support"])
		for _, e := range asList(asMap(asList(ps["containers"])[0])["env"]) {
			n, _ := asMap(e)["name"].(string)
			v, _ := asMap(e)["value"].(string)
			out[n] = v
		}
		return out
	}
	got := env(map[string]any{"support_address": "help$x@acme.example", "mailbox_name": "$(DB_PASS)", "admin_email": "$(ADMIN_PASS)@acme.example"})
	for k, want := range map[string]string{"CAN_SUPPORT_NAME": "$$(DB_PASS)", "CAN_SUPPORT_ADDRESS": "help$$x@acme.example", "ADMIN_EMAIL": "$$(ADMIN_PASS)@acme.example"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	got = env(map[string]any{"support_address": "help@acme.example", "inbound": map[string]any{"imap": map[string]any{"host": "h.example", "encryption": "none"}}})
	if got["CAN_SUPPORT_ADDRESS"] != "" || got["CAN_SUPPORT_INVALID"] == "" || got["CAN_IMAP_WANTED"] != "" {
		t.Errorf("an invalid setting: address %q, invalid %q, wanted %q", got["CAN_SUPPORT_ADDRESS"], got["CAN_SUPPORT_INVALID"], got["CAN_IMAP_WANTED"])
	}
	// Anything of the wrong type renders (these keys were once ignored) and is not applied.
	got = env(map[string]any{"support_address": []any{"a@b.example"}, "inbound": "imap"})
	if got["CAN_SUPPORT_ADDRESS"] != "" || got["CAN_SUPPORT_INVALID"] == "" {
		t.Errorf("wrong types: %q %q", got["CAN_SUPPORT_ADDRESS"], got["CAN_SUPPORT_INVALID"])
	}
}

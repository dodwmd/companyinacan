package installer

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

func secretFixture(t *testing.T) (func(...string) error, func() map[string]any, *bytes.Buffer) {
	t.Helper()
	enable, data, logs, _ := secretFixtureAPI(t)
	return enable, data, logs
}

func secretFixtureAPI(t *testing.T) (func(...string) error, func() map[string]any, *bytes.Buffer, *kube.Fake) {
	t.Helper()
	in, api, _, spec := fixture(t, nil)
	logs := &bytes.Buffer{}
	in.cfg.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	in.cfg.SecretsFor = func(id string) []modsecrets.Spec {
		return []modsecrets.Spec{{Key: "CAN_STUB_TOKEN"}, {Key: "CAN_STUB_HASH", NoRotate: true}, {Key: modsecrets.KeyringKey, Kind: modsecrets.Keyring}}
	}
	enable := func(rot ...string) error {
		s := spec
		s.Rotate = rot
		return in.Enable(context.Background(), s)
	}
	return enable, func() map[string]any { return secretData(get(t, api, "Secret", "mod-stub", "can-module")) }, logs, api
}

func TestGeneratedSecretsAreCreatedOnceAndKept(t *testing.T) {
	enable, data, _ := secretFixture(t)
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	first := data()
	for _, k := range []string{"CAN_STUB_TOKEN", "CAN_STUB_HASH", "keyring", "CAN_API_URL", "CAN_WEBHOOK_SECRET"} {
		if first[k] == nil || first[k] == "" {
			t.Fatalf("%s missing: %v", k, keyNames(first))
		}
	}
	if len(first["CAN_STUB_TOKEN"].(string)) < 32 {
		t.Fatal("generated token is short")
	}
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	second := data()
	for _, k := range []string{"CAN_STUB_TOKEN", "CAN_STUB_HASH", "keyring"} {
		if first[k] != second[k] {
			t.Errorf("%s changed on a second enable: secrets are generated once", k)
		}
	}
}

func TestRotationReplacesOnlyTheRequestedKey(t *testing.T) {
	enable, data, _ := secretFixture(t)
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	before := data()
	if err := enable("CAN_STUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
	after := data()
	if before["CAN_STUB_TOKEN"] == after["CAN_STUB_TOKEN"] {
		t.Fatal("token not rotated")
	}
	if before["keyring"] != after["keyring"] || before["CAN_STUB_HASH"] != after["CAN_STUB_HASH"] {
		t.Fatal("rotation touched other keys")
	}
}

func TestKeyringRotationKeepsOldKeysReadable(t *testing.T) {
	enable, data, _ := secretFixture(t)
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	before := data()["keyring"].(string)
	if err := enable("keyring"); err != nil {
		t.Fatal(err)
	}
	after := data()["keyring"].(string)
	if !strings.HasPrefix(after, "k2 ") || !strings.Contains(after, strings.TrimSpace(before)) {
		t.Fatalf("keyring after rotation:\n%s", after)
	}
}

func TestNonRotatableKeyIsRefusedBeforeAnythingChanges(t *testing.T) {
	enable, data, _ := secretFixture(t)
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	before := data()
	if err := enable("CAN_STUB_HASH"); err == nil {
		t.Fatal("rotating a key marked NoRotate must fail")
	}
	if err := enable("CAN_NOT_OURS"); err == nil {
		t.Fatal("rotating a key the module has no generated secret for must fail")
	}
	after := data()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed by a refused rotation", k)
		}
	}
}

func TestWebhookSecretRotationUsesTheNewSpecValue(t *testing.T) {
	enable, data, _ := secretFixture(t)
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	// the reconciler hands the installer the new HMAC secret in Spec; the installer must accept the
	// rotation request for it and write what it is given
	if err := enable(modsecrets.WebhookKey); err != nil {
		t.Fatal(err)
	}
	if data()["CAN_WEBHOOK_SECRET"] != "s3cret" {
		t.Fatalf("%v", data()["CAN_WEBHOOK_SECRET"])
	}
}

func TestSecretValuesNeverReachTheLogs(t *testing.T) {
	enable, data, logs := secretFixture(t)
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	if err := enable("CAN_STUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
	for k, v := range data() {
		if k == "CAN_API_URL" || k == "CAN_MODULE_ID" || k == "CAN_TOKEN_FILE" || k == "CAN_TOKEN_AUDIENCE" {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(v.(string)), "\n") {
			if _, hexpart, ok := strings.Cut(line, " "); ok {
				line = hexpart
			}
			if len(line) >= 8 && strings.Contains(logs.String(), line) {
				t.Errorf("the value of %s appears in the logs", k)
			}
		}
	}
}

func keyNames(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

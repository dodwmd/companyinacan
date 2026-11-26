package installer

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

// Rotating the webhook secret reads the release record written under the current secret, and a
// retry reads the one an earlier attempt already wrote under the pending secret.
func TestRotatingTheSigningSecretKeepsTheReleaseRecordReadable(t *testing.T) {
	ctx := context.Background()
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(ctx, spec); err != nil {
		t.Fatal(err)
	}
	rotated := spec
	rotated.HMACSecret, rotated.RecordSecret, rotated.Rotate = "n3w", spec.HMACSecret, []string{modsecrets.WebhookKey}
	for _, attempt := range []string{"under the current secret", "again, after the record was already saved with the pending one"} {
		if err := in.Enable(ctx, rotated); err != nil {
			t.Fatalf("%s: %v", attempt, err)
		}
	}
	if got := secretData(get(t, api, "Secret", "mod-stub", "can-module"))["CAN_WEBHOOK_SECRET"]; got != "n3w" {
		t.Fatalf("signing secret %q", got)
	}
	other := spec
	other.HMACSecret, other.RecordSecret = "x", "y"
	if err := in.Enable(ctx, other); err == nil {
		t.Fatal("a record signed with neither secret was accepted")
	}
}

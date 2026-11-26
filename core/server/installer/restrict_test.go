package installer

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

// Rotating the webhook secret must still read the release record written under the old one, both in
// an upgrade and when only the running release is changed.
func TestRotatingTheSigningSecretKeepsTheReleaseRecordReadable(t *testing.T) {
	ctx := context.Background()
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(ctx, spec); err != nil {
		t.Fatal(err)
	}
	rotated := spec
	rotated.HMACSecret, rotated.RecordSecret, rotated.Rotate = "n3w", spec.HMACSecret, []string{modsecrets.WebhookKey}
	if err := in.Enable(ctx, rotated); err != nil {
		t.Fatalf("upgrade with a rotated signing secret: %v", err)
	}
	again := rotated
	again.HMACSecret, again.RecordSecret = "n3wer", "n3w"
	// The chart on disk is not read: break it, and the installed release is still what is applied.
	in.cfg.ModulesDir = t.TempDir()
	before := get(t, api, "Deployment", "mod-stub", "stub")
	if err := in.Restrict(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got := secretData(get(t, api, "Secret", "mod-stub", "can-module"))["CAN_WEBHOOK_SECRET"]; got != "n3wer" {
		t.Fatalf("signing secret %q", got)
	}
	after := get(t, api, "Deployment", "mod-stub", "stub")
	version := func(o kube.Object) any {
		return dig(o, "spec", "template", "metadata", "annotations", secretVersionAnnotation)
	}
	if version(before) == version(after) {
		t.Fatal("the pods are not rolled for the new secret")
	}
	if dig(before, "spec", "replicas") != dig(after, "spec", "replicas") {
		t.Fatal("the workload changed")
	}
	// The record is now under the newest secret.
	last := again
	last.RecordSecret = ""
	if err := in.Restrict(ctx, last); err != nil {
		t.Fatal(err)
	}
}

//go:build !candevstack

package runtime

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

// The release build: whatever else is set, asking for the development stack stops can-core.
func TestReleaseBuildCannotRunTheDevStack(t *testing.T) {
	if devBuild {
		t.Fatal("the default build is a development build")
	}
	env := devEnv(t.TempDir())
	_, err := FromEnv(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "not built for development") {
		t.Fatalf("FromEnv: %v", err)
	}
	// A Config built without FromEnv is refused by Run before anything is opened.
	err = Run(context.Background(), Config{DevStack: true, DevModuleTokenDir: t.TempDir(), DevWebhookSecret: env["CAN_DEV_WEBHOOK_SECRET"], Installer: "none", TokenReview: "fake", Env: func(string) string { return "" }},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "not built for development") {
		t.Fatalf("Run: %v", err)
	}
}

// The fixed module identities of the compose stack are a development feature too.
func TestReleaseBuildRefusesDevModuleTokens(t *testing.T) {
	c := Config{TokenReview: "fake", Installer: "none", Env: func(k string) string {
		return map[string]string{"CAN_DEV_MODULE_TOKENS": "forms:" + strings.Repeat("f", 40)}[k]
	}}
	err := c.devModules(context.Background(), nil, tokenreview.NewFake(), slog.Default())
	if err == nil || !strings.Contains(err.Error(), "not built for development") {
		t.Fatalf("%v", err)
	}
}

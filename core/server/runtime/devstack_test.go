package runtime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

func devEnv(tokenDir string) map[string]string {
	return map[string]string{
		"CAN_DEV_STACK": "true", "CAN_DEV_MODULE_TOKEN_DIR": tokenDir, "CAN_INSTALLER": "none",
		"CAN_TOKEN_REVIEW": "fake", "CAN_COOKIE_SECURE": "false",
		"CAN_DEV_WEBHOOK_SECRET": "dev-hook-secret-0123456789abcdef-test",
	}
}

// asDevBuild lets a test exercise what the candevstack build tag switches on.
func asDevBuild(t *testing.T) {
	t.Helper()
	was := devBuild
	devBuild = true
	t.Cleanup(func() { devBuild = was })
}

func TestDevStackNeedsEveryDevelopmentSettingTogether(t *testing.T) {
	asDevBuild(t)
	dir := t.TempDir()
	fromEnv := func(change map[string]string) (Config, error) {
		env := devEnv(dir)
		for k, v := range change {
			env[k] = v
		}
		return FromEnv(func(k string) string { return env[k] })
	}
	c, err := fromEnv(nil)
	if err != nil || !c.DevStack || c.DevModuleTokenDir != dir {
		t.Fatalf("the compose configuration: %+v %v", c.DevStack, err)
	}
	for name, change := range map[string]map[string]string{
		"inside Kubernetes":     {"KUBERNETES_SERVICE_HOST": "10.43.0.1"},
		"kubernetes installer":  {"CAN_INSTALLER": "kubernetes"},
		"real token review":     {"CAN_TOKEN_REVIEW": "kubernetes"},
		"default token review":  {"CAN_TOKEN_REVIEW": ""},
		"secure cookies":        {"CAN_COOKIE_SECURE": "true"},
		"default cookies":       {"CAN_COOKIE_SECURE": ""},
		"no token directory":    {"CAN_DEV_MODULE_TOKEN_DIR": ""},
		"no webhook secret":     {"CAN_DEV_WEBHOOK_SECRET": ""},
		"short webhook secret":  {"CAN_DEV_WEBHOOK_SECRET": "short"},
		"settings, no switch":   {"CAN_DEV_STACK": ""},
		"secret only":           {"CAN_DEV_STACK": "", "CAN_DEV_MODULE_TOKEN_DIR": ""},
		"directory, switch off": {"CAN_DEV_STACK": "false"},
		"not a boolean":         {"CAN_DEV_STACK": "compose"},
	} {
		if _, err := fromEnv(change); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Without the mode nothing changes: no module management and no known module token.
	off, err := FromEnv(func(string) string { return "" })
	if err != nil || off.DevStack {
		t.Fatalf("%+v %v", off.DevStack, err)
	}
	if inst, err := off.newInstaller(slog.Default()); err != nil || inst != nil {
		t.Fatalf("installer none without the mode: %v %v", inst, err)
	}
	if inst, err := c.newInstaller(slog.Default()); err != nil || inst == nil {
		t.Fatalf("the dev stack must build module management: %v %v", inst, err)
	}
}

func TestDevStackTokenFiles(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	write := func(files map[string]string) string {
		dir := t.TempDir()
		for n, v := range files {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(v), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	work, people := strings.Repeat("w", 40), strings.Repeat("p", 40)
	fake, err := devReviewer(write(map[string]string{"work": work + "\n", "people": people, ".hidden": "x"}), log)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if id, err := fake.Review(ctx, work, tokenreview.Audience); err != nil || id.Namespace != "mod-work" || id.ServiceAccount != "work" {
		t.Fatalf("%+v %v", id, err)
	}
	for _, bad := range []string{"", "x", work + "x"} {
		if _, err := fake.Review(ctx, bad, tokenreview.Audience); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	for name, dir := range map[string]string{
		"missing directory": filepath.Join(t.TempDir(), "none"),
		"empty directory":   write(nil),
		"short token":       write(map[string]string{"work": "short"}),
		"shared token":      write(map[string]string{"work": work, "people": work}),
	} {
		if _, err := devReviewer(dir, log); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The identity forms uses for email.send is not in company.yaml: the periodic reconcile of the
// development stack must not disable it.
func TestDevStackReconcileKeepsDevIdentities(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := Config{TokenReview: "fake", Installer: "none", Env: func(k string) string {
		if k == "CAN_DEV_MODULE_TOKENS" {
			return "forms:" + strings.Repeat("f", 40)
		}
		return ""
	}}
	if err := c.devModules(ctx, db, tokenreview.NewFake(), log); err != nil {
		t.Fatal(err)
	}
	rec := newReconciler(db, registry.New(db, ""), audit.New(db), recordOnly{log})
	desired := reconcile.Desired{Company: &config.Company{}}
	if plan, err := rec.Plan(ctx, desired); err != nil || len(plan) != 1 {
		t.Fatalf("control: without Keep the identity would be disabled: %v %v", plan, err)
	}
	rec.Keep = isDevModule
	if plan, err := rec.Plan(ctx, desired); err != nil || len(plan) != 0 {
		t.Fatalf("%v %v", plan, err)
	}
}

package runtime

// The development stack mode (docker-compose.yml, docs/local-compose.md): module management without a
// cluster. Modules are recorded, never deployed, and each module's identity is a token read from a file
// the fake token review then accepts, so personal access tokens, launch tickets and the module catalogue
// work between plain containers.
//
// It makes can-core trust shared files in place of Kubernetes, so it must be impossible to switch on in
// production. Three independent things stand in the way, and every one of them fails start-up:
//
//   - the mode exists only in a binary built with `-tags candevstack` (dev/compose/Dockerfile; no
//     release image, manifest or script uses the tag, test/contract/devstack_test.go);
//   - it needs CAN_INSTALLER=none, CAN_TOKEN_REVIEW=fake and CAN_COOKIE_SECURE=false;
//   - it refuses to run inside Kubernetes (KUBERNETES_SERVICE_HOST, which the kubelet always sets).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

const (
	envDevStack     = "CAN_DEV_STACK"
	envDevTokenDir  = "CAN_DEV_MODULE_TOKEN_DIR"
	envDevHook      = "CAN_DEV_WEBHOOK_SECRET"
	minDevTokenLen  = 32
	maxDevTokenSize = 512
)

var devModuleIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// checkDevStack refuses every way of asking for the development stack that is not the compose one. A
// half-configured request (a token directory without the switch) is refused too, never ignored.
func (c Config) checkDevStack() error {
	if !c.DevStack {
		if c.DevModuleTokenDir != "" || c.DevWebhookSecret != "" {
			return fmt.Errorf("%s and %s need %s=true", envDevTokenDir, envDevHook, envDevStack)
		}
		return nil
	}
	switch {
	case !devBuild:
		return fmt.Errorf("%s: this can-core was not built for development and cannot run that mode", envDevStack)
	case c.env("KUBERNETES_SERVICE_HOST") != "":
		return fmt.Errorf("%s: refused inside Kubernetes", envDevStack)
	case c.Installer != "none" || c.TokenReview != "fake":
		return fmt.Errorf("%s needs CAN_INSTALLER=none and CAN_TOKEN_REVIEW=fake", envDevStack)
	case c.SecureCookies:
		return fmt.Errorf("%s is for plain-http development: it needs CAN_COOKIE_SECURE=false", envDevStack)
	case c.DevModuleTokenDir == "":
		return fmt.Errorf("%s needs %s", envDevStack, envDevTokenDir)
	case len(c.DevWebhookSecret) < minDevTokenLen:
		return fmt.Errorf("%s needs %s of at least %d characters (the modules' CAN_WEBHOOK_SECRET)", envDevStack, envDevHook, minDevTokenLen)
	}
	return nil
}

// devTokenFile is the name of the token inside a module's directory.
const devTokenFile = "token"

// devReviewer is a fake token review that knows one token per directory of dir: <dir>/<module id>/token.
// Each module gets a directory of its own so compose can give it that one and no other. The identity is
// the one the reconciler records for that module.
func devReviewer(dir string, log *slog.Logger) (*tokenreview.Fake, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envDevTokenDir, err)
	}
	fake, seen := tokenreview.NewFake(), map[string]string{}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !devModuleIDRe.MatchString(id) {
			continue // lost+found, stray files
		}
		b, err := os.ReadFile(filepath.Join(dir, id, devTokenFile)) //nolint:gosec // operator-configured directory, name checked above
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envDevTokenDir, err)
		}
		tok := strings.TrimSpace(string(b))
		if len(tok) < minDevTokenLen || len(tok) > maxDevTokenSize {
			return nil, fmt.Errorf("%s: the token of %s must be %d to %d characters", envDevTokenDir, id, minDevTokenLen, maxDevTokenSize)
		}
		if other, ok := seen[tok]; ok {
			return nil, fmt.Errorf("%s: %s and %s have the same token", envDevTokenDir, other, id)
		}
		seen[tok] = id
		fake.Add(tok, "mod-"+id, id)
	}
	if len(seen) == 0 {
		return nil, errors.New(envDevTokenDir + " holds no module token")
	}
	log.Warn("development stack: module identities are tokens read from files; never run this in production", "modules", len(seen))
	return fake, nil
}

// recordOnly is the installer of the development stack: the containers are started by Compose, so
// enabling or disabling a module only changes what can-core records (and therefore whether its tokens
// and launch tickets work).
type recordOnly struct{ log *slog.Logger }

// Enable implements reconcile.ModuleInstaller.
func (r recordOnly) Enable(_ context.Context, s reconcile.Spec) error {
	r.log.Info("development stack: module recorded as enabled; nothing deployed", "module", s.ID)
	return nil
}

// Disable implements reconcile.ModuleInstaller.
func (r recordOnly) Disable(_ context.Context, id string) error {
	r.log.Info("development stack: module recorded as disabled; its container keeps running", "module", id)
	return nil
}

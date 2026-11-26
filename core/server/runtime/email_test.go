package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

func TestDevModuleIdentities(t *testing.T) {
	asDevBuild(t)
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := func(review, installer, tokens string) Config {
		return Config{TokenReview: review, Installer: installer, Env: func(k string) string {
			if k == "CAN_DEV_MODULE_TOKENS" {
				return tokens
			}
			return ""
		}}
	}
	const spec = "forms:0123456789abcdef01234567"
	for name, c := range map[string]Config{
		"real token review": cfg("kubernetes", "none", spec),
		"with an installer": cfg("fake", "kubernetes", spec),
		"short token":       cfg("fake", "none", "forms:short"),
	} {
		if err := c.devModules(ctx, db, tokenreview.NewFake(), log); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	rev := tokenreview.NewFake()
	if err := cfg("fake", "none", spec).devModules(ctx, db, rev, log); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ModuleByIdentity(ctx, "mod-forms", "forms"); err != nil {
		t.Fatal(err)
	}
	// Started again without them (for instance as a real installation): the record goes.
	if err := cfg("kubernetes", "kubernetes", "").devModules(ctx, db, rev, log); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ModuleByIdentity(ctx, "mod-forms", "forms"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the stale development identity stayed:", err)
	}
}

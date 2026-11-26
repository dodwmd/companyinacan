package launch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type oneAnswer struct {
	out string
	err error
}

func (a oneAnswer) Run(context.Context, []byte, ...string) (string, error) { return a.out, a.err }

func TestModuleWarningsShowFailedUpgrades(t *testing.T) {
	ctx := context.Background()
	got := ModuleWarnings(ctx, oneAnswer{out: `[{"id":"support","state":"enabled","error":"rollout timed out"},{"id":"work","state":"failed","error":"no\u001b[2Jdatabase"}]` + "\n"})
	if len(got) != 2 || !strings.Contains(got[0], `"support": the upgrade failed`) || !strings.Contains(got[0], "rollout timed out") ||
		!strings.Contains(got[1], `"work": the last install or disable failed`) || strings.ContainsRune(got[1], 0x1b) {
		t.Fatalf("%q", got)
	}
	for name, a := range map[string]oneAnswer{
		"healthy":      {out: "[]\n"},
		"old can-core": {err: errors.New(`exit status 2: error: unknown command "modules" for "can-core"`)},
	} {
		if w := ModuleWarnings(ctx, a); len(w) != 0 {
			t.Errorf("%s: %q", name, w)
		}
	}
	if w := ModuleWarnings(ctx, oneAnswer{err: errors.New("no running pod")}); len(w) != 1 {
		t.Errorf("an unanswered question must be said: %q", w)
	}
}

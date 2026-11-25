package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// A planned provision is not a provider: a consumer must not be ordered after (or wait for) a
// module that only declares the capability.
func TestInstallOrderIgnoresPlannedProviders(t *testing.T) {
	app := mf("aaa-app", "1", []string{"cap.x"})
	ghost := mf("zzz-ghost", "1", nil, "cap.x")
	ghost.Provides[0].Status = "planned"
	got := installOrder(map[string]*config.Module{"aaa-app": app, "zzz-ghost": ghost})
	if strings.Join(got, ",") != "aaa-app,zzz-ghost" {
		t.Fatalf("planned provider ordered before its would-be consumer: %v", got)
	}
	ghost.Provides[0].Status = ""
	got = installOrder(map[string]*config.Module{"aaa-app": app, "zzz-ghost": ghost})
	if strings.Join(got, ",") != "zzz-ghost,aaa-app" {
		t.Fatalf("a served provider must come first: %v", got)
	}
}

func TestPlanRefusesNotDeployableModules(t *testing.T) {
	g := newRig(t)
	bad := mf("crmlike", "1", nil)
	bad.Status = "not-deployable"
	if _, err := g.r.Apply(context.Background(), desired(nil, nil, bad), Options{}); !errors.Is(err, ErrNotDeployable) {
		t.Fatalf("err = %v, want ErrNotDeployable", err)
	}
	if ms, _ := g.db.ListModules(context.Background()); len(ms) != 0 {
		t.Errorf("a not-deployable module was recorded: %v", ms)
	}
}

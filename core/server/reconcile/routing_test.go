package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/ticket"
)

type routedFake struct{ *FakeInstaller }

func (routedFake) Routing(_ context.Context, s Spec) (Routing, error) {
	r := Routing{Fingerprint: fmt.Sprint(s.Expose.UI)}
	if s.Expose.UI {
		r.UIHost = s.ID + ".acme.example"
	}
	return r, nil
}

// A change of exposure re-applies the module, and the address of a routed UI is what the Apps
// launcher uses without CAN_MODULE_UI_URLS (specs/040-module-routing).
func TestExposureIsAppliedAndRecorded(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	r := New(rg.db, rg.reg, audit.New(rg.db), routedFake{rg.fk})
	man := mf("work", "0.1.0", nil)
	man.Runtime.WebLogin = &config.WebLogin{BasePath: "/ui"}
	launcher := ticket.Launcher{Domain: "acme.example"}
	apply := func(ui bool, wantKind Kind, wantBase string) {
		t.Helper()
		d := desired(nil, nil, man)
		e := d.Company.Modules["work"]
		e.Expose = &config.Expose{UI: ui}
		d.Company.Modules["work"] = e
		res, err := r.Apply(ctx, d, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Actions) != 1 || res.Actions[0].Kind != wantKind {
			t.Fatalf("actions %v, want one %s", res.Actions, wantKind)
		}
		if got, _ := rg.fk.EnabledSpec("work"); got.Expose.UI != ui {
			t.Error("the installer was not told what to expose")
		}
		m, err := rg.db.GetModule(ctx, "work")
		if err != nil {
			t.Fatal(err)
		}
		if base, _ := launcher.Base(m); base != wantBase {
			t.Errorf("Apps address %q, want %q", base, wantBase)
		}
		if res, err := r.Apply(ctx, d, Options{}); err != nil || len(res.Actions) != 0 {
			t.Fatalf("not idempotent: %v %v", res.Actions, err)
		}
	}
	apply(false, Install, "")
	apply(true, Upgrade, "https://work.acme.example/ui")
	launcher.Overrides = map[string]string{"work": "https://tasks.acme.example"}
	m, _ := rg.db.GetModule(ctx, "work")
	if base, _ := launcher.Base(m); base != "https://tasks.acme.example/ui" {
		t.Errorf("CAN_MODULE_UI_URLS must still win: %q", base)
	}
	launcher.Overrides = nil
	apply(false, Upgrade, "")
}

type brokenRouting struct{ *FakeInstaller }

func (brokenRouting) Routing(context.Context, Spec) (Routing, error) {
	return Routing{}, errors.New("can-routing cannot be read")
}

// Routing facts that cannot be read must not stop disables or installs.
func TestUnreadableRoutingDoesNotStopReconciling(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	a, b := mf("a", "0.1.0", nil), mf("b", "0.1.0", nil)
	if _, err := New(rg.db, rg.reg, audit.New(rg.db), rg.fk).Apply(ctx, desired(nil, nil, a), Options{}); err != nil {
		t.Fatal(err)
	}
	r := New(rg.db, rg.reg, audit.New(rg.db), brokenRouting{rg.fk})
	res, err := r.Apply(ctx, desired(nil, map[string]bool{"a": false}, a, b), Options{})
	if err != nil || len(res.Actions) != 2 || res.Actions[0].Kind != Disable || res.Actions[1].Kind != Install {
		t.Fatalf("%v %v", res.Actions, err)
	}
}

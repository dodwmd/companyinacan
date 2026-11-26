package reconcile

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// The relay credentials are written (and withdrawn) only by an apply, so opting a module in or out,
// or changing the relay of an opted-in one, has to be an upgrade, and the installer has to be given
// the email block being applied.
func TestRelayOptInAndOptOutAreUpgrades(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	support, other := mf("support", "1", nil), mf("other", "1", nil)
	state := func(on *bool, host string) Desired {
		d := desired(nil, nil, support, other)
		if on != nil {
			d.Company.Email = &config.Email{Relay: &config.EmailRelay{Host: host},
				Modules: map[string]config.EmailModule{"support": {Enabled: on}, "other": {Enabled: on}}}
		}
		return d
	}
	yes, no := true, false
	apply := func(d Desired, want string) {
		t.Helper()
		res, err := g.r.Apply(ctx, d, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := kinds(res.Actions); got != want {
			t.Fatalf("actions %q, want %q", got, want)
		}
	}
	apply(state(nil, ""), "install:other,install:support")
	apply(state(&no, "a.example.net"), "") // not opted in: nothing to hand over or withdraw
	apply(state(&yes, "a.example.net"), "upgrade:support")
	if s, _ := g.fk.EnabledSpec("support"); !RelayOptIn(s.Email, "support") {
		t.Fatal("the installer was not given the opt-in")
	}
	apply(state(&yes, "a.example.net"), "")
	apply(state(&yes, "b.example.net"), "upgrade:support")
	apply(state(&no, "b.example.net"), "upgrade:support")
	if s, _ := g.fk.EnabledSpec("support"); RelayOptIn(s.Email, "support") {
		t.Fatal("the installer still sees the opt-in")
	}
}

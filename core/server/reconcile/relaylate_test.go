package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// relayInstaller reads what it would hand over: nothing until the Secret is visible.
type relayInstaller struct {
	*FakeInstaller
	seen string
	err  error
}

func (f *relayInstaller) RelayFingerprint(context.Context, Spec) (string, error) {
	return f.seen, f.err
}

// A relay Secret written just before the apply is not in the mounted volume yet. The module is then
// given nothing, and must be upgraded once the Secret shows up, and again when it changes.
func TestARelaySecretThatShowsUpLaterIsAnUpgrade(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	ri := &relayInstaller{FakeInstaller: g.fk}
	g.r.Installer = ri
	on := true
	d := desired(nil, nil, mf("support", "1", nil), mf("other", "1", nil))
	d.Company.Email = &config.Email{Relay: &config.EmailRelay{Host: "a.example.net"},
		Modules: map[string]config.EmailModule{"support": {Enabled: &on}, "other": {Enabled: &on}}}
	apply := func(want string) {
		t.Helper()
		res, err := g.r.Apply(ctx, d, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := kinds(res.Actions); got != want {
			t.Fatalf("actions %q, want %q", got, want)
		}
	}
	apply("install:other,install:support")
	apply("")
	ri.seen = "first"
	apply("upgrade:support")
	apply("")
	ri.seen = "second password"
	apply("upgrade:support")
	// A Secret that cannot be read just now changes nothing.
	ri.err = errors.New("unreadable")
	apply("")
	ri.err = nil
	apply("")
}

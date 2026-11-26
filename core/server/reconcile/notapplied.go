package reconcile

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// A module whose upgrade is held for approval gets no change at all: applying some of them to the
// installed release would mean rebuilding it without its chart. What is asked of it and does not
// arrive is named instead (Action.NotApplied).

// WaysOut says what an operator can do about a held module.
const WaysOut = "approve the upgrade, or disable the module in company.yaml (that takes it offline)"

// Applied is what of company.yaml a release was installed with.
type Applied struct {
	Expose     config.Expose `json:"expose"`
	RelayOptIn bool          `json:"relay_opt_in,omitempty"`
	// Relay identifies the relay settings and credential the module was given.
	Relay    string `json:"relay,omitempty"`
	Settings string `json:"settings"`
}

func appliedOf(d Desired, id, relay string) Applied {
	entry := d.Company.Modules[id]
	settings, _ := json.Marshal(entry.Settings) // sorted keys
	a := Applied{Expose: entry.Exposure(), Settings: digest(settings)}
	var mail []byte
	if modsecrets.WantsRelay(id) && RelayOptIn(d.Company.Email, id) {
		a.RelayOptIn = true
		mail, _ = json.Marshal(d.Company.Email.Relay)
	}
	if a.RelayOptIn || relay != "" { // relay is also a credential wired in from another module
		a.Relay = digest(mail, []byte(relay))
	}
	return a
}

func (a Applied) encode() string {
	b, _ := json.Marshal(a)
	return string(b)
}

// differences lists what now asks for that was does not have; urgent when something is taken away.
func differences(was, now Applied) (out []string, urgent bool) {
	away := func(removed bool, what string) {
		if removed {
			out, urgent = append(out, what), true
		}
	}
	w, n := was.Expose, now.Expose
	away(w.PublicOn() && !n.PublicOn(), "switching off the public routes (they stay public)")
	away(w.UI && !n.UI, "switching off the web UI route (it stays public)")
	away(w.API && !n.API, "switching off the API route (it stays public)")
	away(w.Signing && !n.Signing, "switching off the signing pages (they stay public)")
	away(w.TokenLogin && !n.TokenLogin, "switching off token sign-in")
	away(!w.EdgeLimitOn() && n.EdgeLimitOn(), "turning the edge rate limits on")
	if (!w.PublicOn() && n.PublicOn()) || (!w.UI && n.UI) || (!w.API && n.API) || (!w.Signing && n.Signing) ||
		(!w.TokenLogin && n.TokenLogin) || (w.EdgeLimitOn() && !n.EdgeLimitOn()) {
		out = append(out, "new exposure settings")
	}
	switch {
	case was.RelayOptIn && !now.RelayOptIn:
		away(true, "mail relay opt-out (the module keeps the credential)")
	case !was.RelayOptIn && now.RelayOptIn:
		out = append(out, "mail relay opt-in")
	case was.Relay != now.Relay:
		away(true, "new mail relay or wired credentials (the module keeps the old ones)")
	}
	if was.Settings != now.Settings {
		out = append(out, "changed settings")
	}
	return out, urgent
}

// notApplied fills in what a held step keeps from its module.
func (r *Reconciler) notApplied(ctx context.Context, d Desired, a *Action, rec store.Module, relay string, routesErr, relayErr error) error {
	rotate, err := r.db.PendingRotations(ctx, a.Module)
	if err != nil {
		return err
	}
	if len(rotate) > 0 {
		a.NotApplied, a.Urgent = append(a.NotApplied, "secret rotation of "+strings.Join(rotate, ", ")+" (the old values stay valid)"), true
	}
	now := appliedOf(d, a.Module, relay)
	var was Applied
	known := rec.Applied != "" && json.Unmarshal([]byte(rec.Applied), &was) == nil
	if relayErr != nil {
		now.Relay = was.Relay
	}
	// Only a routing installer puts the exposure into the hash; without one it proves nothing.
	if _, routes := r.Installer.(RouteAware); !known && routes && routesErr == nil && relayErr == nil && rec.SettingsHash != "" {
		// A record from before Applied: its hash says whether company.yaml still asks for what
		// was installed. If it does, that is the baseline from here on.
		old := Desired{Company: d.Company, Manifests: map[string]*config.Module{a.Module: &rec.Manifest}}
		if rt, err := r.routing(ctx, old, a.Module); err == nil && specHash(d.Company, a.Module, &rec.Manifest, rt.Fingerprint, relay) == rec.SettingsHash {
			was, known, a.baseline = now, true, now.encode()
		}
	}
	if !known {
		a.NotApplied = append(a.NotApplied, "anything changed in its settings, exposure or mail relay since it was installed (can-core cannot tell for this module: routes you switched off may still be public)")
		a.Urgent = true
		return nil
	}
	diff, urgent := differences(was, now)
	a.NotApplied, a.Urgent = append(a.NotApplied, diff...), a.Urgent || urgent
	return nil
}

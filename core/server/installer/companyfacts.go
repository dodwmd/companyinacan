package installer

import (
	"context"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// Configuration a module needs that no chart can hold: facts of company.yaml (links.go has what
// comes from another module). reconcile.companyRevision makes a change of them an upgrade.

// companyValues are the chart value overrides built from company.yaml. jobs does not start without
// company.name and company.base_url; files puts share.base_url in front of the share links it
// returns. The origin is the module's own host (runtime.subdomain), http only when the cluster's
// routing facts say there is no TLS.
func (in *Installer) companyValues(ctx context.Context, s reconcile.Spec) (map[string]any, error) {
	if s.ID == "equity" { // the break-glass window; the identity mode is a connection (links.go)
		st, err := config.ReadEquitySettings(s.Settings)
		if err == nil && s.Within != nil {
			st.BreakGlassUntil = reconcile.WithinBreakGlass(*s.Within, st.BreakGlassUntil)
		}
		if err != nil || st.BreakGlassUntil == "" {
			return nil, err
		}
		return map[string]any{"breakGlassUntil": st.BreakGlassUntil}, nil
	}
	domain, sub := in.domain(s), s.Manifest.Runtime.Subdomain
	if domain == "" || sub == "" || (s.ID != "jobs" && s.ID != "files") {
		return nil, nil
	}
	data, _, err := in.routingData(ctx)
	if err != nil {
		return nil, err
	}
	scheme := "https"
	if data["tls"] == "false" {
		scheme = "http"
	}
	origin := scheme + "://" + sub + "." + domain
	var conf map[string]any
	switch {
	case s.ID == "jobs":
		company := map[string]any{"base_url": origin}
		if s.CompanyName != "" {
			company["name"] = s.CompanyName
		}
		conf = map[string]any{"company": company}
	case s.Expose.PublicOn(): // files, and only while its links are reachable from outside
		conf = map[string]any{"share": map[string]any{"base_url": origin}}
	default:
		return nil, nil
	}
	return map[string]any{"config": conf}, nil
}

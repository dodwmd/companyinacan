package adminui

// The "Internet access" section of a module's page (specs/045-public-access): what company.yaml
// exposes and the resulting addresses. Read-only on purpose.

import (
	"context"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/routing"
)

type exposureRow struct {
	Key, Label, State, StateClass, URL string
}

type exposureView struct {
	Unavailable bool // company.yaml cannot be read here
	Disabled    bool
	NoDomain    bool
	Rows        []exposureRow
}

func (s *Server) exposure(ctx context.Context, id string, m *config.Module) *exposureView {
	v := &exposureView{}
	if s.Mod == nil || s.Mod.Company == nil {
		v.Unavailable = true
		return v
	}
	e, err := s.Mod.Exposure(ctx, id)
	if err != nil {
		v.Unavailable = true
		return v
	}
	v.Disabled, v.NoDomain = !e.Enabled, e.Domain == ""
	urls := map[string]string{}
	if e.Enabled {
		for _, sf := range routing.Surfaces(e.Domain, id, m, e.Expose) {
			urls[sf.Kind] = sf.URL
		}
	}
	add := func(kind string, on bool) {
		labels := map[string]string{
			routing.KindUI: "exp.ui", routing.KindAPI: "exp.api", routing.KindPages: "exp.pages",
			routing.KindLink: "exp.invoice_link", routing.KindForms: "exp.forms",
			routing.KindSign: "exp.signing",
		}
		row := exposureRow{Key: kind, Label: s.msg(labels[kind]), State: s.msg("exp.off"), StateClass: "neutral"}
		if on && e.Enabled {
			row.State, row.StateClass, row.URL = s.msg("exp.on"), "ok", urls[kind]
			// can-core records the host it routed the UI on; none means nothing answers there yet.
			if kind == routing.KindUI && m.Runtime.RoutedUIHost == "" {
				row.State, row.StateClass = s.msg("exp.not_routed"), "warn"
			}
		}
		v.Rows = append(v.Rows, row)
	}
	rt := m.Runtime
	if rt.PublicUI != nil {
		add(routing.KindUI, e.Expose.UI)
		add(routing.KindAPI, e.Expose.API)
	}
	if rt.Subdomain != "" {
		add(routing.KindPages, e.Expose.PublicOn())
	}
	if rt.PublicLink != nil {
		add(routing.KindLink, routing.LinkWanted(m, e.Expose))
	}
	if rt.PublicSign != nil {
		add(routing.KindSign, routing.SignWanted(m, e.Expose))
	}
	if rt.SiteRoute != nil {
		add(routing.KindForms, e.Expose.PublicOn())
	}
	return v
}

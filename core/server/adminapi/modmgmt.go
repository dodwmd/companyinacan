package adminapi

import (
	"errors"
	"net/http"
	"sort"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

// Read-only module-management endpoints (epic E12b, specs/017-module-ui-wizard). Changes are made
// through the admin UI's preview-then-apply flow, which is where the confirmation, opt-in and
// audit steps live; the API only reads.

func (s *Server) modRoutes() []route {
	return []route{
		{"GET /api/v1/catalogue", s.authedPerm(rbac.PermView, s.getCatalogue)},
		{"GET /api/v1/status", s.authedPerm(rbac.PermView, s.getStatus)},
	}
}

type catalogueEntry struct {
	ID              string         `json:"id"`
	Name            string         `json:"name,omitempty"`
	Version         string         `json:"version,omitempty"`
	Status          string         `json:"status"`
	LicenceClass    string         `json:"licence_class,omitempty"`
	SourceAvailable bool           `json:"source_available"`
	Deployable      bool           `json:"deployable"`
	Enabled         bool           `json:"enabled"`
	RAMMB           int            `json:"ram_mb"`
	RAMLimitMB      int            `json:"ram_limit_mb"`
	Requires        []needEntry    `json:"requires"`
	Provides        []provideEntry `json:"provides"`
	Invalid         string         `json:"invalid,omitempty"`
}

type needEntry struct {
	Capability string `json:"capability"`
	State      string `json:"state"`
}

type provideEntry struct {
	Capability string `json:"capability"`
	Served     bool   `json:"served"`
}

type footprintView struct {
	PlatformMB int    `json:"platform_mb"`
	RequestMB  int    `json:"request_mb"`
	LimitMB    int    `json:"limit_mb"`
	DatabaseMB int    `json:"database_mb"`
	TotalMB    int    `json:"total_mb"`
	ServerMB   int    `json:"server_mb"`
	Verdict    string `json:"verdict"`
}

func (s *Server) getCatalogue(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.Mod == nil {
		httpx.Problem(w, http.StatusServiceUnavailable, "module management is not configured")
		return
	}
	cat, err := s.Mod.Catalogue()
	if err != nil {
		s.internal(w, r, err)
		return
	}
	company := &config.Company{}
	cur, err := s.Mod.Company.Get(r.Context())
	switch {
	case err == nil:
		if company, err = modmgr.ParseCompany(cur.Data); err != nil {
			s.internal(w, r, err)
			return
		}
	case !errors.Is(err, modmgr.ErrNoCompany):
		s.internal(w, r, err)
		return
	}
	enabled := modmgr.EnabledModules(company)
	an := cat.Analyse(enabled, company.Primary)
	var mods []*config.Module
	out := make([]catalogueEntry, 0, len(cat.Entries))
	for _, e := range cat.Entries {
		ce := catalogueEntry{ID: e.ID, Status: e.Status(), Deployable: e.Deployable(), Enabled: enabled[e.ID], SourceAvailable: e.SourceAvailable(),
			Requires: []needEntry{}, Provides: []provideEntry{}, Invalid: modmgr.Sanitise(e.Invalid)}
		if m := e.Manifest; m != nil {
			ce.Name, ce.Version, ce.LicenceClass = m.Name, m.Version, m.Provider.LicenceClass
			ce.RAMMB, ce.RAMLimitMB = m.Resources.RAMMB, m.Resources.RAMLimitMB
			if ce.RAMLimitMB == 0 {
				ce.RAMLimitMB = m.Resources.RAMMB * 3 / 2
			}
			for _, c := range m.Requires.Capabilities {
				state := "unmet"
				if n := an.Need(e.ID, c); n != nil {
					state = n.State
				}
				ce.Requires = append(ce.Requires, needEntry{Capability: c, State: state})
			}
			for _, p := range m.Provides {
				ce.Provides = append(ce.Provides, provideEntry{Capability: p.Capability, Served: config.ProvidesServed(p.Status)})
			}
			if enabled[e.ID] {
				mods = append(mods, m)
			}
		}
		out = append(out, ce)
	}
	f := modmgr.Footprint(mods, modmgr.ServerRAMMB(company))
	conflicts := []string{}
	for _, c := range an.Conflicts {
		conflicts = append(conflicts, c.Capability)
	}
	sort.Strings(conflicts)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"modules": out, "conflicts": conflicts,
		"footprint": footprintView{PlatformMB: f.PlatformMB, RequestMB: f.RequestMB, LimitMB: f.LimitMB, DatabaseMB: f.DatabaseMB, TotalMB: f.TotalMB, ServerMB: f.ServerMB, Verdict: f.Verdict},
	})
}

type moduleHealthView struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Ready     string `json:"ready"`
	Version   string `json:"version"`
	LastError string `json:"last_error,omitempty"`
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.Mod == nil {
		httpx.Problem(w, http.StatusServiceUnavailable, "module management is not configured")
		return
	}
	h, err := s.Mod.Status(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	mods := make([]moduleHealthView, 0, len(h.Modules))
	for _, m := range h.Modules {
		mods = append(mods, moduleHealthView{ID: m.ID, State: m.State, Ready: m.Ready, Version: m.Version, LastError: m.LastError})
	}
	body := map[string]any{"modules": mods, "certificates_available": h.CertsNote == "", "disk_available": h.DiskNote == ""}
	if h.Reconcile != nil {
		body["reconcile"] = map[string]any{"at": h.Reconcile.At, "ok": h.Reconcile.OK, "actions": h.Reconcile.Actions, "failed": h.Reconcile.Failed, "error": h.Reconcile.Error}
	}
	certs := make([]modmgr.CertStatus, 0, len(h.Certs))
	body["certificates"] = append(certs, h.Certs...)
	disk := make([]modmgr.DiskUsage, 0, len(h.Disk))
	body["disk"] = append(disk, h.Disk...)
	httpx.WriteJSON(w, http.StatusOK, body)
}

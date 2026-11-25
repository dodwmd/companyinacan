package adminui

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Module management pages (epic E12b, specs/017-module-ui-wizard): the catalogue with footprint and
// capability graph, generated settings forms, change previews and applies, the run progress page,
// the status page and the purge flow. The logic is core/server/modmgr; these handlers only read
// forms, call it and render. Every route is permission-gated; every POST carries the CSRF token
// (authed checks it).

func (s *Server) modRoutes() []route {
	return []route{
		{"GET /catalogue", s.authedPerm(rbac.PermView, s.mod(s.catalogue))},
		{"GET /catalogue/{id}", s.authedPerm(rbac.PermView, s.mod(s.catalogueModule))},
		{"POST /catalogue/{id}/settings", s.authedPerm(rbac.PermOperate, s.mod(s.catalogueSettings))},
		{"GET /catalogue/{id}/purge", s.authedPerm(rbac.PermPurge, s.mod(s.purgePage))},
		{"POST /catalogue/{id}/purge", s.authedPerm(rbac.PermPurge, s.mod(s.purgePost))},
		{"POST /changes", s.authedPerm(rbac.PermOperate, s.mod(s.changeCreate))},
		{"GET /changes/{id}", s.authedPerm(rbac.PermOperate, s.mod(s.changePage))},
		{"POST /changes/{id}/apply", s.authedPerm(rbac.PermOperate, s.mod(s.changeApply))},
		{"GET /runs/{id}", s.authedPerm(rbac.PermView, s.mod(s.runPage))},
		{"GET /status", s.authedPerm(rbac.PermView, s.mod(s.statusPage))},
		{"GET /company", s.authedPerm(rbac.PermOperate, s.mod(s.companyPage))},
	}
}

// mod answers 503 when module management is not configured.
func (s *Server) mod(h func(http.ResponseWriter, *http.Request, *sessionCtx)) func(http.ResponseWriter, *http.Request, *sessionCtx) {
	return func(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
		if s.Mod == nil {
			s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
			return
		}
		h(w, r, sc)
	}
}

// modNav adds the catalogue, status and setup links when module management is configured.
func (s *Server) modNav(r *http.Request, sc *sessionCtx, active string) []navItem {
	if s.Mod == nil {
		return nil
	}
	out := []navItem{
		{Href: "/catalogue", Label: s.msg("nav.catalogue"), Current: active == "catalogue"},
		{Href: "/status", Label: s.msg("nav.status"), Current: active == "status"},
	}
	if ok, err := s.allowed(r, sc, rbac.PermSetup); err == nil && ok {
		if need, err := s.Mod.WizardNeeded(r.Context()); err == nil && need {
			out = append(out, navItem{Href: "/setup", Label: s.msg("nav.setup"), Current: active == "setup"})
		}
	}
	return out
}

// failMod maps a modmgr error to an error page.
func (s *Server) failMod(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error) {
	var de *modmgr.DetailError
	switch {
	case errors.Is(err, modmgr.ErrNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
	case errors.Is(err, modmgr.ErrStale), errors.Is(err, modmgr.ErrConflict):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.stale")
	case errors.Is(err, modmgr.ErrExpired):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.expired")
	case errors.Is(err, modmgr.ErrBusy):
		w.Header().Set("Retry-After", "10")
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.busy", "err.apply_busy")
	case errors.Is(err, modmgr.ErrNoCompany):
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.no_company")
	case errors.Is(err, modmgr.ErrNoChange):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.no_change")
	case errors.Is(err, modmgr.ErrAckRequired):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.ack_required")
	case errors.As(err, &de), errors.Is(err, modmgr.ErrInvalid), errors.Is(err, modmgr.ErrNotDeployable), errors.Is(err, modmgr.ErrBlocked):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.change_invalid")
	default:
		s.internal(w, r, sc, err)
	}
}

// --- catalogue ----------------------------------------------------------------------------------

type capView struct {
	Capability, State, StateLabel, StateClass string
	Providers, Available                      []string
}

type provView struct {
	Capability string
	Served     bool
}

type catRow struct {
	ID, Name, Description, Version, Category    string
	StatusLabel, StatusClass                    string
	Licence                                     string
	SourceAvailable, Experimental               bool
	RAMMB, LimitMB, DiskMB                      int
	Enabled, Selectable                         bool
	Invalid                                     string
	RecordedState, RecordedClass, RecordedLabel string
	Requires                                    []capView
	Provides                                    []provView
	ConflictNote                                []string
	CanPurge                                    bool
	AckLicence, AckExperimental                 bool
}

type fpView struct {
	modmgr.FootprintResult
	VerdictLabel, VerdictClass string
}

type catalogueData struct {
	Rows       []catRow
	Footprint  fpView
	Conflicts  []modmgr.Conflict
	Version    string
	CanOperate bool
	Truncated  bool
	NoCompany  bool
}

var needLabels = map[string][2]string{
	modmgr.NeedMet:      {"cg.need_met", "ok"},
	modmgr.NeedPlatform: {"cg.need_platform", "ok"},
	modmgr.NeedPlanned:  {"cg.need_planned", "warn"},
	modmgr.NeedMissing:  {"cg.need_missing", "err"},
	modmgr.NeedNone:     {"cg.need_none", "err"},
}

var verdictLabels = map[string][2]string{
	"fits":    {"cg.fp_fits", "ok"},
	"tight":   {"cg.fp_tight", "warn"},
	"over":    {"cg.fp_over", "err"},
	"unknown": {"cg.fp_unknown", "neutral"},
}

var moduleStatusLabels = map[string][2]string{
	"ready":          {"cg.status_ready", "ok"},
	"experimental":   {"cg.status_experimental", "warn"},
	"not-deployable": {"cg.status_not_deployable", "err"},
}

func (s *Server) fpView(f modmgr.FootprintResult) fpView {
	l := verdictLabels[f.Verdict]
	return fpView{FootprintResult: f, VerdictLabel: s.msg(l[0]), VerdictClass: l[1]}
}

func (s *Server) needViews(a modmgr.Analysis, module string) []capView {
	var out []capView
	for _, n := range a.Needs {
		if n.Module != module {
			continue
		}
		l := needLabels[n.State]
		out = append(out, capView{Capability: n.Capability, State: n.State, StateLabel: s.msg(l[0]), StateClass: l[1], Providers: n.Providers, Available: n.Available})
	}
	return out
}

func (s *Server) recordedStates(r *http.Request) (map[string]store.Module, error) {
	ms, err := s.DB.ListModules(r.Context())
	if err != nil {
		return nil, err
	}
	out := map[string]store.Module{}
	for _, m := range ms {
		out[m.ID] = m
	}
	return out, nil
}

func (s *Server) catalogue(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ctx := r.Context()
	cat, err := s.Mod.Catalogue()
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	cur, err := s.Mod.Company.Get(ctx)
	if err != nil && !errors.Is(err, modmgr.ErrNoCompany) {
		s.internal(w, r, sc, err)
		return
	}
	d := catalogueData{Truncated: cat.Truncated, NoCompany: errors.Is(err, modmgr.ErrNoCompany), Version: cur.Version}
	var company *config.Company
	if !d.NoCompany {
		if company, err = modmgr.ParseCompany(cur.Data); err != nil {
			s.internal(w, r, sc, err)
			return
		}
	} else {
		company = &config.Company{}
	}
	enabled := modmgr.EnabledModules(company)
	rec, err := s.recordedStates(r)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d.CanOperate, _ = s.allowed(r, sc, rbac.PermOperate)
	canPurge, _ := s.allowed(r, sc, rbac.PermPurge)
	base := cat.Analyse(enabled, company.Primary)
	d.Conflicts = base.Conflicts
	var mods []*config.Module
	for id := range enabled {
		if e := cat.Get(id); e != nil && e.Manifest != nil {
			mods = append(mods, e.Manifest)
		}
	}
	d.Footprint = s.fpView(modmgr.Footprint(mods, modmgr.ServerRAMMB(company)))
	for _, e := range cat.Entries {
		d.Rows = append(d.Rows, s.catRowFor(e, cat, enabled, company.Primary, rec, canPurge))
	}
	s.render(w, r, http.StatusOK, "catalogue.html", s.page(r, sc, "catalogue", s.msg("cg.title"), d))
}

func (s *Server) catRowFor(e *modmgr.Entry, cat *modmgr.Catalogue, enabled map[string]bool, primary map[string]string, rec map[string]store.Module, canPurge bool) catRow {
	row := catRow{ID: e.ID, Name: e.ID, Invalid: modmgr.Sanitise(e.Invalid), Enabled: enabled[e.ID], Selectable: e.Deployable() || enabled[e.ID]}
	l := moduleStatusLabels[e.Status()]
	row.StatusLabel, row.StatusClass = s.msg(l[0]), l[1]
	if m := e.Manifest; m != nil {
		row.Name, row.Description, row.Version, row.Category = m.Name, m.Description, m.Version, m.Category
		row.Licence = m.Provider.Licence
		row.RAMMB, row.LimitMB, row.DiskMB = m.Resources.RAMMB, m.Resources.RAMLimitMB, m.Resources.DiskMB
		if row.LimitMB == 0 {
			row.LimitMB = m.Resources.RAMMB * 3 / 2
		}
		row.SourceAvailable, row.Experimental = e.SourceAvailable(), e.Status() == "experimental"
		with := map[string]bool{e.ID: true}
		for id := range enabled {
			with[id] = true
		}
		a := cat.Analyse(with, primary)
		row.Requires = s.needViews(a, e.ID)
		for _, p := range a.Provides {
			if p.Module == e.ID {
				row.Provides = append(row.Provides, provView{Capability: p.Capability, Served: p.Served})
			}
		}
		if !enabled[e.ID] {
			for _, c := range a.Conflicts {
				for _, id := range c.Modules {
					if id == e.ID {
						row.ConflictNote = append(row.ConflictNote, c.Capability+": "+strings.Join(others(c.Modules, e.ID), ", "))
					}
				}
			}
		}
	}
	if m, ok := rec[e.ID]; ok {
		row.RecordedState = m.State
		switch m.State {
		case store.StateEnabled:
			row.RecordedClass, row.RecordedLabel = "ok", s.msg("state.enabled")
		case store.StateFailed:
			row.RecordedClass, row.RecordedLabel = "err", s.msg("state.failed")
		default:
			row.RecordedClass, row.RecordedLabel = "neutral", s.msg("state.disabled")
		}
		row.CanPurge = canPurge && m.State == store.StateDisabled && !enabled[e.ID]
	}
	return row
}

func others(l []string, self string) []string {
	var out []string
	for _, x := range l {
		if x != self {
			out = append(out, x)
		}
	}
	return out
}

// changeCreate turns the catalogue selection into a proposal.
func (s *Server) changeCreate(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ctx := r.Context()
	cur, err := s.Mod.Company.Get(ctx)
	if err != nil {
		s.failMod(w, r, sc, err)
		return
	}
	if r.PostFormValue("version") != cur.Version {
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.stale")
		return
	}
	company, err := modmgr.ParseCompany(cur.Data)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	cat, err := s.Mod.Catalogue()
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	want := map[string]bool{}
	for _, id := range r.PostForm["enable"] {
		want[id] = true
	}
	have := modmgr.EnabledModules(company)
	var patch modmgr.Patch
	for _, e := range cat.Entries { // only catalogue modules can change; unknown ids are ignored
		if want[e.ID] != have[e.ID] {
			patch.Ops = append(patch.Ops, modmgr.SetEnabled(e.ID, want[e.ID]))
		}
	}
	if len(patch.Ops) == 0 {
		s.redirect(w, r, "/catalogue", "flash.no_changes")
		return
	}
	ch, err := s.Mod.Propose(ctx, sc.User.Username, modmgr.Proposal{Patch: patch, Acks: boundedAcks(r.PostForm["ack"]), Summary: "catalogue"})
	if err != nil {
		s.failMod(w, r, sc, err)
		return
	}
	http.Redirect(w, r, "/changes/"+ch.ID, http.StatusSeeOther)
}

func boundedAcks(in []string) []string {
	if len(in) > 2*modmgr.MaxModules {
		in = in[:2*modmgr.MaxModules]
	}
	return in
}

// --- module detail and settings -----------------------------------------------------------------

type fieldView struct {
	modmgr.Field
	ErrLabel string
}

type catModuleData struct {
	Row         catRow
	Fields      []fieldView
	Unsupported []string
	HasSchema   bool
	SchemaNote  string
	CanOperate  bool
	CanPurge    bool
	Settings    string
	Error       string
}

var fieldErrKeys = map[string]string{
	"required": "cg.err_required", "type": "cg.err_type", "range": "cg.err_range", "length": "cg.err_length",
	"pattern": "cg.err_pattern", "enum": "cg.err_enum", "invalid": "cg.err_invalid",
}

func (s *Server) currentModuleState(r *http.Request, id string) (*modmgr.Catalogue, *modmgr.Entry, *config.Company, map[string]store.Module, error) {
	cat, err := s.Mod.Catalogue()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	e := cat.Get(id)
	cur, err := s.Mod.Company.Get(r.Context())
	if err != nil {
		return cat, e, nil, nil, err
	}
	c, err := modmgr.ParseCompany(cur.Data)
	if err != nil {
		return cat, e, nil, nil, err
	}
	rec, err := s.recordedStates(r)
	return cat, e, c, rec, err
}

func (s *Server) renderModule(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, id string, submitted url.Values, errs map[string]string) {
	cat, e, company, rec, err := s.currentModuleState(r, id)
	if errors.Is(err, modmgr.ErrNoCompany) {
		s.failMod(w, r, sc, err)
		return
	}
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	if e == nil {
		s.fail(w, r, sc, http.StatusNotFound, "err.module_not_found", "err.module_not_found_msg")
		return
	}
	enabled := modmgr.EnabledModules(company)
	canPurge, _ := s.allowed(r, sc, rbac.PermPurge)
	d := catModuleData{Row: s.catRowFor(e, cat, enabled, company.Primary, rec, canPurge), HasSchema: e.Schema != nil}
	d.CanOperate, _ = s.allowed(r, sc, rbac.PermOperate)
	d.CanPurge = d.Row.CanPurge
	if e.SchemaProblem != "" {
		d.SchemaNote = modmgr.Sanitise(e.SchemaProblem)
	}
	if errs != nil {
		if msg, ok := errs[""]; ok {
			d.Error = s.msg(fieldErrKeys[msg])
		}
	}
	var current map[string]any
	if ent, ok := company.Modules[id]; ok {
		current = ent.Settings
	}
	form := e.BuildForm(current)
	for _, f := range form.Fields {
		if submitted != nil { // show what was typed, not what is stored
			f.Value = strings.TrimSpace(submitted.Get(modmgr.FormPrefix + f.Name))
			f.Checked = submitted.Get(modmgr.FormPrefix+f.Name) != ""
		}
		fv := fieldView{Field: f}
		if code, ok := errs[f.Name]; ok {
			fv.ErrLabel = s.msg(fieldErrKeys[code])
		}
		d.Fields = append(d.Fields, fv)
	}
	d.Unsupported = form.Unsupported
	s.render(w, r, status, "cmodule.html", s.page(r, sc, "catalogue", d.Row.Name, d))
}

func (s *Server) catalogueModule(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.renderModule(w, r, sc, http.StatusOK, r.PathValue("id"), nil, nil)
}

func (s *Server) catalogueSettings(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id := r.PathValue("id")
	cat, e, company, _, err := s.currentModuleState(r, id)
	_ = cat
	if errors.Is(err, modmgr.ErrNoCompany) {
		s.failMod(w, r, sc, err)
		return
	}
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	if e == nil || e.Schema == nil {
		s.fail(w, r, sc, http.StatusNotFound, "err.module_not_found", "err.module_not_found_msg")
		return
	}
	var current map[string]any
	if ent, ok := company.Modules[id]; ok {
		current = ent.Settings
	}
	merged, errs := e.ParseSettings(current, r.PostForm)
	if len(errs) > 0 {
		s.renderModule(w, r, sc, http.StatusBadRequest, id, r.PostForm, errs)
		return
	}
	ch, err := s.Mod.Propose(r.Context(), sc.User.Username, modmgr.Proposal{Patch: modmgr.Patch{Ops: []modmgr.Op{modmgr.SetSettings(id, merged)}}, Summary: "settings " + id})
	if err != nil {
		s.failMod(w, r, sc, err)
		return
	}
	http.Redirect(w, r, "/changes/"+ch.ID, http.StatusSeeOther)
}

// --- change preview and apply -------------------------------------------------------------------

type diffRow struct {
	Class, Mark, Text string
}

type ackView struct {
	Key, Label string
	Given      bool
}

type resourceView struct {
	modmgr.ResourcePlan
	ActionLabel string
	KeepLabels  []string
}

type changeData struct {
	ID, Actor, Summary string
	Created            time.Time
	Applied            bool
	RunID              string
	Stale              bool
	Diff               []diffRow
	DiffTooLarge       bool
	Actions            []actionView
	Resources          []resourceView
	Footprint          fpView
	Issues             []string
	MissingAcks        []ackView
	CanApply           bool
	Conflicts          []modmgr.Conflict
	Needs              []needRow
}

type needRow struct {
	Module string
	capView
}

type actionView struct{ Kind, KindLabel, Module, Reason string }

var actionLabels = map[reconcile.Kind]string{
	reconcile.Install: "ch.act_install", reconcile.Upgrade: "ch.act_upgrade",
	reconcile.Enable: "ch.act_enable", reconcile.Disable: "ch.act_disable",
}

var keepLabels = map[string]string{
	"namespace": "ch.keep_namespace", "pvc": "ch.keep_pvc", "secrets": "ch.keep_secrets", "database": "ch.keep_database",
}

func (s *Server) previewData(pv *modmgr.Preview) changeData {
	d := changeData{ID: pv.Change.ID, Actor: pv.Change.Actor, Summary: pv.Change.Summary, Created: pv.Change.CreatedAt, Stale: pv.Stale,
		Applied: pv.Change.State == modmgr.ChangeApplied, RunID: pv.Change.RunID, DiffTooLarge: pv.DiffTooLarge, Footprint: s.fpView(pv.Footprint), Conflicts: pv.Analysis.Conflicts}
	for _, l := range pv.Diff {
		switch l.Op {
		case '+':
			d.Diff = append(d.Diff, diffRow{"add", "+", l.Text})
		case '-':
			d.Diff = append(d.Diff, diffRow{"del", "-", l.Text})
		case '~':
			d.Diff = append(d.Diff, diffRow{"skip", "~", ""})
		default:
			d.Diff = append(d.Diff, diffRow{"ctx", " ", l.Text})
		}
	}
	for _, a := range pv.Actions {
		d.Actions = append(d.Actions, actionView{Kind: string(a.Kind), KindLabel: s.msg(actionLabels[a.Kind]), Module: a.Module, Reason: modmgr.Sanitise(a.Reason)})
	}
	for _, r := range pv.Resources {
		rv := resourceView{ResourcePlan: r, ActionLabel: s.msg(actionLabels[reconcile.Kind(r.Action)])}
		for _, k := range r.Keeps {
			rv.KeepLabels = append(rv.KeepLabels, s.msg(keepLabels[k]))
		}
		d.Resources = append(d.Resources, rv)
	}
	d.Issues = append(d.Issues, pv.Problems...)
	d.Issues = append(d.Issues, pv.Blockers...)
	for _, id := range pv.NotDeployable {
		d.Issues = append(d.Issues, s.msg("ch.not_deployable", id))
	}
	for _, a := range pv.AckNeeded {
		key := "ch.ack_licence"
		if a.Kind == "experimental" {
			key = "ch.ack_experimental"
		}
		d.MissingAcks = append(d.MissingAcks, ackView{Key: a.Key, Label: s.msg(key, a.Module), Given: a.Given})
	}
	for _, n := range pv.Analysis.Needs {
		l := needLabels[n.State]
		d.Needs = append(d.Needs, needRow{Module: n.Module, capView: capView{Capability: n.Capability, State: n.State, StateLabel: s.msg(l[0]), StateClass: l[1], Providers: n.Providers, Available: n.Available}})
	}
	d.CanApply = !d.Applied && !d.Stale && pv.Err() == nil
	return d
}

func (s *Server) changePage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	pv, err := s.Mod.Preview(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failMod(w, r, sc, err)
		return
	}
	s.render(w, r, http.StatusOK, "change.html", s.page(r, sc, "catalogue", s.msg("ch.title"), s.previewData(pv)))
}

func (s *Server) changeApply(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	run, err := s.Mod.Apply(r.Context(), sc.User.Username, r.PathValue("id"))
	if err != nil {
		s.failMod(w, r, sc, err)
		return
	}
	http.Redirect(w, r, "/runs/"+run.ID, http.StatusSeeOther)
}

// --- run progress -------------------------------------------------------------------------------

type outcomeView struct {
	ID, ActionLabel, StateLabel, StateClass, LastError string
}

type runData struct {
	ID, Actor, Error       string
	StateLabel, StateClass string
	Running                bool
	Refresh                bool
	Started                time.Time
	Finished               *time.Time
	Modules                []outcomeView
	Failed                 []string
}

func (s *Server) runView(run modmgr.Run, refresh bool) runData {
	d := runData{ID: run.ID, Actor: run.Actor, Error: run.Error, Started: run.StartedAt, Finished: run.FinishedAt, Failed: run.Failed, Running: run.State == modmgr.RunRunning}
	d.Refresh = d.Running && refresh
	switch run.State {
	case modmgr.RunSucceeded:
		d.StateLabel, d.StateClass = s.msg("rn.succeeded"), "ok"
	case modmgr.RunFailed:
		d.StateLabel, d.StateClass = s.msg("rn.failed"), "err"
	default:
		d.StateLabel, d.StateClass = s.msg("rn.running"), "warn"
	}
	for _, m := range run.Modules {
		ov := outcomeView{ID: m.ID, LastError: m.LastError, ActionLabel: s.msg(actionLabels[reconcile.Kind(m.Action)])}
		switch m.State {
		case store.StateEnabled:
			ov.StateLabel, ov.StateClass = s.msg("state.enabled"), "ok"
		case store.StateFailed:
			ov.StateLabel, ov.StateClass = s.msg("state.failed"), "err"
		case store.StateDisabled:
			ov.StateLabel, ov.StateClass = s.msg("state.disabled"), "neutral"
		default:
			ov.StateLabel, ov.StateClass = s.msg("rn.pending"), "neutral"
		}
		d.Modules = append(d.Modules, ov)
	}
	return d
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	run, err := s.Mod.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failMod(w, r, sc, err)
		return
	}
	d := s.runView(run, r.URL.Query().Get("refresh") != "0")
	s.render(w, r, http.StatusOK, "run.html", s.page(r, sc, "catalogue", s.msg("rn.title"), d))
}

// --- status -------------------------------------------------------------------------------------

type healthModule struct {
	modmgr.ModuleHealth
	ReadyLabel, ReadyClass string
}

type statusData struct {
	Modules     []healthModule
	Reconcile   *modmgr.ReconcileInfo
	ReconcileOK bool
	Certs       []certView
	CertsNote   bool
	Disk        []modmgr.DiskUsage
	DiskNote    bool
	LatestRun   string
	Footprint   fpView
}

type certView struct {
	modmgr.CertStatus
	StateLabel, StateClass string
}

var readyLabels = map[string][2]string{
	modmgr.ReadyYes: {"hs.ready_yes", "ok"}, modmgr.ReadyNo: {"hs.ready_no", "err"},
	modmgr.ReadyOff: {"hs.ready_off", "neutral"}, modmgr.ReadyUnknown: {"hs.ready_unknown", "neutral"},
}

var certLabels = map[string][2]string{
	"valid": {"hs.cert_valid", "ok"}, "pending": {"hs.cert_pending", "warn"},
	"failed": {"hs.cert_failed", "err"}, "unknown": {"hs.cert_unknown", "neutral"},
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	h, err := s.Mod.Status(r.Context())
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := statusData{Reconcile: h.Reconcile, Disk: h.Disk, CertsNote: h.CertsNote != "", DiskNote: h.DiskNote != ""}
	d.ReconcileOK = h.Reconcile != nil && h.Reconcile.OK
	for _, m := range h.Modules {
		l := readyLabels[m.Ready]
		d.Modules = append(d.Modules, healthModule{ModuleHealth: m, ReadyLabel: s.msg(l[0]), ReadyClass: l[1]})
	}
	for _, c := range h.Certs {
		l, ok := certLabels[c.State]
		if !ok {
			l = certLabels["unknown"]
		}
		d.Certs = append(d.Certs, certView{CertStatus: c, StateLabel: s.msg(l[0]), StateClass: l[1]})
	}
	if run, err := s.Mod.LatestRun(r.Context()); err == nil {
		d.LatestRun = run.ID
	}
	s.render(w, r, http.StatusOK, "status.html", s.page(r, sc, "status", s.msg("hs.title"), d))
}

// --- company.yaml -------------------------------------------------------------------------------

type companyData struct {
	Version, Content string
}

func (s *Server) companyPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	cur, err := s.Mod.Company.Get(r.Context())
	if err != nil {
		s.failMod(w, r, sc, err)
		return
	}
	s.render(w, r, http.StatusOK, "company.html", s.page(r, sc, "catalogue", s.msg("cy.title"), companyData{Version: cur.Version, Content: string(cur.Data)}))
}

// --- purge --------------------------------------------------------------------------------------

type purgeData struct {
	ID, Name, Expect, Error string
	HasPurger               bool
}

func (s *Server) renderPurge(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, errKey string) {
	id := r.PathValue("id")
	cat, err := s.Mod.Catalogue()
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	m, err := s.DB.GetModule(r.Context(), id)
	if err != nil || m.State != store.StateDisabled {
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.purge_not_disabled")
		return
	}
	d := purgeData{ID: id, Name: m.Manifest.Name, Expect: "purge " + id, HasPurger: s.Mod.Purger != nil}
	if e := cat.Get(id); e != nil && e.Manifest != nil {
		d.Name = e.Manifest.Name
	}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	s.render(w, r, status, "purge.html", s.page(r, sc, "catalogue", s.msg("pg.title"), d))
}

func (s *Server) purgePage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.renderPurge(w, r, sc, http.StatusOK, "")
}

func (s *Server) purgePost(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	// Typed confirmation, the current password and a distinct permission: three separate gates for
	// the one action that cannot be undone.
	if err := s.Auth.ConfirmPassword(r.Context(), sc.User, r.PostFormValue("password")); err != nil {
		if status, key, ok := accountError(w, err); ok {
			s.renderPurge(w, r, sc, status, key)
			return
		}
		s.internal(w, r, sc, err)
		return
	}
	err := s.Mod.Purge(r.Context(), sc.User.Username, r.PathValue("id"), r.PostFormValue("confirm"))
	switch {
	case err == nil:
		s.redirect(w, r, "/catalogue", "flash.purged")
	case errors.Is(err, modmgr.ErrConfirmation):
		s.renderPurge(w, r, sc, http.StatusBadRequest, "pg.mismatch")
	case errors.Is(err, modmgr.ErrNoPurger):
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.no_purger")
	case errors.Is(err, modmgr.ErrNotPurgeable):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.purge_not_disabled")
	default:
		s.Log.Error("purge failed", "request_id", httpx.RequestID(r.Context()), "module", r.PathValue("id"), "error", err)
		s.fail(w, r, sc, http.StatusBadGateway, "err.purge_failed_title", "err.purge_failed")
	}
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

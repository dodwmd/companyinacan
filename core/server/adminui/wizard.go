package adminui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

// The onboarding wizard (epic E12b): company, template, modules, email relay, DNS, account
// hardening, review and apply. State lives in can-core's database (resumable, one per
// installation); every step saves on its own and is safe to submit twice. Permission: platform.setup
// (owner only by default) because it changes the company identity, domain and relay credentials.

func (s *Server) wizardRoutes() []route {
	return []route{
		{"GET /setup", s.authedPerm(rbac.PermSetup, s.mod(s.setupHome))},
		{"GET /setup/{step}", s.authedPerm(rbac.PermSetup, s.mod(s.setupStep))},
		{"POST /setup/{step}", s.authedPerm(rbac.PermSetup, s.mod(s.setupPost))},
	}
}

// wizardGate sends an owner who has not finished (or dismissed) the wizard to it on the dashboard.
func (s *Server) wizardGate(h func(http.ResponseWriter, *http.Request, *sessionCtx)) func(http.ResponseWriter, *http.Request, *sessionCtx) {
	return func(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
		if s.Mod != nil {
			if ok, err := s.allowed(r, sc, rbac.PermSetup); err == nil && ok {
				if need, err := s.Mod.WizardNeeded(r.Context()); err == nil && need {
					http.Redirect(w, r, "/setup", http.StatusSeeOther)
					return
				}
			}
		}
		h(w, r, sc)
	}
}

var wizardSteps = []string{"company", "template", "modules", "email", "dns", "account", "review"}

var stepLabels = map[string]string{
	"company": "wz.step_company", "template": "wz.step_template", "modules": "wz.step_modules", "email": "wz.step_email",
	"dns": "wz.step_dns", "account": "wz.step_account", "review": "wz.step_review",
}

type stepView struct {
	ID, Label     string
	Done, Current bool
	HasStatus     bool
}

type bundleView struct {
	ID, Label string
	Selected  bool
	Modules   []string
	Skipped   []skippedView
}

type skippedView struct{ ID, Reason string }

type setupData struct {
	Step, StepLabel, Next, Prev          string
	Steps                                []stepView
	Status                               string
	Open                                 bool
	Error                                string
	Name, Domain                         string
	ServerRAMMB                          string
	Bundles                              []bundleView
	Template                             string
	Rows                                 []catRow
	Footprint                            fpView
	Relay                                modmgr.RelayInfo
	DNS                                  modmgr.DNSPlan
	InstantLabel, InstantClass           string
	PasswordRotated, TOTPRotated, KitAck bool
	Username                             string
	Review                               changeData
	ReviewEmpty                          bool
	CanApply                             bool
	RunID                                string
}

func (s *Server) setupHome(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	http.Redirect(w, r, "/setup/company", http.StatusSeeOther)
}

func (s *Server) setupSteps(r *http.Request, sc *sessionCtx, ws modmgr.WizardState, current string) []stepView {
	h, _ := s.Mod.AccountHardening(r.Context(), sc.User.Username)
	var out []stepView
	for _, id := range wizardSteps {
		sv := stepView{ID: id, Label: s.msg(stepLabels[id]), Current: id == current}
		switch id {
		case "company":
			sv.HasStatus, sv.Done = true, ws.Data.Name != "" && ws.Data.Domain != ""
		case "template":
			sv.HasStatus, sv.Done = true, ws.Data.Template != ""
		case "modules":
			sv.HasStatus, sv.Done = true, len(ws.Data.Modules) > 0 || ws.Data.Template == "blank"
		case "email":
			sv.HasStatus, sv.Done = true, ws.Data.Relay.Configured
		case "account":
			sv.HasStatus, sv.Done = true, h.PasswordRotated && h.TOTPRotated && ws.Data.KitAck
		}
		out = append(out, sv)
	}
	return out
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, step, errKey string) {
	ctx := r.Context()
	if !contains(wizardSteps, step) {
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	ws, err := s.Mod.WizardState(ctx)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := setupData{Step: step, StepLabel: s.msg(stepLabels[step]), Status: ws.Status, Open: ws.Status == modmgr.WizardOpen, Username: sc.User.Username,
		Name: ws.Data.Name, Domain: ws.Data.Domain, Template: ws.Data.Template, Relay: ws.Data.Relay, KitAck: ws.Data.KitAck}
	if ws.Data.ServerRAMMB > 0 {
		d.ServerRAMMB = strconv.Itoa(ws.Data.ServerRAMMB)
	}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	d.Steps = s.setupSteps(r, sc, ws, step)
	for i, id := range wizardSteps {
		if id == step {
			if i > 0 {
				d.Prev = wizardSteps[i-1]
			}
			if i+1 < len(wizardSteps) {
				d.Next = wizardSteps[i+1]
			}
		}
	}
	switch step {
	case "template":
		for _, b := range s.Mod.BundleList() {
			bv := bundleView{ID: b.ID, Label: s.msg(bundleLabel(b.ID)), Selected: ws.Data.Template == b.ID}
			sel, skipped, err := s.Mod.BundleModules(ctx, b.ID)
			if err != nil {
				s.internal(w, r, sc, err)
				return
			}
			bv.Modules = sel
			for _, k := range skipped {
				bv.Skipped = append(bv.Skipped, skippedView{ID: k.ID, Reason: skipReason(s, k.Reason)})
			}
			d.Bundles = append(d.Bundles, bv)
		}
	case "modules":
		if !s.setupModules(w, r, sc, ws, &d) {
			return
		}
	case "dns":
		plan, err := s.Mod.DNS(ctx)
		if err != nil && !errors.Is(err, modmgr.ErrNoCompany) {
			s.internal(w, r, sc, err)
			return
		}
		d.DNS = plan
		switch plan.Instant {
		case modmgr.InstantServing:
			d.InstantLabel, d.InstantClass = s.msg("wz.instant_serving"), "ok"
		case modmgr.InstantUnreachable:
			d.InstantLabel, d.InstantClass = s.msg("wz.instant_unreachable"), "warn"
		default:
			d.InstantLabel, d.InstantClass = s.msg("wz.instant_unknown"), "neutral"
		}
	case "account":
		h, err := s.Mod.AccountHardening(ctx, sc.User.Username)
		if err != nil {
			s.internal(w, r, sc, err)
			return
		}
		d.PasswordRotated, d.TOTPRotated = h.PasswordRotated, h.TOTPRotated
	case "review":
		pv, err := s.Mod.WizardPreview(ctx)
		switch {
		case errors.Is(err, modmgr.ErrNoChange):
			d.ReviewEmpty, d.CanApply = true, d.Open
		case err != nil:
			s.failMod(w, r, sc, err)
			return
		default:
			d.Review = s.previewData(pv)
			d.CanApply = d.Open && pv.Err() == nil
		}
		if ws.ChangeID != "" && ws.Status == modmgr.WizardCompleted {
			if ch, err := s.Mod.GetChange(ctx, ws.ChangeID); err == nil {
				d.RunID = ch.RunID
			}
		}
	}
	s.render(w, r, status, "setup.html", s.page(r, sc, "setup", d.StepLabel, d))
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func bundleLabel(id string) string {
	switch id {
	case "starter":
		return "wz.bundle_starter"
	case "blank":
		return "wz.bundle_blank"
	}
	return "wz.bundle_other"
}

func skipReason(s *Server, reason string) string {
	if reason == modmgr.SkipNotDeployable {
		return s.msg("wz.skip_not_deployable")
	}
	return s.msg("wz.skip_unknown")
}

// setupModules fills the module-selection step. It returns false after answering an error.
func (s *Server) setupModules(w http.ResponseWriter, r *http.Request, sc *sessionCtx, ws modmgr.WizardState, d *setupData) bool {
	cat, err := s.Mod.Catalogue()
	if err != nil {
		s.internal(w, r, sc, err)
		return false
	}
	sel := map[string]bool{}
	for _, m := range ws.Data.Modules {
		sel[m] = true
	}
	acks := map[string]bool{}
	for _, a := range ws.Data.Acks {
		acks[a] = true
	}
	var mods []*config.Module
	for _, e := range cat.Entries {
		row := s.catRowFor(e, cat, sel, nil, nil, false)
		row.Enabled = sel[e.ID]
		row.AckLicence, row.AckExperimental = acks["licence:"+e.ID], acks["experimental:"+e.ID]
		d.Rows = append(d.Rows, row)
		if sel[e.ID] && e.Manifest != nil {
			mods = append(mods, e.Manifest)
		}
	}
	d.Footprint = s.fpView(modmgr.Footprint(mods, ws.Data.ServerRAMMB))
	return true
}

func (s *Server) setupStep(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.renderSetup(w, r, sc, http.StatusOK, r.PathValue("step"), "")
}

func (s *Server) setupPost(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	ctx, actor, step := r.Context(), sc.User.Username, r.PathValue("step")
	back := func(flash string) { s.redirect(w, r, "/setup/"+step, flash) }
	bad := func(err error, key string) {
		switch {
		case errors.Is(err, modmgr.ErrInvalid), errors.Is(err, modmgr.ErrNotDeployable):
			s.renderSetup(w, r, sc, http.StatusBadRequest, step, key)
		default:
			s.failMod(w, r, sc, err)
		}
	}
	switch step {
	case "company":
		ram := 0
		if v := strings.TrimSpace(r.PostFormValue("server_ram_mb")); v != "" {
			var err error
			if ram, err = strconv.Atoi(v); err != nil {
				s.renderSetup(w, r, sc, http.StatusBadRequest, step, "wz.err_company")
				return
			}
		}
		_, err := s.Mod.WizardUpdate(ctx, actor, func(d *modmgr.WizardData) error {
			d.Name, d.Domain, d.ServerRAMMB = strings.TrimSpace(r.PostFormValue("name")), strings.ToLower(strings.TrimSpace(r.PostFormValue("domain"))), ram
			return nil
		})
		if err != nil {
			bad(err, "wz.err_company")
			return
		}
		back("flash.saved")
	case "template":
		id := r.PostFormValue("template")
		sel, _, err := s.Mod.BundleModules(ctx, id)
		if err != nil {
			s.renderSetup(w, r, sc, http.StatusBadRequest, step, "wz.err_template")
			return
		}
		_, err = s.Mod.WizardUpdate(ctx, actor, func(d *modmgr.WizardData) error {
			keep := map[string]bool{}
			for _, m := range sel { // a template adds modules; it never switches one off
				keep[m] = true
			}
			if cur, gerr := s.Mod.Company.Get(ctx); gerr == nil {
				if c, perr := modmgr.ParseCompany(cur.Data); perr == nil {
					for m := range modmgr.EnabledModules(c) {
						keep[m] = true
					}
				}
			}
			d.Template, d.Modules, d.Acks = id, sortedKeys(keep), nil
			return nil
		})
		if err != nil {
			bad(err, "wz.err_template")
			return
		}
		back("flash.saved")
	case "modules":
		_, err := s.Mod.WizardUpdate(ctx, actor, func(d *modmgr.WizardData) error {
			d.Modules, d.Acks = append([]string(nil), r.PostForm["enable"]...), boundedAcks(r.PostForm["ack"])
			return nil
		})
		if err != nil {
			bad(err, "wz.err_modules")
			return
		}
		back("flash.saved")
	case "email":
		port, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port")))
		err := s.Mod.SetRelay(ctx, actor, modmgr.RelayInput{
			Host: strings.TrimSpace(r.PostFormValue("host")), Port: port, Username: strings.TrimSpace(r.PostFormValue("username")),
			Password: r.PostFormValue("password"), From: strings.TrimSpace(r.PostFormValue("from")),
		})
		if err != nil {
			if errors.Is(err, modmgr.ErrInvalid) {
				s.renderSetup(w, r, sc, http.StatusBadRequest, step, "wz.err_email")
				return
			}
			s.Log.Error("relay secret failed", "error", err)
			s.fail(w, r, sc, http.StatusBadGateway, "err.relay_failed_title", "err.relay_failed")
			return
		}
		back("flash.relay_saved")
	case "account":
		_, err := s.Mod.WizardUpdate(ctx, actor, func(d *modmgr.WizardData) error {
			d.KitAck = r.PostFormValue("kit") != ""
			return nil
		})
		if err != nil {
			bad(err, "wz.err_company")
			return
		}
		back("flash.saved")
	case "apply":
		run, err := s.Mod.WizardApply(ctx, actor, boundedAcks(r.PostForm["ack"])...)
		switch {
		case errors.Is(err, modmgr.ErrNoChange):
			s.redirect(w, r, "/", "flash.wizard_done")
		case err != nil:
			s.failMod(w, r, sc, err)
		default:
			http.Redirect(w, r, "/runs/"+run.ID, http.StatusSeeOther)
		}
	case "dismiss":
		if err := s.Mod.WizardDismiss(ctx, actor); err != nil {
			s.failMod(w, r, sc, err)
			return
		}
		s.redirect(w, r, "/", "flash.wizard_dismissed")
	default:
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
	}
}

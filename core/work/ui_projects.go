package work

import (
	"errors"
	"net/http"
	"strings"
)

// Creating, editing, archiving and staffing projects in the web UI (specs/065-work-manager). Every
// action is the same Store call the REST API makes, with the signed-in caller's own Principal.

type uiProjectInput struct{ Key, Name, Description string }

type uiProjectNewView struct {
	In       uiProjectInput
	Error    string
	Problems []ProblemField
}

type uiProjectSettingsView struct {
	Project  *Project
	In       uiProjectInput
	Members  []Member
	Error    string
	Problems []ProblemField
}

func (c *uiCtx) description() string {
	return strings.TrimSpace(strings.ReplaceAll(c.r.PostFormValue("description"), "\r\n", "\n"))
}

// formProblem reports whether err is something the person can fix on the form, and how to say it.
func formProblem(err error) (status int, message string, probs []ProblemField, ok bool) {
	var ie *InvalidError
	switch {
	case errors.As(err, &ie):
		return http.StatusUnprocessableEntity, msg("err.invalid_msg"), ie.Problems, true
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, conflictText(err), nil, true
	}
	return 0, "", nil, false
}

func (u *webUI) newProjectPage(c *uiCtx) {
	// Only a caller that can reach every project may create one (Store.CreateProject).
	if !c.p.Unscoped() {
		c.errorPage(http.StatusForbidden, "err.forbidden", "err.not_allowed")
		return
	}
	u.renderNewProject(c, http.StatusOK, &uiProjectNewView{})
}

func (u *webUI) renderNewProject(c *uiCtx, status int, v *uiProjectNewView) {
	b := c.base(msg("project.new"), "projects")
	b.D = v
	c.render(status, "projectnew", b)
}

func (u *webUI) createProject(c *uiCtx) {
	in := uiProjectInput{Key: strings.ToUpper(c.formStr("key")), Name: c.formStr("name"), Description: c.description()}
	_, err := u.a.store.CreateProject(c.r.Context(), c.p, NewProject{Key: in.Key, Name: in.Name, Description: in.Description})
	if err != nil {
		if status, message, probs, ok := formProblem(err); ok {
			u.renderNewProject(c, status, &uiProjectNewView{In: in, Error: message, Problems: probs})
			return
		}
		c.fail(err, "/ui/projects/new")
		return
	}
	c.redirect("/ui/projects/"+in.Key, "project_created")
}

func (u *webUI) projectSettings(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	u.renderSettings(c, http.StatusOK, key, nil)
}

// renderSettings shows the settings page; failed, when set, carries a rejected form's values and why.
func (u *webUI) renderSettings(c *uiCtx, status int, key string, failed *uiProjectSettingsView) {
	proj, err := u.a.store.GetProject(c.r.Context(), c.p, key)
	if err != nil {
		c.fail(err, "/ui/")
		return
	}
	members, err := u.a.store.ListMembers(c.r.Context(), c.p, key)
	if err != nil {
		c.fail(err, "/ui/projects/"+key)
		return
	}
	v := &uiProjectSettingsView{Project: proj, Members: members, In: uiProjectInput{Key: key, Name: proj.Name, Description: proj.Description}}
	if failed != nil {
		v.Error, v.Problems = failed.Error, failed.Problems
		if failed.In.Key != "" {
			v.In = failed.In
		}
	}
	b := c.base(msg("settings.title", proj.Key), "projects")
	b.Project, b.ProjectTab, b.D = proj.Key, "settings", v
	c.render(status, "settings", b)
}

// settingsDone redirects after a settings post, or shows the settings page with what was wrong.
func (u *webUI) settingsDone(c *uiCtx, key string, err error, in uiProjectInput, flash string) {
	back := "/ui/projects/" + key + "/settings"
	if err == nil {
		c.redirect(back, flash)
		return
	}
	if status, message, probs, ok := formProblem(err); ok {
		u.renderSettings(c, status, key, &uiProjectSettingsView{In: in, Error: message, Problems: probs})
		return
	}
	c.fail(err, back)
}

func (u *webUI) saveSettings(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	in := uiProjectInput{Key: key, Name: c.formStr("name"), Description: c.description()}
	_, err := u.a.store.UpdateProject(c.r.Context(), c.p, key, ProjectPatch{Name: &in.Name, Description: &in.Description})
	u.settingsDone(c, key, err, in, "project_saved")
}

func (u *webUI) archiveProject(c *uiCtx) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	v := c.formStr("archived")
	if v != "0" && v != "1" {
		c.errorPage(http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
		return
	}
	archived := v == "1"
	flash := "project_restored"
	if archived {
		flash = "project_archived"
	}
	_, err := u.a.store.UpdateProject(c.r.Context(), c.p, key, ProjectPatch{Archived: &archived})
	u.settingsDone(c, key, err, uiProjectInput{}, flash)
}

func (u *webUI) memberAdd(c *uiCtx)    { u.member(c, true, "member_added") }
func (u *webUI) memberRemove(c *uiCtx) { u.member(c, false, "member_removed") }

func (u *webUI) member(c *uiCtx, on bool, flash string) {
	key, ok := c.projectKey()
	if !ok {
		return
	}
	err := u.a.store.SetMember(c.r.Context(), c.p, key, c.formStr("subject"), on)
	u.settingsDone(c, key, err, uiProjectInput{}, flash)
}

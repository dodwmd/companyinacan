package adminui

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/users"
)

// People's access (E11e, specs/044-users-roles): users, their roles, sessions, tokens and identities,
// the step-up page and the public one-time set-up page. Every decision is made by core/server/users;
// these handlers read forms and render.

func (s *Server) userRoutes() []route {
	return []route{
		{"GET /users", s.authed(s.usersPage)},
		{"POST /users", s.authed(s.userCreate)},
		{"GET /users/{username}", s.authed(s.userPage)},
		{"POST /users/{username}/link", s.authed(s.userLink)},
		{"POST /users/{username}/disable", s.userAction("flash.user_disabled", func(r *http.Request, a users.Actor, name string) error {
			_, err := s.users().Disable(r.Context(), a, name)
			return err
		})},
		{"POST /users/{username}/enable", s.userAction("flash.user_enabled", func(r *http.Request, a users.Actor, name string) error {
			return s.users().Enable(r.Context(), a, name)
		})},
		{"POST /users/{username}/delete", s.authed(s.userDelete)},
		{"POST /users/{username}/roles/grant", s.userAction("flash.role_granted", func(r *http.Request, a users.Actor, name string) error {
			return s.users().Grant(r.Context(), a, name, strings.TrimSpace(r.PostFormValue("role")), strings.TrimSpace(r.PostFormValue("scope")))
		})},
		{"POST /users/{username}/roles/revoke", s.userAction("flash.role_revoked", func(r *http.Request, a users.Actor, name string) error {
			return s.users().Revoke(r.Context(), a, name, r.PostFormValue("role"), r.PostFormValue("scope"))
		})},
		{"POST /users/{username}/sessions/revoke", s.userAction("flash.session_revoked", func(r *http.Request, a users.Actor, name string) error {
			return s.users().RevokeSessions(r.Context(), a, name)
		})},
		{"POST /users/{username}/sessions/{sid}/revoke", s.userAction("flash.session_revoked", func(r *http.Request, a users.Actor, name string) error {
			return s.users().RevokeSession(r.Context(), a, name, r.PathValue("sid"))
		})},
		{"POST /users/{username}/tokens/{id}/revoke", s.userAction("flash.token_revoked", func(r *http.Request, a users.Actor, name string) error {
			return s.users().RevokeToken(r.Context(), a, name, r.PathValue("id"))
		})},
		{"POST /users/{username}/sso/link", s.userAction("flash.sso_linked", func(r *http.Request, a users.Actor, name string) error {
			id, err := strconv.ParseInt(r.PostFormValue("identity"), 10, 64)
			if err != nil {
				return users.ErrInvalid
			}
			return s.users().LinkIdentity(r.Context(), a, id, name)
		})},
		{"POST /users/{username}/sso/unlink", s.userAction("flash.sso_unlinked", func(r *http.Request, a users.Actor, _ string) error {
			id, err := strconv.ParseInt(r.PostFormValue("identity"), 10, 64)
			if err != nil {
				return users.ErrInvalid
			}
			return s.users().UnlinkIdentity(r.Context(), a, id)
		})},
		{"POST /access/roles/{role}/override", s.authed(s.roleOverride)},
		{"GET /reauth", s.authed(s.reauthPage)},
		{"POST /reauth", s.authed(s.reauthPost)},
		{"GET " + users.SetupPath, s.setupLinkPage},
		{"POST " + users.SetupPath, s.setupLinkPost},
	}
}

// users is the user administration service: the one can-core wired, or one over this server's own
// dependencies (without e-mail).
func (s *Server) users() *users.Service {
	if s.Users != nil {
		return s.Users
	}
	return &users.Service{DB: s.DB, Audit: s.Audit, Auth: s.Auth, RBAC: s.RBAC, PATs: s.PATs, SSO: s.SSO, Now: s.Now, Log: s.Log}
}

func actorOf(sc *sessionCtx) users.Actor { return users.Actor{User: sc.User, Session: sc.Session} }

// usersFail renders the answer for a refusal of the users service. back is the page to return to,
// which a step-up leads back to.
func (s *Server) usersFail(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error, back string) {
	detail := func(status int, heading string) {
		d := errorData{Heading: s.msg(heading), Message: err.Error(), RequestID: httpx.RequestID(r.Context()), Home: back, HomeLabel: s.msg("us.back")}
		s.render(w, r, status, "error.html", s.page(r, sc, "", d.Heading, d))
	}
	switch {
	case errors.Is(err, users.ErrStepUp):
		http.Redirect(w, r, "/reauth?next="+url.QueryEscape(back), http.StatusSeeOther)
	case errors.Is(err, users.ErrForbidden):
		detail(http.StatusForbidden, "err.forbidden")
	case errors.Is(err, users.ErrNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.user_not_found", "err.not_found")
	case errors.Is(err, users.ErrLastOwner):
		s.fail(w, r, sc, http.StatusConflict, "err.conflict", "err.last_owner")
	case errors.Is(err, users.ErrExists):
		detail(http.StatusConflict, "err.conflict")
	case errors.Is(err, users.ErrInvalid), errors.Is(err, users.ErrSelf):
		detail(http.StatusBadRequest, "err.bad_request")
	case errors.Is(err, users.ErrUnavailable):
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
	default:
		s.internal(w, r, sc, err)
	}
}

// userAction is a CSRF-protected POST about one user that ends with a redirect to their page.
func (s *Server) userAction(flash string, act func(*http.Request, users.Actor, string) error) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
		name := r.PathValue("username")
		back := "/users/" + url.PathEscape(name)
		if !usernameOK(name) {
			s.usersFail(w, r, sc, users.ErrNotFound, "/users")
			return
		}
		if err := act(r, actorOf(sc), name); err != nil {
			s.usersFail(w, r, sc, err, back)
			return
		}
		s.redirect(w, r, back, flash)
	})
}

type usersData struct {
	Rows      []userRow
	Recent    bool
	CanMail   bool
	StepUpURL string
}

type userRow struct {
	Username, Email, Status, StatusClass, Source, Roles string
}

var userStatus = map[string]struct{ key, class string }{
	users.StatusActive: {"us.st_active", "ok"}, users.StatusInvited: {"us.st_invited", "warn"}, users.StatusDisabled: {"us.st_disabled", "neutral"},
}

func (s *Server) canMail() bool {
	u := s.users()
	return u.Mail != nil && u.BaseURL != "" && u.Mail.Ready()
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	list, err := s.users().List(r.Context(), actorOf(sc))
	if err != nil {
		s.usersFail(w, r, sc, err, "/")
		return
	}
	d := usersData{Recent: s.users().Recent(actorOf(sc)), CanMail: s.canMail(), StepUpURL: "/reauth?next=/users"}
	for _, x := range list {
		st := userStatus[x.Status]
		d.Rows = append(d.Rows, userRow{Username: x.Username, Email: x.Email, Status: s.msg(st.key), StatusClass: st.class, Source: x.Source, Roles: strings.Join(x.Roles, ", ")})
	}
	s.render(w, r, http.StatusOK, "users.html", s.page(r, sc, "users", s.msg("us.title"), d))
}

type linkData struct {
	users.Link
	Expires time.Time
}

func (s *Server) showLink(w http.ResponseWriter, r *http.Request, sc *sessionCtx, l users.Link) {
	s.render(w, r, http.StatusOK, "userlink.html", s.page(r, sc, "users", s.msg("us.link_title"), linkData{Link: l, Expires: l.ExpiresAt}))
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	l, err := s.users().Create(r.Context(), actorOf(sc), r.PostFormValue("username"), r.PostFormValue("email"), r.PostFormValue("send") != "")
	if err != nil {
		s.usersFail(w, r, sc, err, "/users")
		return
	}
	s.showLink(w, r, sc, l)
}

func (s *Server) userLink(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	name := r.PathValue("username")
	l, err := s.users().SendLink(r.Context(), actorOf(sc), name, r.PostFormValue("purpose"), r.PostFormValue("email"), r.PostFormValue("send") != "")
	if err != nil {
		s.usersFail(w, r, sc, err, "/users/"+url.PathEscape(name))
		return
	}
	s.showLink(w, r, sc, l)
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	name := r.PathValue("username")
	back := "/users/" + url.PathEscape(name)
	if r.PostFormValue("confirm") != name {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "us.confirm_wrong")
		return
	}
	if err := s.users().Delete(r.Context(), actorOf(sc), name); err != nil {
		s.usersFail(w, r, sc, err, back)
		return
	}
	s.redirect(w, r, "/users", "flash.user_deleted")
}

type userData struct {
	users.Detail
	StatusLabel, StatusClass string
	Recent, CanMail          bool
	StepUpURL, Base          string
}

func (s *Server) userPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	d, err := s.users().Detail(r.Context(), actorOf(sc), r.PathValue("username"))
	if err != nil {
		s.usersFail(w, r, sc, err, "/users")
		return
	}
	base := "/users/" + url.PathEscape(d.Username)
	st := userStatus[d.Status]
	data := userData{Detail: d, StatusLabel: s.msg(st.key), StatusClass: st.class, Recent: s.users().Recent(actorOf(sc)), CanMail: s.canMail(),
		Base: base, StepUpURL: "/reauth?next=" + url.QueryEscape(base)}
	s.render(w, r, http.StatusOK, "user.html", s.page(r, sc, "users", d.Username, data))
}

func (s *Server) roleOverride(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	err := s.users().Override(r.Context(), actorOf(sc), r.PathValue("role"), r.PostFormValue("permission"), r.PostFormValue("effect"))
	if err != nil {
		s.usersFail(w, r, sc, err, "/access")
		return
	}
	s.redirect(w, r, "/access", "flash.override_saved")
}

// --- step-up ------------------------------------------------------------------------------------

type reauthData struct {
	Next, Error, SSOHref string
	Local                bool
}

func (s *Server) renderReauth(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, next, errKey string) {
	d := reauthData{Next: next, Local: sc.User.AuthSource == store.SourceLocal && sc.Session.Source != store.SourceSSO}
	if !d.Local && s.SSO != nil {
		d.SSOHref = "/auth/oidc/login?stepup=1&next=" + url.QueryEscape(next)
	}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	s.render(w, r, status, "reauth.html", s.page(r, sc, "", s.msg("re.title"), d))
}

func (s *Server) reauthPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.renderReauth(w, r, sc, http.StatusOK, sso.SafeNext(r.URL.Query().Get("next")), "")
}

func (s *Server) reauthPost(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	next := sso.SafeNext(r.PostFormValue("next"))
	err := s.Auth.Reauthenticate(r.Context(), sc.Session, sc.User, r.PostFormValue("password"), strings.TrimSpace(r.PostFormValue("totp")), httpx.ClientIP(r, s.TrustProxy))
	switch {
	case err == nil:
		s.redirect(w, r, next, "flash.reauthenticated")
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.renderReauth(w, r, sc, http.StatusUnauthorized, next, "re.invalid")
	case errors.Is(err, auth.ErrUnauthenticated):
		http.SetCookie(w, auth.ClearCookie(s.Secure))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "5")
		s.renderReauth(w, r, sc, http.StatusServiceUnavailable, next, "login.busy")
	default:
		s.internal(w, r, sc, err)
	}
}

// --- the public set-up page ---------------------------------------------------------------------

type setupLinkData struct {
	Token, LoginCSRF, Username, Secret, URI, Error string
	// KeepPassword: the link replaces the authentication code only.
	KeepPassword bool
}

func (s *Server) renderSetupLink(w http.ResponseWriter, r *http.Request, status int, token, csrf, errKey string) {
	setup, err := s.users().OpenLink(r.Context(), token)
	if errors.Is(err, users.ErrBadLink) {
		s.fail(w, r, nil, http.StatusNotFound, "su.bad_title", "su.bad_link")
		return
	}
	if err != nil {
		s.internal(w, r, nil, err)
		return
	}
	d := setupLinkData{Token: token, LoginCSRF: csrf, Username: setup.Username, Secret: setup.Secret,
		URI: auth.TOTPURI(s.msg("app.name"), setup.Username, setup.Secret), KeepPassword: setup.Purpose == store.LinkMFA}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	http.SetCookie(w, s.loginCookie(csrf))
	s.render(w, r, status, "setpassword.html", s.page(r, nil, "", s.msg("su.title"), d))
}

// setupLinkPage only shows the form: opening a link (a mail scanner does) consumes nothing.
func (s *Server) setupLinkPage(w http.ResponseWriter, r *http.Request) {
	csrf := randToken()
	if c, err := r.Cookie(s.loginCookieName()); err == nil && c.Value != "" {
		csrf = c.Value
	}
	s.renderSetupLink(w, r, http.StatusOK, r.URL.Query().Get("token"), csrf, "")
}

func (s *Server) setupLinkPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, httpx.MaxLoginBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, nil, http.StatusBadRequest, "err.bad_request", "err.bad_request_msg")
		return
	}
	token := r.PostFormValue("token")
	c, err := r.Cookie(s.loginCookieName())
	site := r.Header.Get("Sec-Fetch-Site")
	if err != nil || !equalTokens(c.Value, r.PostFormValue("csrf")) || (site != "" && site != "same-origin" && site != "none") {
		s.fail(w, r, nil, http.StatusForbidden, "err.forbidden", "err.forbidden_msg")
		return
	}
	password := r.PostFormValue("password")
	// The confirmation field exists only when a new password is being chosen.
	if confirm, ok := r.PostForm["confirm"]; ok && confirm[0] != password {
		s.renderSetupLink(w, r, http.StatusBadRequest, token, c.Value, "settings.mismatch")
		return
	}
	err = s.users().UseLink(r.Context(), token, password, strings.TrimSpace(r.PostFormValue("totp")), httpx.ClientIP(r, s.TrustProxy))
	switch {
	case err == nil:
		cc := s.loginCookie("")
		cc.MaxAge = -1
		http.SetCookie(w, cc)
		s.redirect(w, r, "/login", "flash.setup_done")
	case errors.Is(err, users.ErrBadLink):
		s.fail(w, r, nil, http.StatusNotFound, "su.bad_title", "su.bad_link")
	case errors.Is(err, users.ErrBadCode):
		s.renderSetupLink(w, r, http.StatusBadRequest, token, c.Value, "settings.bad_code")
	case errors.Is(err, users.ErrBadPassword):
		s.renderSetupLink(w, r, http.StatusUnauthorized, token, c.Value, "su.bad_password")
	case errors.Is(err, auth.ErrWeakPassword):
		s.renderSetupLink(w, r, http.StatusBadRequest, token, c.Value, "settings.weak")
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "5")
		s.renderSetupLink(w, r, http.StatusServiceUnavailable, token, c.Value, "login.busy")
	default:
		s.internal(w, r, nil, err)
	}
}

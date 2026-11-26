package adminui

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/users"
)

// Single sign-on pages and the two OIDC routes (epic E11b, specs/021-oidc-sso).
func (s *Server) ssoRoutes() []route {
	return []route{
		{"GET /auth/oidc/login", s.oidcLogin},
		{"GET /auth/oidc/callback", s.oidcCallback},
		{"GET /settings/sso", s.authedPerm(rbac.PermManage, s.ssoPage)},
		{"POST /settings/sso/test", s.authedPerm(rbac.PermManage, s.ssoTest)},
		{"POST /settings/sso/identities/{id}/link", s.authedPerm(rbac.PermManage, s.ssoLink)},
		{"POST /settings/sso/identities/{id}/unlink", s.authedPerm(rbac.PermManage, s.ssoUnlink)},
	}
}

func (s *Server) ssoNav(r *http.Request, sc *sessionCtx, active string) []navItem {
	if s.SSO == nil {
		return nil
	}
	if ok, err := s.allowed(r, sc, rbac.PermManage); err != nil || !ok {
		return nil
	}
	return []navItem{{Href: "/settings/sso", Label: s.msg("nav.sso"), Current: active == "sso"}}
}

type interstitialData struct{ Heading, Message, URL string }

// interstitial is a page that moves the browser on by itself (meta refresh) and also offers the
// link. It exists because the browser must make the next request as a navigation started by this
// site: a redirect that continues a cross-site navigation (the identity provider's redirect back to
// us) would be treated as cross-site and would not carry the SameSite=Strict session cookie that was
// just set. form-action 'self' also stops a POST from redirecting to the provider's logout.
func (s *Server) interstitial(w http.ResponseWriter, r *http.Request, msgKey, target string) {
	s.render(w, r, http.StatusOK, "interstitial.html", s.page(r, nil, "", s.msg("sso.title"), interstitialData{Heading: s.msg(msgKey), Message: s.msg("sso.continuing"), URL: target}))
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if s.SSO == nil || !s.SSO.Enabled(r.Context()) {
		s.notFound(w, r)
		return
	}
	loc, ck, err := s.SSO.Begin(r.Context(), r.URL.Query().Get("next"), r.URL.Query().Get("stepup") == "1")
	if err != nil {
		s.Log.Error("sso: login could not start", "request_id", httpx.RequestID(r.Context()), "error", err)
		s.renderLogin(w, r, http.StatusServiceUnavailable, randToken(), "", "login.sso_unavailable")
		return
	}
	// Bound the browser's set of transaction cookies: with MaxFlowCookies already present the older
	// ones are expired (an abandoned login leaves one behind for at most ten minutes).
	var old []string
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, sso.StateCookiePrefix) {
			old = append(old, c.Name)
		}
	}
	if len(old) >= sso.MaxFlowCookies {
		for _, n := range old {
			http.SetCookie(w, sso.ClearStateCookie(n, s.Secure))
		}
	}
	http.SetCookie(w, ck)
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.SSO == nil || !s.SSO.Enabled(r.Context()) {
		s.notFound(w, r)
		return
	}
	var state, old string
	cookieName := ""
	if n, ok := sso.FlowCookieName(r.URL.Query().Get("state")); ok {
		cookieName = n
		if c, err := r.Cookie(n); err == nil {
			state = c.Value
		}
	}
	if c, err := r.Cookie(auth.CookieName); err == nil {
		old = c.Value
	}
	if cookieName != "" {
		http.SetCookie(w, sso.ClearStateCookie(cookieName, s.Secure)) // single use, whatever happens next
	}
	res, err := s.SSO.Complete(r.Context(), sso.CompleteRequest{Query: r.URL.Query(), StateCookie: state, IP: httpx.ClientIP(r, s.TrustProxy), OldToken: old})
	var locked *auth.LockedError
	var denied *sso.Denied
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.RetryAfter.Seconds())+1))
		s.renderLogin(w, r, http.StatusTooManyRequests, randToken(), "", "login.locked")
		return
	case errors.Is(err, sso.ErrBusy):
		w.Header().Set("Retry-After", "5")
		s.renderLogin(w, r, http.StatusServiceUnavailable, randToken(), "", "login.busy")
		return
	case errors.As(err, &denied):
		// One message for every refusal; the reason code is in the audit log.
		s.renderLogin(w, r, http.StatusForbidden, randToken(), "", "login.sso_failed")
		return
	case err != nil:
		s.Log.Error("sso: sign-in failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		s.renderLogin(w, r, http.StatusServiceUnavailable, randToken(), "", "login.sso_unavailable")
		return
	}
	http.SetCookie(w, auth.SessionCookie(res.Login.Token, res.Login.Session.ExpiresAt, s.Secure))
	s.interstitial(w, r, "sso.signed_in", sso.SafeNext(res.Next))
}

type ssoPageData struct {
	Error      string
	Status     sso.Status
	Test       *sso.TestResult
	Identities []sso.IdentityView
	CanLink    bool
}

func (s *Server) renderSSO(w http.ResponseWriter, r *http.Request, sc *sessionCtx, status int, errKey string, test *sso.TestResult) {
	if s.SSO == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	st, err := s.SSO.Status(r.Context())
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	ids, err := s.SSO.Identities(r.Context())
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d := ssoPageData{Status: st, Test: test, Identities: ids}
	if s.RBAC != nil {
		d.CanLink, _ = s.RBAC.HasRole(r.Context(), subjectOf(sc), rbac.Owner)
	} else {
		d.CanLink = true
	}
	if errKey != "" {
		d.Error = s.msg(errKey)
	}
	s.render(w, r, status, "sso.html", s.page(r, sc, "sso", s.msg("sso.title"), d))
}

func (s *Server) ssoPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	s.renderSSO(w, r, sc, http.StatusOK, "", nil)
}

func (s *Server) ssoTest(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.SSO == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	res, err := s.SSO.TestDiscovery(r.Context(), sc.User.Username)
	if errors.Is(err, sso.ErrNotConfigured) {
		s.renderSSO(w, r, sc, http.StatusConflict, "sso.not_configured", nil)
		return
	}
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	s.renderSSO(w, r, sc, http.StatusOK, "", &res)
}

func (s *Server) ssoLink(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.SSO == nil {
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	s.ssoResult(w, r, sc, s.users().LinkIdentity(r.Context(), actorOf(sc), id, r.PostFormValue("username")), "flash.sso_linked")
}

func (s *Server) ssoUnlink(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.SSO == nil {
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	s.ssoResult(w, r, sc, s.users().UnlinkIdentity(r.Context(), actorOf(sc), id), "flash.sso_unlinked")
}

func (s *Server) ssoResult(w http.ResponseWriter, r *http.Request, sc *sessionCtx, err error, flash string) {
	switch {
	case err == nil:
		s.redirect(w, r, "/settings/sso", flash)
	case errors.Is(err, users.ErrForbidden):
		s.fail(w, r, sc, http.StatusForbidden, "err.no_permission", "sso.owner_only")
	case errors.Is(err, users.ErrInvalid):
		s.renderSSO(w, r, sc, http.StatusConflict, "sso.link_invalid", nil)
	case errors.Is(err, users.ErrLastOwner):
		s.renderSSO(w, r, sc, http.StatusConflict, "sso.last_owner", nil)
	default:
		s.usersFail(w, r, sc, err, "/settings/sso")
	}
}

package work

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type uiLoginView struct {
	Error string
	Next  string
	// TokenLogin is whether the paste-a-token form is offered (ui.token_login).
	TokenLogin bool
}

// loginPage shows the sign-in form. The CSRF protection of the login form is a double-submit cookie
// (there is no session yet to bind a token to).
func (u *webUI) loginPage(c *uiCtx) {
	next, _ := safeNext(c.r.URL.Query().Get("next"))
	u.renderLogin(c, http.StatusOK, "", next)
}

func (u *webUI) renderLogin(c *uiCtx, status int, errKey, next string) {
	tok := u.sessions.LoginCSRF(c.w, c.r)
	b := c.base(msg("login.title"), "")
	b.CSRF = tok
	v := &uiLoginView{Next: next, TokenLogin: u.a.tokenLogin}
	if errKey != "" {
		v.Error = msg(errKey)
	}
	b.D = v
	c.render(status, "login", b)
}

// loginPost establishes a session from a pasted personal access token. A valid token is never
// throttled; only failures are counted per client address (the same limiter as the API), and an
// address past the limit is answered 429 after a delay. The token is never echoed back, logged or
// put in the cookie, and every login issues a new session id (a cookie sent along is ignored).
func (u *webUI) loginPost(c *uiCtx) {
	r := c.r
	if !u.a.tokenLogin { // pasted-token sign-in is off (ui.token_login: false): launch tickets only
		u.renderLogin(c, http.StatusForbidden, "login.token_off", "")
		return
	}
	if !u.sessions.CheckLoginCSRF(r) {
		u.renderLogin(c, http.StatusForbidden, "login.csrf", "")
		return
	}
	next, _ := safeNext(r.PostFormValue("next"))
	tok := strings.TrimSpace(r.PostFormValue("token"))
	var p *Principal
	var err error
	if tok != "" && len(tok) <= 512 {
		p, err = u.principalFor(r, tok)
	}
	if p == nil || err != nil || !p.Can(PermRead) {
		if ok, wait := u.a.failures.allow(clientIP(r, u.a.trustProxy)); !ok {
			select {
			case <-time.After(u.a.penalty):
			case <-r.Context().Done():
			}
			c.w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			u.renderLogin(c, http.StatusTooManyRequests, "login.locked", next)
			return
		}
		u.renderLogin(c, http.StatusUnauthorized, "login.invalid", next)
		return
	}
	u.sessions.Start(c.w, r, *p, tok)
	if next == "" {
		next = "/ui/"
	}
	http.Redirect(c.w, r, next, http.StatusSeeOther)
}

func (u *webUI) logout(c *uiCtx) {
	u.sessions.End(c.w, c.r)
	http.Redirect(c.w, c.r, "/ui/login?ok=logged_out", http.StatusSeeOther)
}

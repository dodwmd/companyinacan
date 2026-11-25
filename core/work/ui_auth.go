package work

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type uiLoginView struct {
	Error string
	Next  string
}

// loginPage shows the sign-in form. The CSRF protection of the login form is a double-submit cookie
// (there is no session yet to bind a token to).
func (u *webUI) loginPage(c *uiCtx) {
	next, _ := safeNext(c.r.URL.Query().Get("next"))
	u.renderLogin(c, http.StatusOK, "", next)
}

func (u *webUI) renderLogin(c *uiCtx, status int, errKey, next string) {
	tok := randToken(24)
	u.setCookie(c.w, c.r, u.loginCookieName(c.r), tok, 3600)
	b := c.base(msg("login.title"), "")
	b.CSRF = tok
	v := &uiLoginView{Next: next}
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
	ck, err := r.Cookie(u.loginCookieName(r))
	if err != nil || ck.Value == "" || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(r.PostFormValue("csrf"))) != 1 {
		u.renderLogin(c, http.StatusForbidden, "login.csrf", "")
		return
	}
	next, _ := safeNext(r.PostFormValue("next"))
	tok := strings.TrimSpace(r.PostFormValue("token"))
	var p *Principal
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
	if old, err := r.Cookie(u.sessionCookieName(r)); err == nil {
		u.sessions.delete(old.Value)
	}
	id, _ := u.sessions.create(tok, p.Subject)
	u.setCookie(c.w, r, u.sessionCookieName(r), id, int(u.sessions.abs.Seconds()))
	u.setCookie(c.w, r, u.loginCookieName(r), "", -1)
	if next == "" {
		next = "/ui/"
	}
	http.Redirect(c.w, r, next, http.StatusSeeOther)
}

func (u *webUI) logout(c *uiCtx) {
	u.sessions.delete(c.id)
	u.setCookie(c.w, c.r, u.sessionCookieName(c.r), "", -1)
	http.Redirect(c.w, c.r, "/ui/login?ok=logged_out", http.StatusSeeOther)
}

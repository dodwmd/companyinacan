package finance

import (
	"net/url"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
	"github.com/dodwmd/companyinacan/core/uikit/session/conformance"
)

func TestUISecurityConformance(t *testing.T) {
	conformance.Run(t, conformance.Module{
		Name: "finance", SessionCookie: "can_finance_session", CSRFCookie: "can_finance_login_csrf",
		User: []string{PermWrite}, Privileged: []string{PermAdmin},
		Idle: 30 * time.Minute, Absolute: 8 * time.Hour,
		Recheck: 5 * time.Minute, PrivilegedRecheck: time.Minute,
		SSORecheck: 5 * time.Minute, SSOPrivilegedRecheck: time.Minute,
		LoginCSRFMaxAge: 900, FormLimit: 256 << 10, FirstLimited: 11,
		New: func(t *testing.T, secure bool) *conformance.Env {
			core := fakecore.New(t, "finance")
			u := newUIEnv(t, func(c *Config, o *Options) {
				c.Webhook.URL = ""
				c.UI.InsecureCookies = !secure
				o.Can = core.Client(t)
				o.Auth = &CanAuth{In: &modclient.Introspector{API: o.Can, Now: o.Now}}
			})
			now := func() time.Time { u.mu.Lock(); defer u.mu.Unlock(); return u.now }
			core.Now = now
			return &conformance.Env{H: u.h, Core: core, Advance: u.advance}
		},
		StepUp: func(t *testing.T, _ *conformance.Env, b *conformance.Browser) (string, url.Values) {
			w := b.Do("POST", "/parties", url.Values{"csrf": {b.CSRF("/")}, "kind": {"customer"}, "name": {"Acme Pty"}, "currency": {"USD"}})
			if w.Code != 303 {
				t.Fatalf("party: %d %.300s", w.Code, w.Body)
			}
			return "/parties/" + locID(t, w.Header().Get("Location"), "/parties/") + "/erase", url.Values{"confirm": {"ERASE"}}
		},
	})
}

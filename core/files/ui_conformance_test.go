package files

import (
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
	"github.com/dodwmd/companyinacan/core/uikit/session/conformance"
)

func TestUISecurityConformance(t *testing.T) {
	conformance.Run(t, conformance.Module{
		Name: "files", SessionCookie: "can_files_session", CSRFCookie: "can_files_login_csrf",
		User: []string{PermWrite}, Privileged: []string{PermAdmin},
		Idle: 30 * time.Minute, Absolute: 8 * time.Hour,
		Recheck: 5 * time.Minute, PrivilegedRecheck: time.Minute,
		SSORecheck: 5 * time.Minute, SSOPrivilegedRecheck: time.Minute,
		LoginCSRFMaxAge: 900, FormLimit: 64 << 10, FirstLimited: 10,
		New: func(t *testing.T, secure bool) *conformance.Env {
			core := fakecore.New(t, "files")
			e := newEnv(t, func(c *Config, _ *StoreOptions) { c.UI.Enabled, c.UI.InsecureCookies = true, !secure })
			core.Now = e.now
			can := core.Client(t)
			api := &API{Store: e.s, Auth: &CanAuth{In: &modclient.Introspector{API: can, Now: e.now}}, Log: slog.New(slog.DiscardHandler)}
			return &conformance.Env{H: newHandler(api, can), Core: core, Advance: e.advance}
		},
		StepUp: func(*testing.T, *conformance.Env, *conformance.Browser) (string, url.Values) {
			return "/admin/erasure", url.Values{"subject": {"user:ghost"}, "confirm": {"ERASE"}}
		},
	})
}

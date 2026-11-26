package work

import (
	"log/slog"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
	"github.com/dodwmd/companyinacan/core/uikit/session/conformance"
)

func TestUISecurityConformance(t *testing.T) {
	var built *App // the module built last
	conformance.Run(t, conformance.Module{
		Name: "work", Prefix: "/ui", SessionCookie: "work_session", CSRFCookie: "work_login_csrf",
		User: []string{PermWrite}, Privileged: []string{PermAdmin},
		Idle: 8 * time.Hour, Absolute: 24 * time.Hour,
		// a token is authenticated on every request (through the 30 s introspection cache)
		Recheck: 0, PrivilegedRecheck: 0,
		SSORecheck: 30 * time.Second, SSOPrivilegedRecheck: 30 * time.Second,
		LoginCSRFMaxAge: 3600, FormLimit: 512 << 10, FirstLimited: 31,
		New: func(t *testing.T, secure bool) *conformance.Env {
			core := fakecore.New(t, "work")
			cfg := DefaultConfig()
			cfg.DBPath = filepath.Join(t.TempDir(), "work.db")
			cfg.authPenalty = time.Nanosecond
			clk := &clock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
			core.Now = clk.Now
			opt := Options{Now: clk.Now, Logger: slog.New(slog.DiscardHandler), Can: core.Client(t)}
			if secure {
				opt.PublicURL = "https://work.example.test"
			}
			app, err := NewApp(cfg, Secrets{AdminToken: bootstrap}, opt)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.Close() })
			built = app
			for _, a := range app.srv.auth.(Chain) {
				if ca, ok := a.(*CanAuth); ok {
					ca.In.Now = clk.Now
				}
			}
			return &conformance.Env{H: app.Handler(), Core: core, Advance: clk.Advance}
		},
		// no page here is for administrators alone, so the demotion is to read-only and the request
		// is a write
		Demoted: []string{PermRead},
		Gated: func(t *testing.T, _ *conformance.Env, _ *conformance.Browser) (string, string, url.Values) {
			setup := &Principal{Subject: "user:setup", Permissions: []string{PermAdmin}}
			if _, err := built.Store().CreateProject(t.Context(), setup, NewProject{Key: "DEMO", Name: "Demo"}); err != nil {
				t.Fatal(err)
			}
			return "POST", "/projects/DEMO/issues", url.Values{"title": {"x"}, "type": {"task"}}
		},
	})
}

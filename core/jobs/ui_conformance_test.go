package jobs

import (
	"log/slog"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
	"github.com/dodwmd/companyinacan/core/uikit/session/conformance"
)

func TestUISecurityConformance(t *testing.T) {
	conformance.Run(t, conformance.Module{
		Name: "jobs", SessionCookie: "can_jobs_session", CSRFCookie: "can_jobs_login_csrf",
		User:       []string{"ats.interview"},
		Privileged: []string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview"},
		Idle:       30 * time.Minute, Absolute: 8 * time.Hour,
		Recheck: 5 * time.Minute, PrivilegedRecheck: time.Minute,
		SSORecheck: 5 * time.Minute, SSOPrivilegedRecheck: time.Minute,
		LoginCSRFMaxAge: 900, FormLimit: 128 << 10, FirstLimited: 11,
		New: func(t *testing.T, secure bool) *conformance.Env {
			core := fakecore.New(t, "jobs")
			clk := &clock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
			core.Now = clk.Now
			cfg := DefaultConfig()
			dir := t.TempDir()
			cfg.DBPath, cfg.ResumeDir = filepath.Join(dir, "jobs.db"), filepath.Join(dir, "resumes")
			cfg.Company.Name, cfg.Company.BaseURL = "Example Co", "https://example.test"
			cfg.UI.Enabled, cfg.UI.InsecureCookies = true, !secure
			cfg.Approval.Required = false
			kr, err := ParseKeyring("k1 " + testKeyHex + "\n")
			if err != nil {
				t.Fatal(err)
			}
			can := core.Client(t)
			app, err := NewApp(cfg, Secrets{AdminToken: adminTok, HashKey: "hash-key-0123456789abcdef0123456789abcdef", FormSecret: "form-secret-0123456789abcdef", CanSecret: canSecret},
				Options{Logger: slog.New(slog.DiscardHandler), Keyring: kr, Can: can, Now: clk.Now, Auth: &CanAuth{In: &modclient.Introspector{API: can, Now: clk.Now}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.Close() })
			return &conformance.Env{H: app.Handler(), Core: core, Advance: clk.Add}
		},
		Gated: func(*testing.T, *conformance.Env, *conformance.Browser) (string, string, url.Values) {
			return "GET", "/jobs/new", nil
		},
	})
}

package equity

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
	var holder string // a record somebody else registered, in the module built last
	conformance.Run(t, conformance.Module{
		Name: "equity", SessionCookie: "can_equity_session", CSRFCookie: "can_equity_login_csrf",
		User:       []string{"equity.self-read"},
		Privileged: []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read", "equity.admin"},
		Idle:       30 * time.Minute, Absolute: 8 * time.Hour,
		Recheck: 5 * time.Minute, PrivilegedRecheck: time.Minute,
		SSORecheck: 5 * time.Minute, SSOPrivilegedRecheck: time.Minute,
		LoginCSRFMaxAge: 900, FormLimit: 64 << 10, FirstLimited: 11,
		New: func(t *testing.T, secure bool) *conformance.Env {
			now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			core := fakecore.New(t, "equity")
			core.Now = clock
			can := core.Client(t)
			kr, err := ParseKeyring("k1 " + hexKeyA + "\n")
			if err != nil {
				t.Fatal(err)
			}
			s, err := Open(filepath.Join(t.TempDir(), "equity.db"), Options{Keyring: kr, Now: clock})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			board := Actor{Subject: "board-bob", Roles: []string{RoleBoard}}
			ent := m(s.CreateEntity(t.Context(), board, EntityInput{Name: "Example Pty Ltd", Currency: "USD"}))
			hr := Actor{Subject: "hr-hana", Roles: []string{RoleHR}}
			holder = m(s.CreateStakeholder(t.Context(), hr, ent.ID, StakeholderInput{Kind: "advisor", Identity: Identity{Name: "Ada Advisor"}})).ID
			ui := &UI{Svc: s, Auth: &CanAuth{In: &modclient.Introspector{API: can, Now: clock}}, Sessions: can, Secure: secure, Now: clock,
				Log: slog.New(slog.DiscardHandler)}
			return &conformance.Env{H: ui.Handler(), Core: core, Advance: func(d time.Duration) { now = now.Add(d) }}
		},
		// the holder verification queue: not for a holder
		Gated: func(*testing.T, *conformance.Env, *conformance.Browser) (string, string, url.Values) {
			return "GET", "/company/identity", nil
		},
		// attesting a holder cannot be taken back
		StepUp: func(*testing.T, *conformance.Env, *conformance.Browser) (string, url.Values) {
			return "/company/stakeholders/" + holder + "/attest", url.Values{"confirm": {"yes"}}
		},
	})
}

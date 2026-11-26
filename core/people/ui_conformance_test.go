package people

import (
	"log/slog"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
	"github.com/dodwmd/companyinacan/core/uikit/session/conformance"
)

func TestUISecurityConformance(t *testing.T) {
	conformance.Run(t, conformance.Module{
		Name: "people", SessionCookie: "can_people_session", CSRFCookie: "can_people_login_csrf",
		User: []string{"people.read"}, Privileged: []string{"people.read", "people.write", "people.admin"},
		Idle: 30 * time.Minute, Absolute: 8 * time.Hour,
		Recheck: 5 * time.Minute, PrivilegedRecheck: time.Minute,
		SSORecheck: 5 * time.Minute, SSOPrivilegedRecheck: time.Minute,
		LoginCSRFMaxAge: 900, FormLimit: 64 << 10, FirstLimited: 11,
		New: func(t *testing.T, secure bool) *conformance.Env {
			e := newEnv(t)
			now := *e.clock
			clock := func() time.Time { return now }
			core := fakecore.New(t, "people")
			core.Now = clock
			can := core.Client(t)
			for _, n := range []string{"alice", "bob"} {
				_, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: n, FamilyName: "Example", WorkEmail: n + "@example.com", SSOSubject: "user:" + n, StartDate: "2025-01-06"})
				e.must(err)
			}
			ui := &UI{Svc: e.s, Auth: &CanAuth{In: &modclient.Introspector{API: can, Now: clock}}, Sessions: can, Secure: secure, Now: clock,
				Log: slog.New(slog.DiscardHandler)}
			return &conformance.Env{H: ui.Handler(), Core: core, Advance: func(d time.Duration) { now = now.Add(d) }}
		},
	})
}

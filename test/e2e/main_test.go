//go:build e2e

// Package e2e drives the docker compose stack through its Caddy edge, like a browser or a script
// would. Run it with `make e2e` (stack already up) or `make e2e-up e2e`. See specs/032-e2e-docs and
// docs/getting-started.md. Every scenario creates its own uniquely named data and can be repeated.
package e2e

import (
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

var (
	env = harness.FromEnv()
	web = harness.NewClient(env)
)

// Public dev tokens (dev/compose/secrets) and the fake introspection tokens of the e2e overlay
// (dev/compose/config/introstub.json).
const (
	tokHR      = "e2e-intro-hr-token-0123456789abcdef"
	tokManager = "e2e-intro-manager-token-0123456789abcdef"
	tokAnn     = "e2e-intro-ann-token-0123456789abcdef"
	tokBen     = "e2e-intro-ben-token-0123456789abcdef"
	tokBoard   = "e2e-intro-board-token-0123456789abcdef"
)

func TestMain(m *testing.M) {
	// Fail fast with a pointer, instead of 20 scenarios each timing out, when nothing is listening.
	deadline := time.Now().Add(90 * time.Second)
	for {
		r := tryGet("localhost", "/_edge/healthz")
		if r == 200 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "e2e: the edge did not answer at 127.0.0.1:%s within 90s (last status %d). Start the stack with `make e2e-up`.\n", env.Port, r)
			os.Exit(2)
		}
		time.Sleep(2 * time.Second)
	}
	os.Exit(m.Run())
}

func tryGet(host, path string) (status int) {
	defer func() {
		if recover() != nil {
			status = 0
		}
	}()
	t := &quietT{}
	r := web.Do(t, harness.Req{Host: host, Path: path})
	if t.failed {
		return 0
	}
	return r.Status
}

// quietT swallows failures so a probe can retry.
type quietT struct{ failed bool }

func (q *quietT) Helper()             {}
func (q *quietT) Logf(string, ...any) {}
func (q *quietT) Fatalf(string, ...any) {
	q.failed = true
	panic("probe failed")
}

var (
	adminOnce sync.Once
	adminTOTP string
)

// adminSecret rotates the admin TOTP secret once per test process (through `compose exec`, the same
// thing dev/compose/bootstrap-admin.sh does) and returns it. Scenarios share it, so only one login
// per process needs a fresh code.
func adminSecret(t *testing.T) string {
	t.Helper()
	adminOnce.Do(func() {
		pw := env.Secret(t, "core", "CAN_ADMIN_PASSWORD")
		out := env.Run(t, "exec", "-T", "-e", "CAN_ADMIN_PASSWORD="+pw, "core", "/can-core", "admin", "reset-password", "--rotate-totp")
		adminTOTP = harness.ParseTOTPSecret(out)
	})
	if adminTOTP == "" {
		t.Fatalf("could not read a TOTP secret from `can-core admin reset-password --rotate-totp` (is the core container running?)")
	}
	return adminTOTP
}

// adminLogin signs the session in as the admin with password and TOTP. A TOTP code can be used once
// per 30 second step, so when an earlier login in the same step made the server refuse the code,
// it waits for the next step and tries once more.
func adminLogin(t *testing.T, s *harness.Client) *harness.Response {
	t.Helper()
	return adminLoginAt(t, s, "admin.localhost", env.Port)
}

// adminLoginAt is adminLogin at one of the admin UI's addresses.
func adminLoginAt(t *testing.T, s *harness.Client, host, port string) *harness.Response {
	t.Helper()
	return loginAt(t, s, host, port, "admin", adminSecret(t))
}

// loginAt signs an administrator in with the password every dev account has and their TOTP secret.
// It tries again at the next TOTP step, and once after a minute when can-core's per-address limit of
// sign-in attempts (10 a minute) is used up by the scenarios before.
func loginAt(t *testing.T, s *harness.Client, host, port, user, secret string) *harness.Response {
	t.Helper()
	pw := env.Secret(t, "core", "CAN_ADMIN_PASSWORD")
	hdr := map[string]string{"Origin": "http://" + host + ":" + port}
	limited := false
	for attempt := 0; ; attempt++ {
		page := s.Do(t, harness.Req{Host: host, Port: port, Path: "/login"}).Expect(t, "admin login page", 200)
		code, err := harness.TOTP(secret, time.Now())
		if err != nil {
			t.Fatalf("%v", err)
		}
		r := s.Do(t, harness.Req{Method: "POST", Host: host, Port: port, Path: "/login", Headers: hdr,
			Body: url.Values{"csrf": {harness.CSRF(page.String())}, "username": {user}, "password": {pw}, "totp": {code}}})
		wait := time.Duration(31-time.Now().Unix()%30) * time.Second
		switch {
		case r.Status == 429 && !limited:
			limited, wait = true, 61*time.Second
		case r.Status == 303 || attempt >= 1:
			return r.Expect(t, user+" signs in with password and TOTP", 303)
		}
		t.Logf("login answered HTTP %d (a TOTP code is single use per step?); retrying in %s", r.Status, wait)
		time.Sleep(wait)
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("expected a numeric id, got %q", s)
	}
	return n
}

func tokenOf(t *testing.T, file, key string) string { t.Helper(); return env.Secret(t, file, key) }

func workAPI(t *testing.T) harness.API {
	return harness.API{C: web, Host: "work.localhost", Token: tokenOf(t, "work", "CAN_WORK_ADMIN_TOKEN")}
}
func peopleAPI(token string) harness.API {
	return harness.API{C: web, Host: "people.localhost", Token: token}
}
func jobsAPI(t *testing.T) harness.API {
	return harness.API{C: web, Host: "jobs.localhost", Token: tokenOf(t, "jobs", "CAN_JOBS_ADMIN_TOKEN")}
}
func financeAPI(t *testing.T) harness.API {
	return harness.API{C: web, Host: "finance.localhost", Token: tokenOf(t, "finance", "CAN_FINANCE_ADMIN_TOKEN")}
}
func equityAPI(token string) harness.API {
	return harness.API{C: web, Host: "equity.localhost", Token: token}
}

// equityCoreAPI is the e2e overlay's second equity, wired to can-core and the directory only.
func equityCoreAPI(token string) harness.API {
	return harness.API{C: web, Host: equityCore, Token: token}
}

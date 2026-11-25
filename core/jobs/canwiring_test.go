package jobs

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

const (
	jobsPAT   = "canpat_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	canSecret = "can-hook-secret-0123456789abcdef"
)

func TestRoleFromPermissions(t *testing.T) {
	cases := []struct {
		perms []string
		want  string
	}{
		{[]string{"jobs.admin"}, ""}, // admin is the whole set, not one permission
		{[]string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview", "jobs.admin"}, RoleAdmin},
		{[]string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview"}, RoleRecruiter},
		{[]string{"jobs.read", "ats.read", "ats.write", "ats.interview"}, RoleHiringManager},
		// recruiter and admin both score applications (ActScore), so both need ats.interview
		{[]string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire"}, ""},
		{[]string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "jobs.admin"}, ""},
		{[]string{"ats.interview"}, RoleInterviewer},
		{[]string{"jobs.read"}, ""},                  // reading jobs alone is not a role
		{[]string{"jobs.write", "ats.hire"}, ""},     // part of recruiter is not recruiter
		{[]string{"people.admin", "work.admin"}, ""}, // another module's permissions
		{nil, ""},
	}
	for _, c := range cases {
		if got := roleFromPermissions(c.perms); got != c.want {
			t.Errorf("%v -> %q, want %q", c.perms, got, c.want)
		}
	}
}

func canEnv(t *testing.T) (*env, *fakecore.Core) {
	t.Helper()
	e := newEnv(t, nil)
	e.clk.t = time.Now() // the webhook replay window is checked against this clock
	core := fakecore.New(t, "jobs")
	cfg := DefaultConfig()
	dir := t.TempDir()
	cfg.DBPath = filepath.Join(dir, "jobs.db")
	cfg.ResumeDir = filepath.Join(dir, "resumes")
	cfg.Company.Name, cfg.Company.BaseURL = "Example Co", "https://example.test"
	kr, err := ParseKeyring("k1 " + testKeyHex + "\n")
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(cfg, Secrets{AdminToken: adminTok, HashKey: "hash-key-0123456789abcdef0123456789abcdef", FormSecret: "form-secret-0123456789abcdef", CanSecret: canSecret},
		Options{Logger: slog.New(slog.DiscardHandler), Keyring: kr, Can: core.Client(t), Now: e.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	e.app, e.h = app, app.Handler()
	return e, core
}

func TestPersonalAccessTokenGetsTheRoleItsPermissionsCover(t *testing.T) {
	e, core := canEnv(t)
	core.AddToken(jobsPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview"}})
	e.want(e.do(jobsPAT, "POST", "/v1/jobs", jobBody("Engineer", "user:bob")), 201)
	if r := e.do(jobsPAT, "GET", "/v1/tokens", nil); r.Code != 403 {
		t.Fatalf("a recruiter token must not reach admin routes: %d", r.Code)
	}
	core.AddToken(jobsPAT+"I", fakecore.Token{Subject: "user:ivy", Permissions: []string{"ats.interview"}})
	if r := e.do(jobsPAT+"I", "POST", "/v1/jobs", jobBody("Other", "user:bob")); r.Code != 403 {
		t.Fatalf("an interviewer token created a job: %d", r.Code)
	}
	core.AddToken(jobsPAT+"X", fakecore.Token{Subject: "user:x", Permissions: []string{"jobs.read"}}) // covers no role
	if r := e.do(jobsPAT+"X", "GET", "/v1/jobs", nil); r.Code != 401 {
		t.Fatalf("a token covering no role must be refused: %d", r.Code)
	}
}

func TestJobsEventsGoThroughCanCore(t *testing.T) {
	e, core := canEnv(t)
	id, _ := e.openJob("Engineer", "user:bob")
	if id == "" {
		t.Fatal("no job")
	}
	if n, err := e.app.dispatcher.DeliverDue(context.Background()); err != nil || n == 0 {
		t.Fatalf("delivered %d: %v", n, err)
	}
	var opened bool
	for _, ev := range core.Published() {
		opened = opened || ev.Type == "job.opened.v1"
	}
	if !opened {
		t.Fatalf("job.opened.v1 not published: %+v", core.Published())
	}
	_ = time.Second
}

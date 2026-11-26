package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/verify"
)

const verifyFacts = `now=1800000000
nodes=1
nodes_ready=1
ntp_synced=yes
workload=can-system/can-core 1 1
workload=mod-forms/forms 1 0
route=can-system/site|["websecure"]|Host(` + "`acme.example`" + `)
route=can-system/admin|["websecure"]|Host(` + "`admin.acme.example`" + `)
end=ok
`

// noNetwork records what the probes tried to reach.
func noNetwork(t *testing.T) *[]string {
	t.Helper()
	var dialled []string
	old := verifyNet
	verifyNet = func() verify.Net {
		return verify.Net{Now: time.Now, Dial: func(_ context.Context, _, a string) (net.Conn, error) {
			dialled = append(dialled, a)
			return nil, errors.New("refused")
		}}
	}
	t.Cleanup(func() { verifyNet = old })
	return &dialled
}

func verifyArgs(dir string, extra ...string) []string {
	return append([]string{"verify", "--server", "--company-dir", dir, "--modules-dir", "../../modules"}, extra...)
}

func TestVerifyServerDefaultSendsNothingElsewhere(t *testing.T) {
	dir, r := serverCompany(t, "status=ok\n")
	r.mu["can verify: read-only facts"] = verifyFacts
	dialled := noNetwork(t)
	out := filepath.Join(t.TempDir(), "report.json")
	code, stdout, errs := run(t, verifyArgs(dir, "--output", "json", "--out", out)...)
	if code != ExitFailure { // forms is not ready, among others
		t.Fatalf("code=%d out=%q err=%q", code, stdout, errs)
	}
	if len(r.ran) != 2 || len(r.copied) != 0 || len(*dialled) != 0 {
		t.Fatalf("scripts %d, copies %d, connections %v", len(r.ran), len(r.copied), *dialled)
	}
	var rep verify.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(out)
	if err != nil || string(saved) != stdout {
		t.Fatalf("%v: the saved report differs", err)
	}
	for _, bad := range []string{"acme.example", "203.0.113.7", "Acme", "root"} {
		if strings.Contains(stdout, bad) {
			t.Errorf("the report identifies the installation (%q):\n%s", bad, stdout)
		}
	}
	got := map[string]verify.Check{}
	for _, c := range rep.Checks {
		got[c.ID] = c
	}
	if rep.Schema != verify.Schema || rep.Provider != "ssh" || len(rep.Probes) != 0 || rep.Summary.Fail == 0 {
		t.Errorf("%+v", rep)
	}
	for id, want := range map[string]verify.Status{"server.ssh": verify.Pass, "server.clock": verify.Pass, "firewall.node-isolation": verify.Pass,
		"cluster.workloads": verify.Fail, "public.certificates": verify.Skip, "edge.rate-limit": verify.Skip, "edge.client-address": verify.Skip,
		"backup.repository": verify.Skip, "email.relay-tls": verify.Skip, "sso.discovery": verify.Skip} {
		if got[id].Status != want {
			t.Errorf("%s: %+v, want %s", id, got[id], want)
		}
	}
	// The report is never overwritten, and nothing is probed when it cannot be written.
	r.ran = nil
	if code, _, _ := run(t, verifyArgs(dir, "--out", out)...); code != ExitFailure || len(r.ran) != 0 {
		t.Errorf("overwrite: code=%d ran=%d", code, len(r.ran))
	}
}

func TestVerifyServerPublicProbeConnectsOnlyToTheServer(t *testing.T) {
	dir, r := serverCompany(t, "status=ok\n")
	r.mu["can verify: read-only facts"] = verifyFacts
	dialled := noNetwork(t)
	code, out, errs := run(t, verifyArgs(dir, "--probe-public")...)
	if code != ExitFailure || len(*dialled) == 0 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	for _, a := range *dialled {
		if !strings.HasPrefix(a, "203.0.113.7:") {
			t.Errorf("connected to %s", a)
		}
	}
	for _, want := range []string{"FAIL  public.dns", "SKIP  public.closed-ports", "FAIL  public.certificates", "A skip is not a pass"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestVerifyServerUnreachable(t *testing.T) {
	dir, r := serverCompany(t, "")
	dialled := noNetwork(t)
	code, out, _ := run(t, verifyArgs(dir, "--probe-public", "--probe-client-ip", "--probe-rate-limit")...)
	if code != ExitFailure || !strings.Contains(out, "FAIL  server.ssh") || !strings.Contains(out, "1 failed") || len(*dialled) != 0 || len(r.ran) != 1 {
		t.Fatalf("code=%d ran=%d dialled=%v out=%q", code, len(r.ran), *dialled, out)
	}
}

func TestVerifyUsage(t *testing.T) {
	if code, _, errs := run(t, "verify"); code != ExitUsage || !strings.Contains(errs, "--server") {
		t.Errorf("code=%d err=%q", code, errs)
	}
	dir := sshCompany(t)
	useLifecycle(t, func() provider.Deps { return provider.Deps{} }, nil)
	if code, _, errs := run(t, verifyArgs(dir)...); code != ExitFailure || !strings.Contains(errs, "no server recorded") {
		t.Errorf("code=%d err=%q", code, errs)
	}
}

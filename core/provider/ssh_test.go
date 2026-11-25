package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func sshSpec() Spec {
	return Spec{Name: "Acme", Domain: "acme.example", Options: map[string]any{"host": "203.0.113.7", "user": "root"}}
}

func newSSH(r Runner) *SSH { return &SSH{deps: testDeps(r)} }

func TestSSHCreateRunsPreflightThenBootstrap(t *testing.T) {
	r := newFakeRunner(
		fakeRule{contains: "id -u", out: probeGood},
		fakeRule{contains: "bash /usr/local/sbin/can-bootstrap.sh", out: "CAN_STEP: swap\nCAN_BOOTSTRAP_DONE\n"},
	)
	st := NewStore(t.TempDir())
	var out strings.Builder
	if err := newSSH(r).Create(context.Background(), sshSpec(), st, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	script := string(r.Copied["/usr/local/sbin/can-bootstrap.sh"])
	want, _ := Bootstrap(BootstrapOptions{})
	if script != want || r.Modes["/usr/local/sbin/can-bootstrap.sh"] != "0700" {
		t.Fatal("uploaded script differs from generated bootstrap or has wrong mode")
	}
	state, _ := st.Load()
	if state.Server == nil || state.Server.IP != "203.0.113.7" || !state.Done("ssh.bootstrap") || state.K3sVersion != DefaultK3sVersion {
		t.Fatalf("state: %+v", state)
	}
	if !strings.Contains(out.String(), "CAN_BOOTSTRAP_DONE") {
		t.Errorf("remote log not shown:\n%s", out.String())
	}
}

func TestSSHCreateStopsOnFailedPreflight(t *testing.T) {
	bad := strings.Replace(probeGood, "mem_mb=1950", "mem_mb=512", 1)
	r := newFakeRunner(fakeRule{contains: "id -u", out: bad})
	st := NewStore(t.TempDir())
	err := newSSH(r).Create(context.Background(), sshSpec(), st, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "ram") {
		t.Fatalf("%v", err)
	}
	if len(r.Copied) != 0 || len(r.Ran) != 1 {
		t.Fatal("bootstrap ran despite failed preflight")
	}
	if _, serr := os.Stat(st.Path); !os.IsNotExist(serr) {
		t.Fatal("state written before preflight passed")
	}
}

func TestSSHCreateIsIdempotent(t *testing.T) {
	r := newFakeRunner(fakeRule{contains: "id -u", out: probeBootstrapped})
	st := NewStore(t.TempDir())
	var out strings.Builder
	for i := 0; i < 2; i++ {
		if err := newSSH(r).Create(context.Background(), sshSpec(), st, &out); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.Copied) != 0 {
		t.Fatal("re-ran the bootstrap on an already bootstrapped host")
	}
	if !strings.Contains(out.String(), "already installed") {
		t.Errorf("output: %s", out.String())
	}
}

func TestSSHCreateRerunsWhenPinnedVersionChanges(t *testing.T) {
	r := newFakeRunner(
		fakeRule{contains: "id -u", out: probeBootstrapped},
		fakeRule{contains: "can-bootstrap.sh", out: "ok\n"},
	)
	spec := sshSpec()
	spec.Options["k3s_version"] = "v1.34.0+k3s1"
	if err := newSSH(r).Create(context.Background(), spec, NewStore(t.TempDir()), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Copied["/usr/local/sbin/can-bootstrap.sh"]), "v1.34.0+k3s1") {
		t.Fatal("upgrade did not upload the new pinned version")
	}
}

func TestSSHBootstrapFailureIsReportedAndNotRecorded(t *testing.T) {
	r := newFakeRunner(
		fakeRule{contains: "id -u", out: probeGood},
		fakeRule{contains: "can-bootstrap.sh", out: "CAN_ERROR: node did not become Ready\n", err: errors.New("exit status 1")},
	)
	st := NewStore(t.TempDir())
	var out strings.Builder
	err := newSSH(r).Create(context.Background(), sshSpec(), st, &out)
	if err == nil || !strings.Contains(out.String(), "node did not become Ready") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
	if s, _ := st.Load(); s.Done("ssh.bootstrap") {
		t.Fatal("failed bootstrap recorded as done")
	}
}

func TestSSHOutputIsRedacted(t *testing.T) {
	r := newFakeRunner(
		fakeRule{contains: "id -u", out: probeGood},
		fakeRule{contains: "can-bootstrap.sh", out: "K3S_TOKEN=supersecretvalue123\n"},
	)
	var out strings.Builder
	if err := newSSH(r).Create(context.Background(), sshSpec(), NewStore(t.TempDir()), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "supersecretvalue123") {
		t.Fatal("remote secret leaked into output")
	}
}

func TestSSHPlanDoesNotTouchTheHost(t *testing.T) {
	r := newFakeRunner()
	st := NewStore(t.TempDir())
	acts, err := newSSH(r).Plan(context.Background(), sshSpec(), st)
	if err != nil || len(acts) != 2 || acts[0].Done || acts[1].Done {
		t.Fatalf("%+v %v", acts, err)
	}
	if len(r.Ran) != 0 || len(r.Copied) != 0 {
		t.Fatal("Plan touched the host")
	}
	if _, serr := os.Stat(st.Path); !os.IsNotExist(serr) {
		t.Fatal("Plan wrote state")
	}
}

func TestSSHPlanMarksDoneAfterCreate(t *testing.T) {
	r := newFakeRunner(fakeRule{contains: "id -u", out: probeBootstrapped})
	st := NewStore(t.TempDir())
	p := newSSH(r)
	_ = p.Create(context.Background(), sshSpec(), st, &strings.Builder{})
	acts, _ := p.Plan(context.Background(), sshSpec(), st)
	if !acts[1].Done {
		t.Fatalf("%+v", acts)
	}
}

func TestSSHDestroy(t *testing.T) {
	r := newFakeRunner(
		fakeRule{contains: "id -u", out: probeBootstrapped},
		fakeRule{contains: "k3s-uninstall.sh", out: "CAN_UNINSTALL_DONE\n"},
	)
	st := NewStore(t.TempDir())
	p := newSSH(r)
	if err := p.Destroy(context.Background(), sshSpec(), st, &strings.Builder{}); err != nil || len(r.Ran) != 0 {
		t.Fatalf("destroy with no state must be a no-op: %v", err)
	}
	_ = p.Create(context.Background(), sshSpec(), st, &strings.Builder{})
	var out strings.Builder
	if err := p.Destroy(context.Background(), sshSpec(), st, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "left as is") {
		t.Errorf("must say the server is kept: %s", out.String())
	}
	if _, err := os.Stat(st.Path); !os.IsNotExist(err) {
		t.Fatal("state not removed")
	}
	n := len(r.Ran)
	if err := p.Destroy(context.Background(), sshSpec(), st, &strings.Builder{}); err != nil || len(r.Ran) != n {
		t.Fatal("second destroy must be a no-op")
	}
}

func TestSSHStatus(t *testing.T) {
	st := NewStore(t.TempDir())
	ctx := context.Background()
	r := newFakeRunner(fakeRule{contains: "id -u", out: probeBootstrapped}, fakeRule{contains: "k3s_active", out: statusReady})
	p := newSSH(r)
	if s, err := p.Status(ctx, sshSpec(), st); err != nil || s.Phase != PhaseAbsent {
		t.Fatalf("%+v %v", s, err)
	}
	_ = p.Create(ctx, sshSpec(), st, &strings.Builder{})
	if s, _ := p.Status(ctx, sshSpec(), st); s.Phase != PhaseReady {
		t.Fatalf("%+v", s)
	}

	cases := map[string]struct {
		out   string
		err   error
		phase Phase
	}{
		"unreachable":      {"", fmt.Errorf("ssh: timeout"), PhaseDegraded},
		"k3s down":         {"bootstrapped=" + DefaultK3sVersion + "\nk3s_active=failed\nnodes_ready=0\n", nil, PhaseDegraded},
		"still installing": {"bootstrapped=\nk3s_active=inactive\nnodes_ready=0\n", nil, PhaseProvisioning},
	}
	for name, c := range cases {
		p2 := newSSH(newFakeRunner(fakeRule{contains: "k3s_active", out: c.out, err: c.err}))
		if s, _ := p2.Status(ctx, sshSpec(), st); s.Phase != c.phase {
			t.Errorf("%s: phase %s want %s", name, s.Phase, c.phase)
		}
	}
}

func TestSSHAddressAndTarget(t *testing.T) {
	st := NewStore(t.TempDir())
	p := newSSH(newFakeRunner(fakeRule{contains: "id -u", out: probeBootstrapped}))
	if _, err := p.Address(st); err == nil {
		t.Fatal("want error before create")
	}
	_ = p.Create(context.Background(), sshSpec(), st, &strings.Builder{})
	if ip, err := p.Address(st); err != nil || ip != "203.0.113.7" {
		t.Fatalf("%q %v", ip, err)
	}
	spec := sshSpec()
	spec.Options["user"], spec.Options["port"], spec.Options["key_file"] = "deploy", 2200, "/k/id"
	tg, err := TargetFor(spec, st, Deps{})
	if err != nil || tg.Host != "203.0.113.7" || tg.User != "deploy" || tg.Port != 2200 || tg.KeyFile != "/k/id" {
		t.Fatalf("%+v %v", tg, err)
	}
}

func TestSSHOptionValidation(t *testing.T) {
	p := newSSH(newFakeRunner())
	st := NewStore(t.TempDir())
	for name, mut := range map[string]func(Spec){
		"missing host":   func(s Spec) { delete(s.Options, "host") },
		"host injection": func(s Spec) { s.Options["host"] = "-oProxyCommand=x" },
		"bad tarballs":   func(s Spec) { s.Options["image_tarballs"] = []any{"x"} },
		"bad version":    func(s Spec) { s.Options["k3s_version"] = "v1" },
	} {
		spec := sshSpec()
		mut(spec)
		if _, err := p.Plan(context.Background(), spec, st); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestBootstrapOptionsFromTarballs(t *testing.T) {
	spec := sshSpec()
	spec.Options["image_tarballs"] = []any{map[string]any{"name": "a.tar", "url": "https://example.invalid/a.tar", "sha256": strings.Repeat("a", 64)}}
	spec.Options["operator_ip"] = "198.51.100.9"
	bo, err := BootstrapOptionsFrom(spec)
	if err != nil || len(bo.ImageTarballs) != 1 || bo.OperatorCIDR != "198.51.100.9/32" {
		t.Fatalf("%+v %v", bo, err)
	}
}

func TestNewAndStubs(t *testing.T) {
	for _, typ := range Types() {
		p, err := New(typ, Deps{})
		if err != nil || p.Name() != typ {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	if _, err := New("nope", Deps{}); err == nil {
		t.Fatal("unknown type accepted")
	}
	for _, typ := range []string{"local", "static"} {
		p, _ := New(typ, Deps{})
		st := NewStore(t.TempDir())
		for name, err := range map[string]error{
			"plan":    func() error { _, e := p.Plan(context.Background(), Spec{}, st); return e }(),
			"create":  p.Create(context.Background(), Spec{}, st, &strings.Builder{}),
			"destroy": p.Destroy(context.Background(), Spec{}, st, &strings.Builder{}),
			"status":  func() error { _, e := p.Status(context.Background(), Spec{}, st); return e }(),
			"address": func() error { _, e := p.Address(st); return e }(),
		} {
			if !errors.Is(err, ErrNotImplemented) {
				t.Errorf("%s.%s: %v", typ, name, err)
			}
		}
		if _, err := os.Stat(st.Path); !os.IsNotExist(err) {
			t.Errorf("%s stub wrote state", typ)
		}
	}
}

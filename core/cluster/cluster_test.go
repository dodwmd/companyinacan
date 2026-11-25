package cluster

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fake records every command and answers from canned responses keyed by the command line.
type fake struct {
	calls []Cmd
	out   map[string]string
	err   map[string]error
}

func (f *fake) Run(_ context.Context, c Cmd) (string, error) {
	f.calls = append(f.calls, c)
	k := c.String()
	return f.out[k], f.err[k]
}

func (f *fake) lines() []string {
	var s []string
	for _, c := range f.calls {
		s = append(s, c.String())
	}
	return s
}

func cfg() Config { return DefaultConfig() }

const listKey = "k3d cluster list companyinacan-dev --output json"

func TestCreatePlanIsExact(t *testing.T) {
	got := strings.Join(cfg().CreatePlan().Lines(), "\n")
	want := strings.Join([]string{
		"k3d cluster create companyinacan-dev --config dev/k3d.yaml",
		"kubectl --context k3d-companyinacan-dev wait --for=condition=Ready node --all --timeout=180s",
		"kubectl --context k3d-companyinacan-dev get crd/helmchartconfigs.helm.cattle.io",
		"kubectl --context k3d-companyinacan-dev wait --for=condition=Established crd/helmchartconfigs.helm.cattle.io --timeout=180s",
		"kubectl --context k3d-companyinacan-dev apply -k deploy/base",
		"kubectl --context k3d-companyinacan-dev -n kube-system rollout restart deployment/coredns",
		"kubectl --context k3d-companyinacan-dev -n kube-system rollout status deployment/coredns --timeout=120s",
	}, "\n")
	if got != want {
		t.Fatalf("plan mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDeleteAndStatusPlans(t *testing.T) {
	if got := cfg().DeletePlan().Lines(); !reflect.DeepEqual(got, []string{"k3d cluster delete companyinacan-dev"}) {
		t.Fatalf("delete: %v", got)
	}
	if got := cfg().StatusPlan().Lines(); !reflect.DeepEqual(got, []string{listKey}) {
		t.Fatalf("status: %v", got)
	}
}

func TestCustomNameFlowsIntoContext(t *testing.T) {
	c := cfg()
	c.Name = "x"
	if !strings.Contains(strings.Join(c.ApplyPlan().Lines(), "\n"), "--context k3d-x ") {
		t.Fatal("context not derived from name")
	}
}

func TestValidateName(t *testing.T) {
	for _, bad := range []string{"", "Has Space", "-a", "a_b", strings.Repeat("a", 33), "UP"} {
		c := cfg()
		c.Name = bad
		if c.Validate() == nil {
			t.Errorf("name %q should be rejected", bad)
		}
	}
	if err := cfg().Validate(); err != nil {
		t.Fatal(err)
	}
}

const listRunning = `[{"name":"companyinacan-dev","serversCount":1,"serversRunning":1}]`

func TestStatus(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		err    error
		exists bool
		run    bool
		fail   bool
	}{
		{"running", listRunning, nil, true, true, false},
		{"stopped", `[{"name":"companyinacan-dev","serversCount":1,"serversRunning":0}]`, nil, true, false, false},
		{"absent-empty-list", `[]`, nil, false, false, false},
		{"absent-error", "", errors.New("exit status 1: FATA No nodes found for given cluster"), false, false, false},
		{"docker-down", "", errors.New("exit status 1: Cannot connect to the Docker daemon"), false, false, true},
		{"garbage", "not json", nil, false, false, true},
	}
	for _, tc := range cases {
		f := &fake{out: map[string]string{listKey: tc.out}, err: map[string]error{listKey: tc.err}}
		st, err := New(cfg(), f).Status(context.Background())
		if (err != nil) != tc.fail {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
		if err == nil && (st.Exists != tc.exists || st.Running != tc.run) {
			t.Errorf("%s: got %+v", tc.name, st)
		}
	}
}

func TestUpCreatesWhenAbsentAndSkipsWhenPresent(t *testing.T) {
	f := &fake{err: map[string]error{listKey: errors.New("No nodes found for given cluster")}}
	if err := New(cfg(), f).Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls); n != 1+len(cfg().CreatePlan()) {
		t.Fatalf("calls=%d: %v", n, f.lines())
	}
	f = &fake{out: map[string]string{listKey: listRunning}}
	if err := New(cfg(), f).Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, l := range f.lines() {
		if strings.HasPrefix(l, "k3d cluster create") {
			t.Fatal("must not create an existing cluster")
		}
	}
	if len(f.calls) != 1+len(cfg().ApplyPlan()) {
		t.Fatalf("existing cluster should only re-apply: %v", f.lines())
	}
}

func TestUpStopsAtFirstFailure(t *testing.T) {
	boom := errors.New("boom")
	f := &fake{err: map[string]error{
		listKey: errors.New("No nodes found"),
		"k3d cluster create companyinacan-dev --config dev/k3d.yaml": boom,
	}}
	err := New(cfg(), f).Up(context.Background())
	if !errors.Is(err, boom) || len(f.calls) != 2 {
		t.Fatalf("err=%v calls=%v", err, f.lines())
	}
}

func TestUpStartsAStoppedCluster(t *testing.T) {
	stopped := `[{"name":"companyinacan-dev","serversCount":1,"serversRunning":0}]`
	f := &fake{out: map[string]string{listKey: stopped}}
	if err := New(cfg(), f).Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.lines()[1]; got != "k3d cluster start companyinacan-dev" {
		t.Fatalf("calls=%v", f.lines())
	}
}

func TestUpSurfacesStatusErrors(t *testing.T) {
	f := &fake{err: map[string]error{listKey: errors.New("Cannot connect to the Docker daemon")}}
	if err := New(cfg(), f).Up(context.Background()); err == nil || len(f.calls) != 1 {
		t.Fatalf("err=%v calls=%v", err, f.lines())
	}
}

func TestRetriesUntilTheCRDExists(t *testing.T) {
	crd := "kubectl --context k3d-companyinacan-dev get crd/helmchartconfigs.helm.cattle.io"
	n := 0
	f := &countingFake{fake: fake{out: map[string]string{listKey: listRunning}}, failFirst: map[string]int{crd: 3}, n: &n}
	m := New(cfg(), f)
	var slept int
	m.sleep = func(time.Duration) { slept++ }
	if err := m.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slept != 3 {
		t.Fatalf("slept %d times, want 3", slept)
	}
	// Non-retried commands fail immediately.
	f = &countingFake{fake: fake{out: map[string]string{listKey: listRunning}}, failFirst: map[string]int{"kubectl --context k3d-companyinacan-dev apply -k deploy/base": 1}, n: &n}
	m = New(cfg(), f)
	m.sleep = func(time.Duration) { t.Fatal("must not retry apply") }
	if err := m.Up(context.Background()); err == nil {
		t.Fatal("expected apply failure")
	}
}

// countingFake fails the first N runs of selected commands.
type countingFake struct {
	fake
	failFirst map[string]int
	n         *int
}

func (f *countingFake) Run(ctx context.Context, c Cmd) (string, error) {
	out, err := f.fake.Run(ctx, c)
	if f.failFirst[c.String()] > 0 {
		f.failFirst[c.String()]--
		return "", errors.New("not yet")
	}
	return out, err
}

func TestDown(t *testing.T) {
	f := &fake{out: map[string]string{listKey: listRunning}}
	if err := New(cfg(), f).Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.lines(), []string{listKey, "k3d cluster delete companyinacan-dev"}) {
		t.Fatal(f.lines())
	}
}

func TestDownOfAbsentClusterIsANoOp(t *testing.T) {
	f := &fake{err: map[string]error{listKey: errors.New("No nodes found for given cluster")}}
	if err := New(cfg(), f).Down(context.Background()); err != nil || len(f.calls) != 1 {
		t.Fatalf("err=%v calls=%v", err, f.lines())
	}
}

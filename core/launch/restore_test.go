package launch

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
)

func restoreCluster(k *fakeKube) *RestoreCluster {
	return &RestoreCluster{Kube: k, CoreImage: "can-core:dev", Out: &bytes.Buffer{}, Sleep: func(context.Context, time.Duration) {},
		Timeouts: Timeouts{Rollout: 10 * time.Second, CRD: 10 * time.Second, Module: 10 * time.Second}}
}

func (f *fakeKube) joined() string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c.Args, " ")+"\n"+c.Stdin)
	}
	return strings.Join(out, "\n")
}

func TestRestoreClusterStartDeploysRestoreMode(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "restore status", out: "restoring r1\n"}}}
	c := restoreCluster(k)
	err := c.Start(context.Background(), backup.StartOptions{ID: "r1", CompanyYAML: []byte("name: Acme\ndomain: acme.example\n")})
	if err != nil {
		t.Fatal(err)
	}
	all := k.joined()
	for _, want := range []string{"CAN_RESTORE", "r1", "acme.example", "memory: 384Mi"} {
		if !strings.Contains(all, want) {
			t.Errorf("manifests lack %q", want)
		}
	}
	if strings.Contains(all, "CAN_RESTORE_OVERWRITE") {
		t.Error("overwrite without confirmation")
	}
	// a can-core that did not latch restore mode must stop the restore
	k2 := &fakeKube{rules: []kubeRule{{contains: "restore status", out: "normal\n"}}}
	if err := restoreCluster(k2).Start(context.Background(), backup.StartOptions{ID: "r1", CompanyYAML: []byte("name: A\n")}); err == nil {
		t.Fatal("continued although can-core is not in restore mode")
	}
}

func TestRestoreClusterInspect(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "get namespaces", out: "namespace/can-system\nnamespace/mod-forms\n"}}}
	found, err := restoreCluster(k).Inspect(context.Background())
	if err != nil || len(found) != 1 || !strings.Contains(found[0], "mod-forms") {
		t.Fatal(found, err)
	}
}

func TestRestoreClusterVolumes(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{
		{contains: "gone --ignore-not-found", out: ""},
		{contains: "persistentvolumeclaim data --ignore-not-found", out: "persistentvolumeclaim/data\n"},
		{contains: "jsonpath={.status.phase}", out: "Bound pv-1"},
		{contains: "get persistentvolume pv-1", out: "/var/lib/rancher/k3s/storage/pvc-1_mod-a_data"},
	}}
	got, err := restoreCluster(k).Volumes(context.Background(), []backup.VolumeRef{{Namespace: "mod-a", Claim: "data"}, {Namespace: "mod-a", Claim: "gone"}})
	if err != nil || len(got) != 1 || got[0].Dir != "/var/lib/rancher/k3s/storage/pvc-1_mod-a_data" {
		t.Fatal(got, err)
	}
	if all := k.joined(); !strings.Contains(all, "claimName: data") || !strings.Contains(all, "delete pod can-restore-bind-0") {
		t.Error("the bind pod was not created and removed")
	}
	// a volume that is not a local directory is refused
	for _, dir := range []string{"", "/", "relative", "/var/lib/../../etc"} {
		k := &fakeKube{rules: []kubeRule{
			{contains: "--ignore-not-found -o name", out: "persistentvolumeclaim/data\n"},
			{contains: "jsonpath={.status.phase}", out: "Bound pv-1"},
			{contains: "get persistentvolume pv-1", out: dir},
		}}
		if _, err := restoreCluster(k).Volumes(context.Background(), []backup.VolumeRef{{Namespace: "mod-a", Claim: "data"}}); err == nil {
			t.Errorf("accepted volume directory %q", dir)
		}
	}
}

func TestRestoreClusterReportsKeepTheError(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "restore erase", out: `{"unapplied":["forms:lead-submission"],"purged":[]}` + "\n", err: context.DeadlineExceeded}}}
	rep, err := restoreCluster(k).Erase(context.Background(), []backup.Entry{{Subject: "s", Class: "c"}}, nil)
	if err == nil || len(rep.Unapplied) != 1 {
		t.Fatal(rep, err)
	}
	if !strings.Contains(k.joined(), `"subject":"s"`) {
		t.Error("the entries go on stdin")
	}
}

// Going live always restarts can-core and ends only when the new process serves normally.
func TestRestoreClusterCompleteVerifiesNormalMode(t *testing.T) {
	rules := func(serving string) []kubeRule {
		return []kubeRule{
			{contains: "restore status", out: "normal restored r1\n"},
			{contains: "restore serving", out: serving},
			{contains: "get configmap company", out: "name: A\ndomain: a.example\n"},
		}
	}
	k := &fakeKube{rules: rules("normal\n")}
	if err := restoreCluster(k).Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := k.joined()
	if !strings.Contains(all, "rollout restart deploy/can-core") || strings.Index(all, "rollout restart") > strings.Index(all, "restore serving") {
		t.Error("no restart before the check:\n" + all)
	}
	k = &fakeKube{rules: rules("restoring\n")}
	if err := restoreCluster(k).Complete(context.Background()); err == nil || !strings.Contains(err.Error(), "restore mode") {
		t.Fatalf("a latched can-core must not be reported as live: %v", err)
	}
}

// An exposed module cannot start before `can up` writes the routing facts: the restore says so
// instead of waiting for it, and still waits for a module that is stopped for another reason.
func TestRestoreClusterCompleteDoesNotWaitForRoutes(t *testing.T) {
	for name, tc := range map[string]struct {
		problem, want string
		polls         int
	}{
		"routes":  {`[{"id":"work","state":"enabled","error":"ConfigMap can-system/can-routing does not exist, so nothing can be routed: run can up"}]`, `module "work" is restored and starts when`, 1},
		"another": {`[{"id":"work","state":"failed","error":"image not found"}]`, `module "work" has not come up yet`, 3},
	} {
		// can-core says "restoring" until it is told to complete and restarted.
		k := &fakeKube{rules: []kubeRule{
			{contains: "restore status", out: "restoring r1\n", times: 1},
			{contains: "restore status", out: "normal restored r1\n"},
			{contains: "restore complete", out: `{"modules":["work"]}`},
			{contains: "restore serving", out: "normal\n"},
			{contains: "get configmap company", out: "name: A\ndomain: a.example\n"},
			{contains: "modules problems", out: tc.problem},
			{contains: "get deployments,statefulsets", out: "0/\n"},
		}}
		c := restoreCluster(k)
		var out bytes.Buffer
		c.Out = &out
		if err := c.Complete(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: %q not in\n%s", name, tc.want, out.String())
		}
		if n := strings.Count(k.joined(), "get deployments,statefulsets"); n != tc.polls {
			t.Errorf("%s: %d polls, want %d", name, n, tc.polls)
		}
	}
}

func TestRestoreClusterMode(t *testing.T) {
	for want, rules := range map[string][]kubeRule{
		"":             {{contains: "get deployment can-core", out: ""}},
		"restoring r1": {{contains: "get deployment can-core", out: "deployment.apps/can-core\n"}, {contains: "restore status", out: "restoring r1\n"}},
		"unknown":      {{contains: "get deployment can-core", out: "deployment.apps/can-core\n"}, {contains: "restore status", err: context.DeadlineExceeded}},
	} {
		if got, err := restoreCluster(&fakeKube{rules: rules}).Mode(context.Background()); err != nil || got != want {
			t.Errorf("want %q got %q %v", want, got, err)
		}
	}
}

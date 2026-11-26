package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/launch"
)

// moduleKube answers for the deployments of modules: work appears after `after` questions, support
// never does.
type moduleKube struct {
	after, asked int
}

const rolledOut = `{"items":[{"metadata":{"name":"work","generation":2},"spec":{"replicas":1},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}]}`

func (k *moduleKube) Run(_ context.Context, _ []byte, args ...string) (string, error) {
	line := strings.Join(args, " ")
	switch {
	case strings.Contains(line, "-n mod-work get deploy"):
		if k.asked++; k.asked <= k.after {
			return `{"items":[]}`, nil
		}
		return rolledOut, nil
	case strings.Contains(line, "get deploy"):
		return `{"items":[]}`, nil
	case strings.Contains(line, "modules problems"):
		return `[{"id":"support","state":"failed","error":"image pull"},{"id":"other","state":"failed","error":"x"}]`, nil
	case strings.Contains(line, "modules plan"):
		return `{"actions":[]}`, nil
	}
	return "", nil
}

const twoModules = "schema: 1\nname: Acme Co\ndomain: acme.example\nprovider:\n  type: ssh\n  options:\n    host: 203.0.113.7\nmodules:\n  work: {}\n  support: {}\n  wiki: { enabled: false }\n"

func useModuleWait(t *testing.T, k launch.Kube) {
	t.Helper()
	res := stackResult()
	res.CompanyYAML = []byte(twoModules)
	useStack(t, false, res, nil)
	lifecycle.kube = func(string, string) launch.Kube { return k }
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	lifecycle.now = func() time.Time { return clock }
	moduleWaitSleep = func(_ context.Context, d time.Duration) { clock = clock.Add(d) }
	t.Cleanup(func() { moduleWaitSleep = nil })
}

func TestUpWaitsForModulesAndSaysWhichAreNotReady(t *testing.T) {
	dir, _ := setup(t)
	useModuleWait(t, &moduleKube{after: 3})
	code, out, errs := run(t, stackArgs(dir, "--module-wait", "1m", "--output", "json")...)
	if code != ExitOK {
		t.Fatalf("a module that is not ready does not fail `can up`: code=%d err=%q", code, errs)
	}
	for _, want := range []string{"waiting for can-core to install: support, work (at most 1m0s", "work is ready after 15s", "still waiting for support (30s)"} {
		if !strings.Contains(errs, want) {
			t.Errorf("progress lacks %q:\n%s", want, errs)
		}
	}
	for _, want := range []string{`"modules_not_ready":["support"]`, "NOT READY YET: support (after waiting 1m0s", `module \"support\": the last install`} {
		if !strings.Contains(out, want) {
			t.Errorf("result lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `module \"other\"`) || strings.Contains(out, "wiki") {
		t.Errorf("only the modules waited for are reported:\n%s", out)
	}
}

func TestUpModuleWaitZeroSaysSo(t *testing.T) {
	dir, _ := setup(t)
	k := &moduleKube{}
	useModuleWait(t, k)
	code, out, errs := run(t, stackArgs(dir, "--module-wait", "0")...)
	if code != ExitOK || k.asked != 0 || !strings.Contains(out, "not waited for (--module-wait 0): can-core installs support, work") {
		t.Fatalf("code=%d asked=%d out=%q err=%q", code, k.asked, out, errs)
	}
}

type errKube struct{}

func (errKube) Run(context.Context, []byte, ...string) (string, error) {
	return "", errors.New("tunnel closed")
}

func TestModuleWaitEndsWhenInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var slept int
	notReady, err := launch.WaitModules(ctx, errKube{}, []string{"work"}, launch.ModuleWait{
		Timeout: time.Hour, Sleep: func(context.Context, time.Duration) { slept++; cancel() }, Say: func(string, ...any) {},
	})
	if slept != 1 || len(notReady) != 1 || err == nil || !strings.Contains(err.Error(), "tunnel closed") {
		t.Fatalf("slept=%d %v %v", slept, notReady, err)
	}
}

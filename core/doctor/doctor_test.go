package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func lookup(found ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, f := range found {
		set[f] = true
	}
	return func(n string) (string, error) {
		if set[n] {
			return "/usr/bin/" + n, nil
		}
		return "", errors.New("not found")
	}
}

func TestAllRequiredPresent(t *testing.T) {
	res := Check(lookup("go", "git", "make"))
	if HasMissingRequired(res) {
		t.Fatalf("unexpected missing required: %+v", res)
	}
	var optMissing int
	for _, r := range res {
		if r.Status == StatusOptionalMissing {
			optMissing++
			if r.Remedy == "" {
				t.Errorf("%s: optional-missing tool needs a remedy", r.Tool)
			}
		}
	}
	if optMissing == 0 {
		t.Fatal("expected some optional tools reported missing")
	}
}

func TestMissingRequired(t *testing.T) {
	res := Check(lookup("go", "make")) // git missing
	if !HasMissingRequired(res) {
		t.Fatal("expected missing required")
	}
	for _, r := range res {
		if r.Tool == "git" && (r.Status != StatusMissing || r.Remedy == "") {
			t.Fatalf("git: %+v", r)
		}
	}
}

func TestContainerRuntimeAcceptsPodman(t *testing.T) {
	for _, r := range Check(lookup("go", "git", "make", "podman")) {
		if r.Tool == "docker or podman" && r.Status != StatusOK {
			t.Fatalf("podman should satisfy the container runtime check: %+v", r)
		}
	}
}

type call struct{ name, args string }

func fakeRunner(calls *[]call, out map[string]string, fail map[string]bool) Runner {
	return func(_ context.Context, name string, args ...string) (string, error) {
		a := strings.Join(args, " ")
		*calls = append(*calls, call{name, a})
		key := name + " " + a
		if fail[key] {
			return "", errors.New("exit status 1: password=hunter2 refused")
		}
		return out[key], nil
	}
}

func TestReachSkipsToolsThatAreNotInstalled(t *testing.T) {
	var calls []call
	res := Reach(context.Background(), lookup(), fakeRunner(&calls, nil, nil))
	if len(res) != 0 || len(calls) != 0 {
		t.Fatalf("ran commands for missing tools: %+v %+v", res, calls)
	}
}

func TestReachReportsDockerComposeAndKubernetes(t *testing.T) {
	var calls []call
	out := map[string]string{
		"docker info --format {{.ServerVersion}}":        "27.1.1\n",
		"docker compose -p companyinacan ps -q":          "abc\ndef\n",
		"kubectl --request-timeout=3s get --raw=/readyz": "ok",
	}
	res := Reach(context.Background(), lookup("docker", "kubectl"), fakeRunner(&calls, out, nil))
	byTool := map[string]Result{}
	for _, r := range res {
		byTool[r.Tool] = r
		if r.Required {
			t.Errorf("%s must never be required", r.Tool)
		}
	}
	if byTool["docker daemon"].Status != StatusOK || byTool["compose stack companyinacan"].Status != StatusOK || byTool["kubernetes API"].Status != StatusOK {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(byTool["compose stack companyinacan"].Path, "2 containers") {
		t.Fatalf("container count: %+v", byTool["compose stack companyinacan"])
	}
	if HasMissingRequired(res) {
		t.Fatal("reachability must not fail doctor")
	}
}

func TestReachFailuresAreGentleAndNeverLeakErrorText(t *testing.T) {
	var calls []call
	fail := map[string]bool{"docker info --format {{.ServerVersion}}": true, "kubectl --request-timeout=3s get --raw=/readyz": true}
	res := Reach(context.Background(), lookup("docker", "kubectl"), fakeRunner(&calls, nil, fail))
	got := map[string]string{}
	for _, r := range res {
		if strings.Contains(r.Remedy+r.Path, "hunter2") {
			t.Fatalf("error text leaked: %+v", r)
		}
		got[r.Tool] = r.Status
	}
	if got["docker daemon"] != StatusUnreachable || got["kubernetes API"] != StatusUnreachable {
		t.Fatalf("%v", got)
	}
	if _, ok := got["compose stack companyinacan"]; ok {
		t.Fatal("compose is not probed when the daemon is down")
	}
}

func TestReachComposeNotRunning(t *testing.T) {
	var calls []call
	out := map[string]string{"docker info --format {{.ServerVersion}}": "27", "docker compose -p companyinacan ps -q": ""}
	res := Reach(context.Background(), lookup("docker"), fakeRunner(&calls, out, nil))
	for _, r := range res {
		if r.Tool == "compose stack companyinacan" && r.Status != StatusNotRunning {
			t.Fatalf("%+v", r)
		}
	}
}

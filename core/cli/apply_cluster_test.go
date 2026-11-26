package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/cluster"
)

// fileExec records kubectl invocations and the manifest file they were given.
type fileExec struct {
	args     []string
	manifest string
	err      error
	// facts answers `get configmap can-routing` and site `get ingressroutes site`.
	facts, site string
	written     []string // every manifest applied
}

func (f *fileExec) Run(_ context.Context, c cluster.Cmd) (string, error) {
	f.args = c.Args
	for i, a := range c.Args {
		if a == "-f" {
			b, _ := os.ReadFile(c.Args[i+1])
			f.manifest = string(b)
			f.written = append(f.written, f.manifest)
		}
	}
	switch line := strings.Join(c.Args, " "); {
	case strings.Contains(line, "get configmap can-routing"):
		return f.facts, f.err
	case strings.Contains(line, "get ingressroutes.traefik.io site"):
		return f.site, f.err
	}
	return "configmap/company serverside-applied", f.err
}

func useExec(t *testing.T, f *fileExec) {
	t.Helper()
	old := newExecutor
	t.Cleanup(func() { newExecutor = old })
	newExecutor = func(*cobra.Command) cluster.Executor { return f }
}

func TestApplySyncsCompanyYamlIntoTheCluster(t *testing.T) {
	f := &fileExec{}
	useExec(t, f)
	dir := writeCompany(t, applyCompany)
	code, out, errs := run(t, "apply", dir, "--modules-dir", "../../modules", "--context", "k3d-x")
	if code != ExitOK || !strings.Contains(out, "synced company.yaml") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if f.args[0] != "--context" || f.args[1] != "k3d-x" {
		t.Fatalf("args %v", f.args)
	}
	var cm struct {
		Kind     string
		Metadata struct{ Name, Namespace string }
		Data     map[string]string
	}
	if err := yaml.Unmarshal([]byte(f.manifest), &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Kind != "ConfigMap" || cm.Metadata.Name != "company" || cm.Metadata.Namespace != "can-system" || cm.Data["company.yaml"] != applyCompany {
		t.Fatalf("%+v", cm)
	}
}

func TestApplyDoesNotSyncAnInvalidCompany(t *testing.T) {
	f := &fileExec{}
	useExec(t, f)
	dir := writeCompany(t, "schema: 1\nname: A\nprovider: {type: local}\nmodules:\n  ghost: {}\n")
	if code, _, _ := run(t, "apply", dir, "--modules-dir", "../../modules"); code != ExitFailure || f.args != nil {
		t.Fatalf("code=%d kubectl=%v", code, f.args)
	}
}

func TestApplySyncFailureIsReported(t *testing.T) {
	useExec(t, &fileExec{err: errors.New("connection refused")})
	code, _, errs := run(t, "apply", writeCompany(t, applyCompany), "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, "connection refused") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

// The routing facts are never invented: they are kept, read off the site route, or left to `can up`.
func TestApplyRoutingFacts(t *testing.T) {
	sslip := `{"spec":{"entryPoints":["web"],"routes":[{"match":"PathPrefix(` + "`/`" + `)"}]}}`
	for _, c := range []struct {
		name, facts, site string
		want              []string // in the facts written; nil = none written
		say               string
	}{
		{"already for this domain", `{"data":{"domain":"a.example","tls":"true","edgeLimit":"on"}}`, sslip, nil, ""},
		{"missing, plain HTTP stack", "", sslip, []string{`tls: "false"`, `catchAll: "true"`, `siteHosts: ""`}, "from the site route"},
		{"missing, no site route", "", "", nil, "run `can up`"},
	} {
		f := &fileExec{facts: c.facts, site: c.site}
		useExec(t, f)
		dir := writeCompany(t, applyCompany)
		code, out, errs := run(t, "apply", dir, "--modules-dir", "../../modules")
		if code != ExitOK || !strings.Contains(out, c.say) {
			t.Fatalf("%s: %d %q %q", c.name, code, out, errs)
		}
		var facts string
		for _, m := range f.written {
			if strings.Contains(m, "name: can-routing") {
				facts = m
			}
		}
		if (facts != "") != (c.want != nil) {
			t.Fatalf("%s: facts written: %q", c.name, facts)
		}
		for _, w := range c.want {
			if !strings.Contains(facts, w) {
				t.Errorf("%s: lacks %s in %s", c.name, w, facts)
			}
		}
	}
}

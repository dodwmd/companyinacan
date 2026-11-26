package contract

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/ticket"
	"github.com/dodwmd/companyinacan/core/validate"
)

// can-core's development stack mode (core/server/runtime/devstack.go) trusts token files instead of
// Kubernetes. It is compiled in only with `-tags candevstack`. Nothing that builds, renders or ships a
// production installation may mention the tag or the settings that ask for the mode.
func TestNothingProductionCanEnableTheDevStack(t *testing.T) {
	words := []string{"candevstack", "CAN_DEV_STACK", "CAN_DEV_MODULE_TOKEN_DIR"}
	shipped := []string{"deploy", "scripts", ".github", "modules", "templates", "schemas", "cmd", "examples", "Makefile",
		"core/cli", "core/launch", "core/provider", "core/cluster", "core/config", "core/server/installer", "core/server/modmgr"}
	for _, dir := range shipped {
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
			t.Fatalf("%s: %v (update this list)", dir, err)
		}
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path) //nolint:gosec // repository files
			if err != nil {
				return err
			}
			for _, w := range words {
				if strings.Contains(string(b), w) {
					t.Errorf("%s mentions %s: the development stack must stay out of everything that ships", path, w)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The compose stack is the one place the mode is on, so every port it publishes is loopback only, and
// the addresses and company.yaml it gives can-core are ones can-core accepts.
func TestComposeDevStackStaysOnLoopbackAndIsUsable(t *testing.T) {
	var compose struct {
		Services map[string]struct {
			Ports       []string       `yaml:"ports"`
			Environment map[string]any `yaml:"environment"`
			Networks    map[string]struct {
				Aliases []string `yaml:"aliases"`
			} `yaml:"networks"`
		} `yaml:"services"`
	}
	b, err := os.ReadFile(filepath.Join(root, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &compose); err != nil {
		t.Fatal(err)
	}
	for name, s := range compose.Services {
		for _, p := range s.Ports {
			if !strings.HasPrefix(p, "127.0.0.1:") {
				t.Errorf("service %s publishes %q beyond loopback", name, p)
			}
		}
	}
	env := compose.Services["core"].Environment
	l := ticket.Launcher{Domain: env["CAN_COMPANY_DOMAIN"].(string), AllowHTTP: true}
	urls := strings.Split(env["CAN_MODULE_UI_URLS"].(string), ",")
	if len(urls) != 4 {
		t.Errorf("CAN_MODULE_UI_URLS = %v, want work, people, jobs and finance", urls)
	}
	for _, e := range urls {
		id, u, _ := strings.Cut(e, "=")
		if !l.Acceptable(u) || !strings.HasPrefix(u, "http://localhost:") {
			t.Errorf("%s: %q is not an address the launcher accepts on one plain-http host", id, u)
		}
	}
	seed := filepath.Join(root, "dev", "compose", "config", "company.yaml")
	data, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range validate.CompanyBytes(seed, data, filepath.Join(root, "modules")) {
		if p.Severity == problem.Error {
			t.Errorf("dev company.yaml: %s %s: %s", p.Rule, p.Path, p.Message)
		}
	}
	// Every module can-core records is wired to it with its own token file, and has one generated.
	init, err := os.ReadFile(filepath.Join(root, "dev", "compose", "init.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var company struct {
		Modules map[string]any `yaml:"modules"`
	}
	if err := yaml.Unmarshal(data, &company); err != nil {
		t.Fatal(err)
	}
	for id := range company.Modules {
		e := compose.Services[id].Environment
		if e["CAN_MODULE_ID"] != id || e["CAN_TOKEN_FILE"] != "/tokens/"+id || e["CAN_API_URL"] != "http://core:8090" {
			t.Errorf("service %s is not wired to can-core: %v", id, e)
		}
		if !strings.Contains(string(init), " "+id) {
			t.Errorf("init.sh generates no token for %s", id)
		}
		// A module that subscribes to events is reachable at the name can-core delivers to, and
		// verifies with the secret can-core signs with.
		doc, ps := config.Load(filepath.Join(root, "modules", id, "module.yaml"))
		if doc == nil {
			t.Fatalf("%s: %v", id, ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		_, subs, err := registry.FromManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		for _, sb := range subs {
			url, err := registry.ServiceURL("mod-"+id, sb.Service, sb.Port)
			if err != nil {
				t.Fatal(err)
			}
			host, _, _ := strings.Cut(strings.TrimPrefix(url, "http://"), ":")
			if !slices.Contains(compose.Services[id].Networks["default"].Aliases, host) {
				t.Errorf("service %s has no network alias %s: can-core cannot deliver %s", id, host, sb.Event)
			}
			if e["CAN_WEBHOOK_SECRET"] != env["CAN_DEV_WEBHOOK_SECRET"] || e["CAN_WEBHOOK_SECRET"] == nil {
				t.Errorf("service %s does not verify with can-core's secret", id)
			}
		}
	}
}

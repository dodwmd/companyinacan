package contract

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/server/adminapi"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

// The OpenAPI files are the HTTP contract of can-core (epic E03). These tests keep them and the
// handlers from drifting: every served route is documented and every documented route is served.

type openAPI struct {
	OpenAPI string `yaml:"openapi"`
	Paths   map[string]map[string]struct {
		OperationID string              `yaml:"operationId"`
		Responses   map[string]any      `yaml:"responses"`
		Security    *[]map[string][]any `yaml:"security"`
		Parameters  []struct {
			Name string `yaml:"name"`
			In   string `yaml:"in"`
		} `yaml:"parameters"`
	} `yaml:"paths"`
	Webhooks map[string]map[string]struct {
		Parameters []struct {
			Name string `yaml:"name"`
			In   string `yaml:"in"`
		} `yaml:"parameters"`
	} `yaml:"webhooks"`
	Components struct {
		Schemas map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

func loadAPI(t *testing.T, name string) (openAPI, string) {
	t.Helper()
	p := filepath.Join(root, "specs", "005-can-core-mvp", "contracts", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var api openAPI
	if err := yaml.Unmarshal(b, &api); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return api, string(b)
}

func (a openAPI) routes() []string {
	var out []string
	for path, ops := range a.Paths {
		for method := range ops {
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}

func TestCanCoreAPIContracts(t *testing.T) {
	for file, served := range map[string][]string{
		"admin-api.yaml":  adminapi.Routes(),
		"module-api.yaml": moduleapi.Routes(),
	} {
		t.Run(file, func(t *testing.T) {
			api, raw := loadAPI(t, file)
			if !strings.HasPrefix(api.OpenAPI, "3.1") {
				t.Fatalf("openapi = %q, want 3.1.x", api.OpenAPI)
			}
			sort.Strings(served)
			got, want := strings.Join(served, "\n"), strings.Join(api.routes(), "\n")
			if got != want {
				t.Fatalf("handlers and contract differ\nserved:\n%s\n\ndocumented:\n%s", got, want)
			}
			ids := map[string]bool{}
			for path, ops := range api.Paths {
				for method, op := range ops {
					if op.OperationID == "" || ids[op.OperationID] {
						t.Errorf("%s %s: operationId missing or duplicated", method, path)
					}
					ids[op.OperationID] = true
					if len(op.Responses) == 0 {
						t.Errorf("%s %s: no responses documented", method, path)
					}
					if path != "/healthz" {
						isLogin := file == "admin-api.yaml" && path == "/api/v1/login"
						if _, ok := op.Responses["401"]; !ok && !isLogin {
							t.Errorf("%s %s: protected operation does not document 401", method, path)
						}
					}
					// every operation documents a success response and only known status codes
					hasOK := false
					for code := range op.Responses {
						switch code[0] {
						case '2':
							hasOK = true
						case '4', '5':
						default:
							t.Errorf("%s %s: unexpected response code %q", method, path, code)
						}
					}
					if !hasOK {
						t.Errorf("%s %s: no 2xx response", method, path)
					}
					// admin API: session-authenticated unsafe methods carry the csrf scheme and document 403;
					// safe methods do not.
					if file == "admin-api.yaml" && path != "/healthz" && path != "/api/v1/login" {
						needsCSRF := strings.ToUpper(method) != "GET"
						hasCSRF := false
						if op.Security != nil {
							for _, req := range *op.Security {
								_, c := req["csrf"]
								hasCSRF = hasCSRF || c
							}
						}
						if needsCSRF != hasCSRF {
							t.Errorf("%s %s: csrf security scheme present=%v, want %v", method, path, hasCSRF, needsCSRF)
						}
						if _, has403 := op.Responses["403"]; needsCSRF && !has403 {
							t.Errorf("%s %s: unsafe operation does not document 403 (CSRF)", method, path)
						}
					}
					// open operations declare it explicitly with security: []
					if (path == "/healthz" || (file == "admin-api.yaml" && path == "/api/v1/login")) && (op.Security == nil || len(*op.Security) != 0) {
						t.Errorf("%s %s: must declare `security: []`", method, path)
					}
					for _, p := range op.Parameters {
						if p.In == "path" && !strings.Contains(path, "{"+p.Name+"}") {
							t.Errorf("%s %s: path parameter %s not in the path", method, path, p.Name)
						}
					}
				}
			}
			// every $ref points at a defined component
			for _, ref := range refs(raw) {
				name := strings.TrimPrefix(ref, "#/components/")
				kind, id, _ := strings.Cut(name, "/")
				if kind == "schemas" {
					if _, ok := api.Components.Schemas[id]; !ok {
						t.Errorf("dangling $ref %s", ref)
					}
				}
			}
		})
	}
}

func refs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, `$ref: "`)[1:] {
		out = append(out, part[:strings.Index(part, `"`)])
	}
	return out
}

// The webhook headers documented for module authors are the ones the dispatcher sends.
func TestWebhookHeadersDocumented(t *testing.T) {
	api, _ := loadAPI(t, "module-api.yaml")
	op := api.Webhooks["event"]["post"]
	have := map[string]bool{}
	for _, p := range op.Parameters {
		if p.In == "header" {
			have[p.Name] = true
		}
	}
	for _, h := range []string{webhook.HeaderEventID, webhook.HeaderEventType, webhook.HeaderAttempt, webhook.HeaderTimestamp, webhook.HeaderSignature} {
		if !have[h] {
			t.Errorf("webhook header %s is sent but not documented", h)
		}
	}
}

package restoreops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// The installer renders charts by running its own binary as `render-chart`; here that is this test.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "render-chart" {
		os.Exit(installer.RunRenderChild(os.Stdin, os.Stdout, 0, 0))
	}
	os.Exit(m.Run())
}

// A connected module that was disabled when the backup was made has no copied credential in the
// Secrets export (disabling removed it). The restore recreates it all the same, and keeps the
// credential of one that was enabled (specs/057-module-capability-wiring). equity and provisioning
// come before and after people in the order of the restore.
func TestRestoreWithConnectedConsumers(t *testing.T) {
	link := config.Link{Capability: "directory", Provider: "people", Service: "people-api", Port: 8081}
	wired := func(m *config.Module) { m.Runtime.Wired = []config.Link{link} }
	const token = "lookup-token-lookup-token-lookup-token"
	for name, c := range map[string]struct{ equity, provisioning string }{
		"consumers disabled": {store.StateDisabled, store.StateDisabled},
		"consumers failed":   {store.StateFailed, store.StateFailed},
		"equity enabled":     {store.StateEnabled, store.StateDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			signed := func(id, state string, change func(*config.Module)) store.Module {
				m := module(id, state, change)
				m.HMACSecret = "0123456789abcdef0123456789abcdef"
				return m
			}
			o, _, api := ops(t,
				signed("equity", c.equity, wired),
				signed("people", store.StateEnabled, nil),
				signed("provisioning", c.provisioning, wired))
			o.ModulesDir = "../../../modules"
			data := map[string]string{}
			if c.equity == store.StateEnabled {
				data["CAN_EQUITY_DIRECTORY_TOKEN"] = base64.StdEncoding.EncodeToString([]byte(token))
			}
			export, err := json.Marshal(backup.SecretsExport{Version: 1, Secrets: []backup.ExportedSecret{{Namespace: "mod-equity", Name: "can-module", Data: data}}})
			if err != nil {
				t.Fatal(err)
			}
			for file, content := range map[string][]byte{
				backup.SecretsDump: export,
				backup.CompanyDump: []byte("schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\nmodules:\n  people: {}\n  equity: {enabled: false}\n"),
			} {
				p := filepath.Join(o.DumpsDir, filepath.FromSlash(file))
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			o.Installer = func(seed func(context.Context, reconcile.Spec) error, _ string) reconcile.ModuleInstaller {
				return installer.New(installer.Config{API: api, ModulesDir: o.ModulesDir, PlatformURL: "http://can-core.can-system.svc:8090",
					NodeIP: "172.18.0.2", Domain: "acme.example", Quiesce: true, Seed: seed, ChildAddressSpaceMB: -1, Poll: time.Millisecond})
			}
			rep, err := o.Apply(context.Background(), false)
			if err != nil || len(rep.Modules) != 3 {
				t.Fatalf("restore: %v %+v", err, rep)
			}
			s, err := api.Get(context.Background(), "Secret", "mod-equity", "can-module")
			if err != nil {
				t.Fatal(err)
			}
			held, _ := s["data"].(map[string]any)["CAN_EQUITY_DIRECTORY_TOKEN"].(string)
			if want := data["CAN_EQUITY_DIRECTORY_TOKEN"]; held != want {
				t.Errorf("equity's credential = %q, want %q", held, want)
			}
			if _, err := api.Get(context.Background(), "NetworkPolicy", "mod-equity", "can-wire-to-people"); !kube.IsNotFound(err) {
				t.Errorf("a restore made a connection: %v", err)
			}
		})
	}
}

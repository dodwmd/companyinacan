//go:build integration

package integration

// Every in-house module, enabled from company.yaml, installs through can-core on a real cluster and
// becomes Ready; then can-core reads the erasure feed of every module that has one
// (specs/048-erasure-collection). That is all this proves: it does not use the modules.
//
// Runs only with CAN_IT_CANCORE=1 and CAN_IT_MODULES=1 (.github/workflows/modules-k3d.yml), with
// can-core and the module images imported into the cluster.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/backup"
)

// inHouseModules is read from the manifests, so that a new in-house module is covered.
func inHouseModules(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), "modules", "[a-z]*", "module.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			ID       string `yaml:"id"`
			Provider struct {
				Type string `yaml:"type"`
			} `yaml:"provider"`
		}
		if err := yaml.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m.Provider.Type == "inhouse" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func TestEveryInHouseModuleOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" || os.Getenv("CAN_IT_MODULES") != "1" {
		t.Skip("CAN_IT_CANCORE=1 and CAN_IT_MODULES=1 not set (needs the module images in the cluster)")
	}
	ids := inHouseModules(t)
	if len(ids) < 10 {
		t.Fatalf("only %v", ids)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		dumpState(t)
		for _, id := range ids {
			out, _ := kubectl("-n", "mod-"+id, "logs", "deploy/"+id, "--tail=20")
			t.Logf("mod-%s:\n%s", id, out)
		}
	})
	prepareDumpsDir(t)
	company := "schema: 1\nname: Every Module Co\ndomain: modules.example\nprovider:\n  type: local\n" +
		"organisation: {legal_name: Every Module Pty Ltd, address: \"1 Anvil Way\\nBrisbane\", tax_id: TAX-1}\nmodules:\n"
	// Settings that each module's own loader reads at start (specs/064-module-settings): a pod that is
	// Ready accepted its configuration. None is looser to remove than to keep: TestModuleWiringOnK3d
	// applies a company.yaml without them to this cluster next, and a widening upgrade waits.
	settings := map[string]string{
		"board":        `{retention: {board-packs: 1y}}`,
		"entity":       `{retention: {entity-person: 1y}}`,
		"equity":       `{retention: {equity-grants: 1y}}`,
		"files":        `{max_file_mb: 100, quota_gb_per_owner: 20, inline_images: true, share: {max_ttl_hours: 800}, ui: {idle_minutes: 60}}`,
		"finance":      `{payment_instructions: "Pay by transfer\nRef $(NOT_EXPANDED)", retention_years: 5, ui: {idle_minutes: 60}}`,
		"forms":        `{rate_limit_per_minute: 20, cors_allowed_origins: ["https://app.modules.example"]}`,
		"jobs":         `{collect_eeo: true, max_resume_mb: 10, default_valid_days: 30, retention: {rejected_days: 100}}`,
		"people":       `{retention: {people-timeoff: 1y}, ui: {absolute_hours: 12}}`,
		"provisioning": `{leaver_cutoff: end_of_day}`,
		"work":         `{retention_days: 365, max_body_bytes: 2097152}`,
	}
	for _, id := range ids {
		entry := "{}"
		if s := settings[id]; s != "" {
			entry = "{settings: " + s + "}"
		}
		company += "  " + id + ": " + entry + "\n"
	}
	canApplyModules(t, company)
	deadline := time.Now().Add(15 * time.Minute)
	for _, id := range ids {
		eventually(t, max(time.Until(deadline), 30*time.Second), id+" to be rolled out", func() (bool, string) {
			out, err := kubectl("-n", "mod-"+id, "rollout", "status", "deploy/"+id, "--timeout=5s")
			return err == nil, out
		})
		if got := mustKubectl(t, "-n", "mod-"+id, "get", "pod", "-l", "app.kubernetes.io/name="+id, "-o", "jsonpath={.items[*].status.containerStatuses[*].restartCount}"); got != "0" {
			t.Errorf("%s restarted: %s", id, got)
		}
		if got := mustKubectl(t, "-n", "mod-"+id, "get", "deploy", id, "-o", "jsonpath={.metadata.labels}"); !strings.Contains(got, `"can.dev/module":"`+id+`"`) {
			t.Errorf("%s was not installed by can-core: %s", id, got)
		}
	}
	if got := mustKubectl(t, "-n", "mod-jobs", "get", "configmap", "jobs-config", "-o", "jsonpath={.data}"); !strings.Contains(got, "Every Module Co") || !strings.Contains(got, "careers.modules.example") {
		t.Errorf("jobs config lacks the company facts: %s", got)
	}

	for id, want := range map[string][]string{
		"finance":      {"name: Every Module Pty Ltd", "Pay by transfer", "$(NOT_EXPANDED)", "retention_years: 5", "tax_id: TAX-1"},
		"files":        {"max_file_bytes: 104857600", "quota_bytes_per_owner: 21474836480", "inline: true"},
		"provisioning": {"leaver_cutoff: end_of_day"},
	} {
		got := mustKubectl(t, "-n", "mod-"+id, "get", "configmap", id+"-config", "-o", "jsonpath={.data}")
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s config lacks %q: %s", id, w, got)
			}
		}
	}

	// can-core reads every erasure feed with the credential it generated, through the network rules.
	status := backup.DefaultSettings().DumpsDir + "/" + backup.CollectedDir + "/" + backup.CollectedStatusFile
	feeds := []string{"board", "entity", "equity", "files", "finance", "forms", "jobs", "people", "work"}
	eventually(t, 4*time.Minute, "every erasure feed to be read", func() (bool, string) {
		out := onNode(t, "cat "+status+" 2>/dev/null || true")
		var st backup.CollectStatus
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			return false, out
		}
		return slices.Equal(st.Collected, feeds) && len(st.Failed) == 0 && slices.Contains(st.NoFeed, "provisioning"), out
	})
}

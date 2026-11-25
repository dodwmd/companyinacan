package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/cluster"
)

// companyConfigMap renders the applied company.yaml as the ConfigMap can-core mounts and
// reconciles (CAN_COMPANY_FILE=/etc/can/company/company.yaml).
func companyConfigMap(content string) (string, error) {
	b, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "company", "namespace": "can-system", "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}},
		"data":     map[string]any{"company.yaml": content},
	})
	return string(b), err
}

// syncCompany writes the (already validated) company.yaml into the cluster as a ConfigMap with
// kubectl. can-core, running in the cluster, picks the change up within its reconcile interval and
// installs, upgrades or disables modules (core/server/runtime/sync.go).
func syncCompany(cmd *cobra.Command, companyFile, kubeContext string) (string, error) {
	raw, err := os.ReadFile(companyFile) //nolint:gosec // user-supplied company.yaml, already validated
	if err != nil {
		return "", err
	}
	manifest, err := companyConfigMap(string(raw))
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "can-company-*.yaml")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(manifest); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	args := []string{"apply", "-f", tmp.Name(), "--server-side", "--field-manager=can-cli", "--force-conflicts"}
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	out, err := newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: args})
	if err != nil {
		return "", fmt.Errorf("kubectl apply: %w", err)
	}
	return strings.TrimSpace(out), nil
}

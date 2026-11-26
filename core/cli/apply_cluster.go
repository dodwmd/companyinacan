package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/routing"
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

// syncRouting keeps ConfigMap can-system/can-routing (core/routing). `can up` writes it; here it is
// only written when it is missing or names another domain, and then from what the site's own route
// (also made by `can up`) says, never from assumptions: TLS and the hosts differ between a domain,
// sslip.io names and plain HTTP on the address. Without a site route nothing is written, and the
// returned note says so.
func syncRouting(cmd *cobra.Command, domain, kubeContext string) (note string, err error) {
	kubectl := func(args ...string) (string, error) {
		if kubeContext != "" {
			args = append([]string{"--context", kubeContext}, args...)
		}
		return newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: args})
	}
	have, err := kubectl("-n", routing.Namespace, "get", "configmap", routing.ConfigMapName, "--ignore-not-found", "-o", "jsonpath={.data.domain}")
	if err != nil {
		return "", fmt.Errorf("kubectl get configmap %s: %w", routing.ConfigMapName, err)
	}
	if strings.TrimSpace(have) == domain {
		return "", nil
	}
	site, err := kubectl("-n", routing.Namespace, "get", "ingressroutes.traefik.io", "site", "--ignore-not-found", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("kubectl get ingressroute site: %w", err)
	}
	if strings.TrimSpace(site) == "" {
		return "warning: ConfigMap can-system/can-routing was not written (no site route to read it from): modules are not routed until you run `can up`", nil
	}
	facts, err := routing.FromSiteRoute(domain, []byte(site))
	if err != nil {
		return "", fmt.Errorf("%w; run `can up` to write ConfigMap %s/%s", err, routing.Namespace, routing.ConfigMapName)
	}
	manifest, err := facts.Manifest()
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "can-routing-*.yaml")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(manifest); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if _, err := kubectl("apply", "-f", tmp.Name(), "--server-side", "--field-manager=can-cli", "--force-conflicts"); err != nil {
		return "", fmt.Errorf("kubectl apply: %w", err)
	}
	return "wrote ConfigMap can-system/can-routing from the site route", nil
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

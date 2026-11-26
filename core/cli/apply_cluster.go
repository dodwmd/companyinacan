package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/routing"
)

// companyConfigMap renders the applied company.yaml as the ConfigMap can-core mounts and
// reconciles (CAN_COMPANY_FILE=/etc/can/company/company.yaml). A resourceVersion makes the API
// server refuse the apply if the object changed since it was read.
func companyConfigMap(content, resourceVersion string) (string, error) {
	md := map[string]any{"name": "company", "namespace": "can-system", "labels": map[string]any{"app.kubernetes.io/part-of": "companyinacan"}}
	if resourceVersion != "" {
		md["resourceVersion"] = resourceVersion
	}
	b, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": md,
		"data":     map[string]any{"company.yaml": content},
	})
	return string(b), err
}

// syncRouting keeps ConfigMap can-system/can-routing (core/routing). `can up` writes it; here its
// hosts are only written when it is missing or names another domain, and then from what the site's
// own route (also made by `can up`) says, never from assumptions: TLS and the hosts differ between
// a domain, sslip.io names and plain HTTP on the address. Without a site route nothing is written,
// and the returned note says so. The edge limits follow dns.proxied, and stay off when `can up
// --no-edge-limits` switched them off: the ConfigMap says so (remembered, from this directory's
// state, only counts when the ConfigMap is gone).
func syncRouting(cmd *cobra.Command, domain, kubeContext string, proxied, remembered bool) (note string, err error) {
	kubectl := func(args ...string) (string, error) {
		if kubeContext != "" {
			args = append([]string{"--context", kubeContext}, args...)
		}
		return newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: args})
	}
	raw, err := kubectl("-n", routing.Namespace, "get", "configmap", routing.ConfigMapName, "--ignore-not-found", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("kubectl get configmap %s: %w", routing.ConfigMapName, err)
	}
	var have struct {
		Data map[string]string `json:"data"`
	}
	exists := strings.TrimSpace(raw) != ""
	if exists {
		if err := json.Unmarshal([]byte(raw), &have); err != nil {
			return "", fmt.Errorf("ConfigMap %s/%s is not readable: %w", routing.Namespace, routing.ConfigMapName, err)
		}
	}
	reason := have.Data["edgeLimitReason"]
	off, why := routing.EdgeLimits(reason == routing.ReasonFlag || (!exists && remembered), proxied)
	// Off for a reason nobody recorded (an older can, which may have had the flag as well as
	// dns.proxied): it stays off and unrecorded, so that removing dns.proxied does not switch it on.
	if reason == "" && have.Data["edgeLimit"] == "off" {
		off, why = true, ""
	}
	write := func(facts routing.Config, note string) (string, error) {
		facts.NoEdgeLimit, facts.NoEdgeLimitReason = off, why
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
		return note, nil
	}
	if exists && have.Data["domain"] == domain {
		wasOff := have.Data["edgeLimit"] == "off"
		if wasOff == off && reason == why {
			return "", nil
		}
		facts, err := routing.Parse(have.Data)
		if err != nil {
			return "", fmt.Errorf("%w; run `can up` to write ConfigMap %s/%s", err, routing.Namespace, routing.ConfigMapName)
		}
		note := ""
		if wasOff != off {
			word := "on"
			if off {
				word = "off"
			}
			note = "edge limits are now " + word + " for module routes; the site and admin routes follow on the next `can up`"
		}
		return write(facts, note)
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
	return write(facts, "wrote ConfigMap can-system/can-routing from the site route")
}

// companyInForce is the company.yaml the cluster has now, nil when there is none or it cannot be
// read (the notes then compare against a company without modules).
func companyInForce(cmd *cobra.Command, kubeContext string) *config.Company {
	args := []string{"-n", "can-system", "get", "configmap", "company", "--ignore-not-found", "-o", `jsonpath={.data.company\.yaml}`}
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	out, err := newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: args})
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	doc, _ := config.LoadBytes("company.yaml", []byte(out))
	if doc == nil {
		return nil
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		return nil
	}
	return c
}

// syncCompany writes the (already validated) company.yaml raw into the cluster as a ConfigMap with
// kubectl. can-core, running in the cluster, picks the change up within its reconcile interval and
// installs, upgrades or disables modules (core/server/runtime/sync.go).
func syncCompany(cmd *cobra.Command, raw []byte, kubeContext, resourceVersion string) (string, error) {
	manifest, err := companyConfigMap(string(raw), resourceVersion)
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
		if strings.Contains(err.Error()+out, "the object has been modified") {
			return "", errors.New("the company.yaml in the cluster changed while applying; run `can apply` again")
		}
		return "", fmt.Errorf("kubectl apply: %w", err)
	}
	return strings.TrimSpace(out), nil
}

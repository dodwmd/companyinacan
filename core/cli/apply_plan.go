package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// printActions writes a plan; a held step says what it waits for.
func printActions(w io.Writer, verb string, actions []reconcile.Action) {
	for _, a := range actions {
		word := verb
		switch a.Held {
		case reconcile.HeldApproval:
			word = "waiting for approval:"
		case reconcile.HeldRetry:
			word = "waiting to retry:"
		}
		fmt.Fprintf(w, "%s %s: %s (%s)\n", word, a.Kind, a.Module, a.Reason)
		for _, x := range a.Widens {
			fmt.Fprintf(w, "    newly allowed: %s\n", x)
		}
		if a.Held == reconcile.HeldApproval {
			fmt.Fprintf(w, "    the running release stays until you approve: can apply --approve %s\n", a.Module)
		}
	}
}

// approvalCodes maps each module named to the approval its held upgrade needs.
func approvalCodes(planned []reconcile.Action, modules []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range modules {
		for _, a := range planned {
			if a.Module == id && a.Held == reconcile.HeldApproval {
				out[id] = a.Approval
			}
		}
		if out[id] == "" {
			return nil, fmt.Errorf("--approve %s: no upgrade of that module is waiting for approval", id)
		}
	}
	return out, nil
}

// clusterPreview prints what the can-core in the cluster would do with this company.yaml and records
// the approvals asked for. Without --plan or --approve a can-core that cannot be asked (not up yet,
// or too old) is only noted.
func clusterPreview(cmd *cobra.Command, companyFile, kubeContext string, approve []string, required bool) error {
	exec := func(args ...string) (string, error) {
		full := append([]string{"-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "modules"}, args...)
		if kubeContext != "" {
			full = append([]string{"--context", kubeContext}, full...)
		}
		return newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: full})
	}
	data, err := os.ReadFile(companyFile) //nolint:gosec // user-supplied company.yaml
	if err != nil {
		return err
	}
	var res reconcile.Result
	out, err := exec("plan", "--company-b64", base64.StdEncoding.EncodeToString(data))
	if err == nil && strings.HasPrefix(strings.TrimSpace(out), "{") {
		err = json.Unmarshal([]byte(out), &res)
	} else if err == nil {
		err = fmt.Errorf("unexpected answer")
	}
	if err != nil {
		if required || len(approve) > 0 {
			return fmt.Errorf("reading the plan from can-core: %w", err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "note: no preview: can-core could not be asked for its plan (%v)\n", err)
		return nil
	}
	codes, err := approvalCodes(res.Actions, approve)
	if err != nil {
		return err
	}
	for i, a := range res.Actions {
		if codes[a.Module] != "" && a.Held == reconcile.HeldApproval {
			res.Actions[i].Held = ""
		}
	}
	printActions(cmd.OutOrStdout(), "planned", res.Actions)
	if len(res.Actions) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no module needs installing, upgrading or stopping")
	}
	if required {
		return nil // --plan changes nothing, so it records no approval either
	}
	for id, code := range codes {
		if _, err := exec("approve", id, code); err != nil {
			return fmt.Errorf("approving the upgrade of %s: %w", id, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "approved the upgrade of %s\n", id)
	}
	return nil
}

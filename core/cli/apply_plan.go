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
			for _, x := range a.NotApplied {
				fmt.Fprintf(w, "    NOT APPLIED: %s\n", x)
			}
			fmt.Fprintf(w, "    %s gets no change until you approve what is listed (can apply --approve %s=%s) or disable it in company.yaml, which takes it offline\n", a.Module, a.Module, a.Approval)
		}
	}
}

// notAppliedError fails an apply that leaves exposure or access in place which the operator asked
// to take away: a held module gets no change at all.
func notAppliedError(held []reconcile.Action) error {
	var lines []string
	for _, a := range held {
		if a.Held == reconcile.HeldApproval && a.Urgent {
			lines = append(lines, fmt.Sprintf("module %s: NOT APPLIED: %s", a.Module, strings.Join(a.NotApplied, "; ")))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("%s\nTwo ways out: %s", strings.Join(lines, "\n"), reconcile.WaysOut)
}

// approvalCodes reads --approve module=code arguments. The code is the one the plan showed: it
// stands for that list of what is newly allowed, so an upgrade that has changed since is refused.
func approvalCodes(planned []reconcile.Action, args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, arg := range args {
		id, code, _ := strings.Cut(arg, "=")
		want := ""
		for _, a := range planned {
			if a.Module == id && a.Held == reconcile.HeldApproval {
				want = a.Approval
			}
		}
		switch {
		case want == "":
			return nil, fmt.Errorf("--approve %s: no upgrade of that module is waiting for approval", id)
		case code == "":
			return nil, fmt.Errorf("--approve %s: give the code the plan shows (`can apply --plan`): --approve %s=<code>", id, id)
		case code != want:
			return nil, fmt.Errorf("--approve %s: the code does not match the waiting upgrade; read `can apply --plan` again", id)
		}
		out[id] = code
	}
	return out, nil
}

// clusterPreview prints what the can-core in the cluster would do with this company.yaml and records
// the approvals asked for. Without --plan or --approve a can-core that cannot be asked (not up yet,
// or too old) is only warned about. notApplied is what the apply that follows cannot deliver (see
// notAppliedError); the caller fails with it once the rest is applied.
func clusterPreview(cmd *cobra.Command, companyFile, kubeContext string, approve []string, required bool) (notApplied, err error) {
	exec := func(args ...string) (string, error) {
		full := append([]string{"-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "modules"}, args...)
		if kubeContext != "" {
			full = append([]string{"--context", kubeContext}, full...)
		}
		return newExecutor(cmd).Run(context.Background(), cluster.Cmd{Name: "kubectl", Args: full})
	}
	data, err := os.ReadFile(companyFile) //nolint:gosec // user-supplied company.yaml
	if err != nil {
		return nil, err
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
			return nil, fmt.Errorf("reading the plan from can-core: %w", err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: no preview: can-core could not be asked for its plan (%v). If a module's upgrade is waiting for approval, no change reaches it: check `can status`.\n", err)
		return nil, nil
	}
	codes, err := approvalCodes(res.Actions, approve)
	if err != nil {
		return nil, err
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
		return nil, nil // --plan changes nothing, so it records no approval either
	}
	for id, code := range codes {
		if _, err := exec("approve", id, code); err != nil {
			return nil, fmt.Errorf("approving the upgrade of %s: %w", id, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "approved the upgrade of %s\n", id)
	}
	_, held := reconcile.SplitHeld(res.Actions)
	return notAppliedError(held), nil
}

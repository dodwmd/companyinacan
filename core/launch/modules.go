package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ModuleWarnings asks can-core for the modules whose last install or upgrade failed. Helm rolls a
// failed upgrade back, so only can-core's own record shows it. A can-core too old to be asked says
// nothing.
func ModuleWarnings(ctx context.Context, kube Kube) []string {
	out, err := kube.Run(ctx, nil, "-n", SystemNS, "exec", "deploy/can-core", "--", "/can-core", "modules", "problems")
	if err != nil {
		if strings.Contains(err.Error(), "unknown command") {
			return nil
		}
		return []string{"the module states could not be read from can-core: " + err.Error()}
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var mods []struct {
		ID, State, Error string
		NotApplied       string `json:"not_applied"`
	}
	if err := json.Unmarshal([]byte(out), &mods); err != nil {
		return []string{"can-core's list of module problems cannot be read"}
	}
	var warnings []string
	for _, m := range mods {
		what := "the last install or disable failed"
		if m.State == "enabled" {
			what = "the upgrade failed and the previous release is still running"
		}
		if m.State == "pending-approval" {
			warnings = append(warnings, fmt.Sprintf("module %q: an upgrade waits for your approval and NO change reaches the module until then, exposure removal and secret rotation included. Two ways out: approve it (admin UI catalogue, or `can apply --plan` then `can apply --approve %s=<code>`), or disable the module in company.yaml (that takes it offline). It would add: %q", m.ID, m.ID, m.Error))
			if m.NotApplied != "" {
				warnings = append(warnings, fmt.Sprintf("module %q: NOT APPLIED: %q", m.ID, m.NotApplied))
			}
			continue
		}
		warnings = append(warnings, fmt.Sprintf("module %q: %s (can-core retries): %q", m.ID, what, m.Error))
	}
	return warnings
}

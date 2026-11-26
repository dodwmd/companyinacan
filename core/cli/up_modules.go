package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
)

// defaultModuleWait is how long `can up` waits for can-core to install the enabled modules.
const defaultModuleWait = 5 * time.Minute

// moduleWaitSleep is a seam for tests (nil: really sleep).
var moduleWaitSleep func(context.Context, time.Duration)

// waitForModules waits for the modules of the pushed company.yaml and returns those still not
// ready with the warnings that say so. Not being ready does not fail `can up`: can-core goes on.
func waitForModules(ctx context.Context, kube launch.Kube, company []byte, timeout time.Duration, kubeconfig string, out io.Writer) (notReady, warnings []string) {
	doc, _ := config.LoadBytes("company.yaml", company)
	if doc == nil {
		return nil, nil
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		return nil, nil
	}
	var ids []string
	for id, e := range c.Modules {
		if e.IsEnabled() {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	slices.Sort(ids)
	if timeout <= 0 {
		return nil, []string{"not waited for (--module-wait 0): can-core installs " + strings.Join(ids, ", ") + " after this command; `can status` shows failures"}
	}
	notReady, lastErr := launch.WaitModules(ctx, kube, ids, launch.ModuleWait{
		Timeout: timeout, Sleep: moduleWaitSleep, Now: lifecycle.now,
		Say: func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) },
	})
	if len(notReady) == 0 {
		return nil, nil
	}
	why := "after waiting " + timeout.String()
	if ctx.Err() != nil {
		why, ctx = "and the wait was interrupted", context.WithoutCancel(ctx)
	}
	warnings = []string{fmt.Sprintf("NOT READY YET: %s (%s; last seen: %v). can-core goes on installing and retries: check with `can status` or `kubectl --kubeconfig %s -n mod-%s get pods`",
		strings.Join(notReady, ", "), why, lastErr, kubeconfig, notReady[0])}
	// What can-core itself says went wrong with them.
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, w := range launch.ModuleWarnings(qctx, kube) {
		if slices.ContainsFunc(notReady, func(id string) bool { return strings.HasPrefix(w, fmt.Sprintf("module %q: the ", id)) }) {
			warnings = append(warnings, w)
		}
	}
	return notReady, warnings
}

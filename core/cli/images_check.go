package cli

// The image preflight of `can up` and `can apply` (epic E06d): a module whose image the server
// cannot get is refused before anything is changed, instead of failing in the cluster later.

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modimages"
)

// imagePullable asks the registry whether a reference can be pulled (a test seam).
var imagePullable = func(ctx context.Context, ref string) (bool, error) {
	return modimages.Checker{}.Pullable(ctx, ref)
}

const skipImageCheckHelp = "do not check that the images of the enabled modules are loaded or pullable (the server gets them another way: a mirror, credentials, a manual import)"

func enabledModules(c *config.Company) []string {
	var ids []string
	for id, e := range c.Modules {
		if e.IsEnabled() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// requiredImages reads the images of the modules and warns about those it cannot read.
func requiredImages(warn io.Writer, modulesDir string, ids []string) map[string][]string {
	refs, unknown := modimages.Required(modulesDir, ids)
	for _, u := range unknown {
		fmt.Fprintf(warn, "warning: images not checked for %s\n", u)
	}
	return refs
}

func sortedRefs(refs map[string][]string) []string {
	out := make([]string, 0, len(refs))
	for r := range refs {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// checkImages fails when an image is neither held (loaded already, or about to be) nor pullable.
// where says what "held" means, for the message.
func checkImages(ctx context.Context, refs map[string][]string, held func(string) bool, where string) error {
	var bad []string
	for _, ref := range sortedRefs(refs) {
		if held(ref) {
			continue
		}
		ok, err := imagePullable(ctx, ref)
		if ok && err == nil {
			continue
		}
		why := "not published, or private"
		if err != nil {
			why = "the registry could not be asked: " + err.Error()
		}
		mods := slices.Clone(refs[ref])
		sort.Strings(mods)
		bad = append(bad, fmt.Sprintf("  %s (module %s): not %s, and not pullable (%s)", ref, strings.Join(mods, ", "), where, why))
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("images of enabled modules are neither loaded nor pullable:\n%s\n"+
		"from a source checkout run `make images` (it builds every in-house image) and `make up IMAGES_FROM=build/images`, which ships what company.yaml enables; "+
		"otherwise disable the module, or pass --skip-image-check if the server gets the image another way", strings.Join(bad, "\n"))
}

// clusterImages lists what the cluster's nodes already hold, normalised.
func clusterImages(cmd *cobra.Command, kubeContext string) (map[string]bool, error) {
	have := map[string]bool{}
	for _, query := range [][]string{
		{"get", "nodes", "-o", `jsonpath={range .items[*].status.images[*]}{range .names[*]}{@}{"\n"}{end}{end}`},
		// A node reports at most 50 images; what a container runs from is certainly there.
		{"get", "pods", "-A", "-o", `jsonpath={range .items[*].status.containerStatuses[?(@.imageID)]}{.image}{"\n"}{end}`},
	} {
		if kubeContext != "" {
			query = append([]string{"--context", kubeContext}, query...)
		}
		out, err := newExecutor(cmd).Run(cmd.Context(), cluster.Cmd{Name: "kubectl", Args: query})
		if err != nil {
			return nil, fmt.Errorf("kubectl %s: %w (needed to check the module images; nothing was changed)", strings.Join(query, " "), err)
		}
		for _, ref := range strings.Fields(out) {
			have[modimages.Normalize(ref)] = true
		}
	}
	return have, nil
}

// A node lists an image some time after it was loaded: the kubelet refreshes its list every 30 s
// and then posts the node status. So an image that is not listed is looked for again this long
// (test seams).
var (
	imageListWait  = 60 * time.Second
	imageListEvery = 5 * time.Second
)

// applyImageCheck is the preflight of `can apply` against the cluster it is about to change.
func applyImageCheck(cmd *cobra.Command, modulesDir, kubeContext string, c *config.Company) error {
	refs := requiredImages(cmd.ErrOrStderr(), modulesDir, enabledModules(c))
	if len(refs) == 0 {
		return nil
	}
	have, err := clusterImages(cmd, kubeContext)
	if err != nil {
		return err
	}
	held := func(r string) bool { return have[modimages.Normalize(r)] }
	missing := map[string][]string{}
	for ref, mods := range refs {
		if !held(ref) {
			missing[ref] = mods
		}
	}
	err = checkImages(cmd.Context(), missing, func(string) bool { return false }, "on the cluster")
	if err == nil {
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "some images are not listed by the cluster yet and cannot be pulled: waiting up to %s for the node to list images loaded just now\n", imageListWait)
	for waited := time.Duration(0); waited < imageListWait; waited += imageListEvery {
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(imageListEvery):
		}
		if have, err = clusterImages(cmd, kubeContext); err != nil {
			return err
		}
		for ref := range missing {
			if held(ref) {
				delete(missing, ref)
			}
		}
		if len(missing) == 0 {
			return nil
		}
	}
	if err := checkImages(cmd.Context(), missing, held, "on the cluster"); err != nil {
		return fmt.Errorf("%w (nothing was changed)", err)
	}
	return nil
}

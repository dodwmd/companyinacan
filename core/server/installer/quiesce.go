package installer

import (
	"context"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

// removeRoutes deletes the Ingress and IngressRoute objects of a namespace.
func (in *Installer) removeRoutes(ctx context.Context, ns string) error {
	for _, kind := range []string{"IngressRoute", "Ingress"} {
		items, err := in.cfg.API.List(ctx, kind, ns, "")
		if err != nil && !kube.IsNotFound(err) {
			return err
		}
		for _, o := range items {
			if err := in.cfg.API.Delete(ctx, kind, ns, nameOf(o)); err != nil {
				return err
			}
		}
	}
	return nil
}

// quiesce turns a rendered release into its restore form: nothing runs and nothing is public.
// The next normal apply of the module puts the full rendering back (and prunes nothing it needs:
// what is dropped here is only missing, never replaced).
func quiesce(objs []kube.Object) []kube.Object {
	var out []kube.Object
	for _, o := range objs {
		switch o["kind"] {
		case "Job", "Ingress", "IngressRoute":
			continue // seeding hooks are skipped in a restore; routes appear when the company goes live
		case "Deployment", "StatefulSet", "CronJob":
			spec := asMap(o["spec"])
			if spec == nil {
				spec = map[string]any{}
				o["spec"] = spec
			}
			if o["kind"] == "CronJob" {
				spec["suspend"] = true
			} else {
				spec["replicas"] = 0
			}
		}
		out = append(out, o)
	}
	return out
}

// Package modules holds what the `can` CLI reads of the shipped modules, so that it works where
// there is no modules directory (a release archive, `go install`). Only core/cli imports it;
// can-core reads the module directories baked into its image.
package modules

import "embed"

// Shipped is, for every module, <id>/module.yaml and what the image check of `can up` and
// `can apply` reads of its chart: values.yaml and whether there are templates. Not enough to install.
//
//go:embed */module.yaml */chart/values.yaml */chart/templates
var Shipped embed.FS

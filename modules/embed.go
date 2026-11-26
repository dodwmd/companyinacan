// Package modules holds the manifests of the shipped modules, so that the `can` CLI works where
// there is no modules directory (a release archive, `go install`). Only core/cli imports it;
// can-core reads the module directories baked into its image.
package modules

import "embed"

// Manifests is <id>/module.yaml for every shipped module. Charts are not part of it.
//
//go:embed */module.yaml
var Manifests embed.FS

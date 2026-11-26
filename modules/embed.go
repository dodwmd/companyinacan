// Package modules holds what the `can` CLI reads of the shipped modules, so that it works where
// there is no modules directory (a release archive, `go install`). Only core/cli imports it;
// can-core reads the module directories baked into its image.
package modules

import "embed"

// Shipped is, for every module, <id>/module.yaml, its whole chart and its hook scripts: what `can`
// validates and plans with, and everything can-core hashes to tell whether a module changed, so the
// two agree. all: because chart helpers start with "_". Installing is can-core's alone.
//
//go:embed */module.yaml */settings.schema.json all:*/chart all:*/hooks
var Shipped embed.FS

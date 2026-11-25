// Package deploy embeds the manifest templates can-core instantiates at run time (module
// namespaces and their RBAC). The files stay plain YAML so they are reviewable and testable offline.
package deploy

import "embed"

// Templates holds templates/module-namespace.yaml and templates/module-rbac.yaml.
//
//go:embed templates/*.yaml
var Templates embed.FS

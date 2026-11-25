package deploy

import "embed"

// Stack holds the manifests `can up` applies to a fresh cluster (epic E24): the platform base,
// can-core and the site pod. They are embedded so a release binary needs no checkout. This file
// is deliberately not copied into the can-core image build (deploy/can-core/Dockerfile copies
// only embed.go and templates), so the image does not carry them.
//
//go:embed base/*.yaml can-core/can-core.yaml can-core/kustomization.yaml site/*.yaml
var Stack embed.FS

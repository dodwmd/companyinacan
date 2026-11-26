// Command can-provision is the in-house provisioning service: the `identity.provision`
// capability, a SCIM 2.0 server over the people directory plus joiner, mover and leaver workflows
// against downstream systems. See modules/provisioning/README.md and docs/provisioning.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/provision"
)

func main() {
	if modbackup.Handles(os.Args[1:]) { // `backup` and `restore` (specs/043-module-backup-hooks)
		os.Exit(modbackup.Main("can-provision", provision.Migrations(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(provision.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

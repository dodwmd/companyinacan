// Command can-forms is the in-house forms gateway: public form intake, lead storage and
// the leads capability API. See modules/forms/README.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/modbackup"
)

func main() {
	if modbackup.Handles(os.Args[1:]) { // `backup` and `restore` (specs/043-module-backup-hooks)
		os.Exit(modbackup.Main("can-forms", os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(forms.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

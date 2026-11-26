// Command can-files is the in-house files service: the `files` capability, document and file
// storage with spaces and access lists, immutable versions, share links, a virus-scan hook,
// retention, legal hold and erasure. See modules/files/README.md and docs/files.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/files"
	"github.com/dodwmd/companyinacan/core/modbackup"
)

func main() {
	if modbackup.Handles(os.Args[1:]) { // `backup` and `restore` (specs/043-module-backup-hooks)
		os.Exit(modbackup.Main("can-files", os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(files.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

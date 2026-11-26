// Command can-work is the in-house work tracker: projects, issues, workflows, sprints and
// the `work` capability API. See modules/work/README.md and docs/work-tracker.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/work"
)

func main() {
	if modbackup.Handles(os.Args[1:]) { // `backup` and `restore` (specs/043-module-backup-hooks)
		os.Exit(modbackup.Main("can-work", os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(work.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

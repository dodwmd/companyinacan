// Command can-jobs is the in-house recruiting module: job requisitions, public careers
// pages with job-board feeds, application intake and an ATS pipeline. See docs/recruiting.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/modbackup"
)

func main() {
	if modbackup.Handles(os.Args[1:]) { // `backup` and `restore` (specs/043-module-backup-hooks)
		os.Exit(modbackup.Main("can-jobs", jobs.Migrations(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(jobs.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

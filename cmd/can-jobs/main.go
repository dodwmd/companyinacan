// Command can-jobs is the in-house recruiting module: job requisitions, public careers
// pages with job-board feeds, application intake and an ATS pipeline. See docs/recruiting.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/jobs"
)

func main() {
	os.Exit(jobs.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

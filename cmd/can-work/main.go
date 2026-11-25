// Command can-work is the in-house work tracker: projects, issues, workflows, sprints and
// the `work` capability API. See modules/work/README.md and docs/work-tracker.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/work"
)

func main() {
	os.Exit(work.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// Command can-people is the in-house people service: the `directory` and `performance`
// capabilities (people records, org structure, 1:1s, goals, onboarding checklists). See
// modules/people/README.md and docs/people.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/people"
)

func main() {
	os.Exit(people.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// Command can-equity is the in-house equity service: the `equity` capability (legal entities,
// share classes, plans and pools, stakeholders, option grants with vesting, exercises, an
// append-only share ledger, cap-table views, illustrative round modelling, valuation records).
// See modules/equity/README.md and docs/equity.md.
//
// This is a tool, not legal, tax or securities advice.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/equity"
)

func main() {
	os.Exit(equity.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// Command can-finance is the in-house finance service: parties, invoices, payments, contracts,
// timesheets, expenses and the invoices, contracts and timesheets capability APIs. See
// modules/finance/README.md and docs/finance.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/modbackup"
)

func main() {
	if modbackup.Handles(os.Args[1:]) { // `backup` and `restore` (specs/043-module-backup-hooks)
		os.Exit(modbackup.Main("can-finance", finance.Migrations(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(finance.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// Command can-finance is the in-house finance service: parties, invoices, payments, contracts,
// timesheets, expenses and the invoices, contracts and timesheets capability APIs. See
// modules/finance/README.md and docs/finance.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/finance"
)

func main() {
	os.Exit(finance.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// Command can is the Company in a Can command line tool.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

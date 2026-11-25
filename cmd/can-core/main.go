// Command can-core is the in-cluster control plane.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/server/corecli"
)

func main() {
	os.Exit(corecli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

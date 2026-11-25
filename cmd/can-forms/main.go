// Command can-forms is the in-house forms gateway: public form intake, lead storage and
// the leads capability API. See modules/forms/README.md.
package main

import (
	"os"

	"github.com/dodwmd/companyinacan/core/forms"
)

func main() {
	os.Exit(forms.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

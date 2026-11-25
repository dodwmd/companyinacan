package installer

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The installer renders charts by re-executing its own binary as `render-chart`. In tests that
// binary is the test binary, so it has to answer that command line (the can-core binary does the
// same through corecli's hidden subcommand).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "render-chart" {
		var asMB, heapMB int
		for _, a := range os.Args[2:] {
			switch {
			case strings.HasPrefix(a, "--as-mb="):
				_, _ = fmt.Sscanf(a, "--as-mb=%d", &asMB)
			case strings.HasPrefix(a, "--heap-mb="):
				_, _ = fmt.Sscanf(a, "--heap-mb=%d", &heapMB)
			}
		}
		os.Exit(RunRenderChild(os.Stdin, os.Stdout, asMB, heapMB))
	}
	os.Exit(m.Run())
}

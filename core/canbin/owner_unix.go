//go:build unix

package canbin

import (
	"os"
	"syscall"
)

// ownedByUser says whether p belongs to the user running `can`. A seam for tests.
var ownedByUser = func(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Geteuid()
}

//go:build unix

package people

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwner requires the keyring file to be owned by the service's user or by root (a mounted
// Secret is root-owned with the group set from fsGroup).
func checkOwner(path string, st os.FileInfo) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := uint32(os.Geteuid()); sys.Uid != uid && sys.Uid != 0 { //nolint:gosec // uid fits uint32
		return fmt.Errorf("keyring: %s is owned by uid %d, expected %d or root", path, sys.Uid, uid)
	}
	return nil
}

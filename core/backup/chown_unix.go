//go:build unix

package backup

import (
	"os"
	"syscall"
)

// chownLike gives target the owner of the backed-up file (best effort: only
// root can do it, and a restore runs as root; pods run as non-root users).
func chownLike(target string, info os.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		_ = os.Lchown(target, int(st.Uid), int(st.Gid))
	}
}

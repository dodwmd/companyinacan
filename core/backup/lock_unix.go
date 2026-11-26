//go:build unix

package backup

import (
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

// lockFile takes an exclusive lock on f until it is closed.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) } //nolint:gosec // fd fits int

package dumps

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropPageCache asks the kernel to forget the cached pages of a file that has just been flushed.
// A dump writes a copy of every database, and those pages are charged to can-core's cgroup (and
// counted in its working set) although nothing reads them again until the host backup does.
// It is only a hint: an error costs nothing but the memory. Never use it on a file SQLite has
// open: closing a second descriptor drops the process's locks on it.
func dropPageCache(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}

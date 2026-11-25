//go:build linux

package installer

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setAddressSpaceLimit applies RLIMIT_AS to the calling process (the render child): the address
// space it already has plus extraMB. A fixed absolute limit does not work for a Go binary: the
// runtime reserves a large, version-dependent amount at start-up (the same binary fails at 512 MiB
// and starts at 1 GiB, and a limit set after start-up below the current size makes the next runtime
// allocation fatal). extraMB <= 0 means no limit.
func setAddressSpaceLimit(extraMB int) error {
	if extraMB <= 0 {
		return nil
	}
	n := uint64(extraMB)<<20 + currentAddressSpace()
	return syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: n, Max: n})
}

func currentAddressSpace() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 1 << 30 // unknown: a generous base
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 1 << 30
	}
	pages, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 1 << 30
	}
	return pages * uint64(os.Getpagesize())
}

// applyProcAttr puts the child in its own process group so the whole group can be killed.
func applyProcAttr(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGKILL) }

func exitHeapLimit() { os.Exit(5) }

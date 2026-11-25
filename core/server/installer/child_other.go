//go:build !linux

package installer

import (
	"os"
	"os/exec"
)

// Non-Linux: no address-space limit and no process group (the heap watchdog and the wall-clock kill
// still apply). can-core only ships for Linux; this keeps the package compiling elsewhere.
func setAddressSpaceLimit(int) error { return nil }

func applyProcAttr(*exec.Cmd) {}

func killGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func exitHeapLimit() { os.Exit(5) }

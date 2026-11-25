//go:build !linux

package modmgr

import (
	"context"
	"errors"
)

// DirDisk is only implemented on Linux, where can-core runs.
type DirDisk struct {
	Name string
	Path string
}

// Usage implements DiskReader.
func (DirDisk) Usage(context.Context) ([]DiskUsage, error) {
	return nil, errors.New("disk usage is not available on this platform")
}

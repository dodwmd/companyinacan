//go:build linux

package modmgr

import (
	"context"
	"syscall"
)

// DirDisk reports the usage of the filesystem holding Path (can-core's data volume).
type DirDisk struct {
	Name string
	Path string
}

// Usage implements DiskReader.
func (d DirDisk) Usage(context.Context) ([]DiskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.Path, &st); err != nil {
		return nil, err
	}
	total := st.Blocks * uint64(st.Bsize) //nolint:gosec // Bsize is positive
	free := st.Bavail * uint64(st.Bsize)  //nolint:gosec // Bsize is positive
	return []DiskUsage{{Name: d.Name, TotalBytes: total, UsedBytes: total - min(free, total)}}, nil
}

//go:build !unix

package jobs

import "os"

func checkOwner(string, os.FileInfo) error { return nil }

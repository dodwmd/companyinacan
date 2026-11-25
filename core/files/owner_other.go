//go:build !unix

package files

import "os"

func checkOwner(string, os.FileInfo) error { return nil }

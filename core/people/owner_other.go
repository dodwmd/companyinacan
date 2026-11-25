//go:build !unix

package people

import "os"

func checkOwner(string, os.FileInfo) error { return nil }

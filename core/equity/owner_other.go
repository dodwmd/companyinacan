//go:build !unix

package equity

import "os"

func checkOwner(string, os.FileInfo) error { return nil }

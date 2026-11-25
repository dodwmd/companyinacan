//go:build !unix

package board

import "os"

func checkOwner(string, os.FileInfo) error { return nil }

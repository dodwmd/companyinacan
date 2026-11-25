//go:build !unix

package backup

import "os"

func chownLike(string, os.FileInfo) {}

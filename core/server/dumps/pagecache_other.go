//go:build !linux

package dumps

import "os"

func dropPageCache(*os.File) {}

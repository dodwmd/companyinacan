//go:build !unix

package backup

import "os"

const noFollow = 0

// The host that runs the backup is Linux; elsewhere the in-process mutex is all there is.
func lockFile(*os.File) error { return nil }

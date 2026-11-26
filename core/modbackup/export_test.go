package modbackup

import "time"

// SetRetryPause shortens the wait for a missing file; it returns the undo.
func SetRetryPause(d time.Duration) func() {
	old := retryPause
	retryPause = d
	return func() { retryPause = old }
}

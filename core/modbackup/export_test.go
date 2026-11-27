package modbackup

import "time"

// SetRetryPause shortens the wait for a missing file; it returns the undo.
func SetRetryPause(d time.Duration) func() {
	old := retryPause
	retryPause = d
	return func() { retryPause = old }
}

// SealedColumn is one encrypted column of a service's export.
type SealedColumn struct{ Table, Column, Row, Prefix, Suffix string }

// SealedColumns lists a service's encrypted columns as the export opens them.
func SealedColumns(binary string) []SealedColumn {
	var out []SealedColumn
	for _, t := range exportTables[binary] {
		for col, s := range t.Sealed {
			out = append(out, SealedColumn{t.Name, col, s.Row, s.Prefix, s.Suffix})
		}
	}
	return out
}

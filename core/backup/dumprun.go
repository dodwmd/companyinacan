package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DumpRunName is the file in the dumps directory recording how the last dump run ended.
const DumpRunName = "LAST-RUN"

// DumpRun is the outcome of one dump run.
type DumpRun struct {
	RunID string    `json:"run_id"`
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	// Skipped are the modules this run did not dump although the run succeeded.
	Skipped []SkippedModule `json:"skipped,omitempty"`
}

// SkippedModule is a module left out of a dump run: a disabled module whose hook cannot run, or
// one that suddenly reports no data.
type SkippedModule struct {
	ID       string `json:"id"`
	Reason   string `json:"reason"`
	Database string `json:"database,omitempty"`
	NoData   bool   `json:"no_data,omitempty"`
	Kept     bool   `json:"kept,omitempty"` // an earlier dump is still in the dumps directory
}

// DatabaseServer reports whether the module's data lives in a database server, which the raw
// volume copy does not back up.
func (s SkippedModule) DatabaseServer() bool {
	return s.Database == "postgres" || s.Database == "mariadb"
}

// Message says what the skip means for the operator.
func (s SkippedModule) Message() string {
	m := fmt.Sprintf("module %q was NOT dumped: %s.", s.ID, s.Reason)
	switch {
	case s.DatabaseServer():
		m += " Its DATABASE IS NOT BACKED UP (copies of a running database server's files are not a usable backup)."
	case s.NoData:
		m += " Nothing new of it is backed up."
	default:
		m += " Only its raw volume files are in the backup."
	}
	if s.Kept {
		m += " An earlier dump is kept."
	}
	return m
}

// oneLine folds s onto one line of at most max bytes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// WriteDumpRun records r in the dumps directory (one line, atomic, 0600).
func WriteDumpRun(dir string, r DumpRun) error {
	r.Error = oneLine(r.Error, 600)
	skipped := append([]SkippedModule(nil), r.Skipped...)
	for i := range skipped {
		skipped[i].Reason = oneLine(skipped[i].Reason, 200)
	}
	r.Skipped = skipped
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+DumpRunName+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, DumpRunName))
}

// ReadDumpRun reads the record; the error is os.ErrNotExist before the first run.
func ReadDumpRun(dir string) (DumpRun, error) {
	b, err := os.ReadFile(filepath.Join(dir, DumpRunName)) //nolint:gosec // operator-chosen dir
	if err != nil {
		return DumpRun{}, err
	}
	return ParseDumpRun(b)
}

// ParseDumpRun parses the content of the DumpRunName file.
func ParseDumpRun(b []byte) (DumpRun, error) {
	var r DumpRun
	err := json.Unmarshal(b, &r)
	if err == nil && r.RunID == "" {
		err = errors.New("not a dump run record")
	}
	return r, err
}

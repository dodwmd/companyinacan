package backup

import (
	"encoding/json"
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
}

// WriteDumpRun records r in the dumps directory (one line, atomic, 0600).
func WriteDumpRun(dir string, r DumpRun) error {
	r.Error = strings.Join(strings.Fields(r.Error), " ")
	if len(r.Error) > 600 {
		r.Error = r.Error[:600]
	}
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
	var r DumpRun
	b, err := os.ReadFile(filepath.Join(dir, DumpRunName)) //nolint:gosec // operator-chosen dir
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

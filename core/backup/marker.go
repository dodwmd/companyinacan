package backup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Marker is the completion marker can-core writes after a consistent dump run.
// can-core deletes it at the start of a run, so its absence means "dumps are
// not consistent"; the timer refuses a marker older than the configured age.
// The format is two key=value lines so the backup script can read it with sed.
type Marker struct {
	RunID      string
	FinishedAt time.Time
}

// clockSkew is how far into the future a marker may be dated.
const clockSkew = 5 * time.Minute

// WriteMarker writes the marker atomically with mode 0600.
func WriteMarker(path string, m Marker) error {
	body := fmt.Sprintf("run_id=%s\nfinished_at=%d\n", m.RunID, m.FinishedAt.Unix())
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadMarker parses a marker file.
func ReadMarker(path string) (Marker, error) {
	f, err := os.Open(path) //nolint:gosec // operator-chosen path
	if err != nil {
		return Marker{}, fmt.Errorf("completion marker: %w", err)
	}
	defer func() { _ = f.Close() }()
	var m Marker
	var haveTime bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "run_id":
			m.RunID = v
		case "finished_at":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return Marker{}, fmt.Errorf("completion marker: bad finished_at %q", v)
			}
			m.FinishedAt, haveTime = time.Unix(n, 0), true
		}
	}
	if m.RunID == "" || !haveTime {
		return Marker{}, errors.New("completion marker: missing run_id or finished_at")
	}
	return m, nil
}

// Check reports whether the marker is acceptable at now.
func (m Marker) Check(now time.Time, maxAge time.Duration) error {
	age := now.Sub(m.FinishedAt)
	switch {
	case age < -clockSkew:
		return fmt.Errorf("completion marker %s is dated in the future", m.RunID)
	case age > maxAge:
		return fmt.Errorf("completion marker %s is %s old (limit %s): can-core did not finish a dump run", m.RunID, age.Round(time.Minute), maxAge)
	}
	return nil
}

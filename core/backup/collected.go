package backup

// Erasures collected by can-core (specs/048-erasure-collection). can-core cannot write the root-owned
// ledger of the host, so it appends the modules' tombstones to a ledger of its own inside the dumps
// directory (a dot directory: never part of the dumps, the manifest or a snapshot) and `can backup
// ledger-import`, run as root by the backup script, adds them to the host ledger.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The directory under the dumps directory and the files in it.
const (
	CollectedDir        = ".erasure-ledger"
	CollectedLedger     = "collected.jsonl"
	CollectedStatusFile = "status.json"
)

// CollectedMaxAge is how old can-core's last collection may be before the host reports it.
const CollectedMaxAge = 2 * time.Hour

const (
	maxCollected = 64 << 20
	maxStatus    = 1 << 20
)

// CollectStatus is what can-core's last collection did. It names modules, never subjects.
type CollectStatus struct {
	At        time.Time `json:"at"`
	Collected []string  `json:"collected,omitempty"` // erasure feeds read completely
	// Failed are enabled modules with a feed that could not be read: their newest erasures may be missing.
	Failed map[string]string `json:"failed,omitempty"`
	// NotAsked are modules with a feed that are not running: what was collected earlier stays.
	NotAsked []string `json:"not_asked,omitempty"`
	// NoFeed hold data classes but have no erasure feed: NOTHING erased in them is recorded.
	NoFeed []string `json:"no_feed,omitempty"`
	// Rejected counts tombstones refused: a subject that is not an opaque id, or a data class the
	// module does not declare.
	Rejected int `json:"rejected,omitempty"`
}

// ValidTombstone reports whether the ledger takes this subject and class.
func ValidTombstone(subject, class string) bool {
	return subjectRe.MatchString(subject) && classRe.MatchString(class)
}

// readPlain reads a regular file that is not a symlink, up to limit bytes.
func readPlain(p string, limit int64) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0) //nolint:gosec // fixed names under the dumps directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s is larger than %d bytes", p, limit)
	}
	return b, err
}

// ReadCollected returns the tombstones can-core wrote into dir. can-core can write there and the
// reader runs as root, so neither the directory nor the file may be a link. A missing directory or
// file is no entries.
func ReadCollected(dir string) ([]Entry, error) {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a plain directory", dir)
	}
	raw, err := readPlain(filepath.Join(dir, CollectedLedger), maxCollected)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	es, _, err := parseLedger(raw)
	if err != nil {
		return nil, fmt.Errorf("erasures collected by can-core: %w", err)
	}
	return es, nil
}

// ImportReport is what ImportCollected did.
type ImportReport struct {
	Found, Added int
	Status       *CollectStatus // nil when can-core wrote none
	// Problems are reasons the ledger may lack erasures that the operator can do something about.
	Problems []string
	// Notes are standing limits, said every time.
	Notes []string
}

// ImportCollected appends what can-core collected in dir to the ledger. The import itself failing is
// an error; an incomplete or old collection is reported in Problems after what there is was imported.
func ImportCollected(dir string, l *Ledger, now time.Time) (ImportReport, error) {
	var rep ImportReport
	es, err := ReadCollected(dir)
	if err != nil {
		return rep, err
	}
	rep.Found = len(es)
	if rep.Added, err = l.AppendMany(es); err != nil {
		return rep, err
	}
	raw, err := readPlain(filepath.Join(dir, CollectedStatusFile), maxStatus)
	if errors.Is(err, fs.ErrNotExist) {
		rep.Problems = append(rep.Problems, "can-core has not recorded a collection of erasures (is it running this release?)")
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	var st CollectStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return rep, fmt.Errorf("can-core's collection status: %w", err)
	}
	rep.Status = &st
	if age := now.Sub(st.At); age > CollectedMaxAge {
		rep.Problems = append(rep.Problems, fmt.Sprintf("can-core last collected erasures %s ago: newer erasures are NOT in the ledger", age.Round(time.Minute)))
	}
	var failed []string
	for m := range st.Failed {
		failed = append(failed, m)
	}
	sort.Strings(failed)
	if len(failed) > 0 {
		rep.Problems = append(rep.Problems, "the erasures of these modules could not be read, newer ones are NOT in the ledger: "+strings.Join(failed, ", "))
	}
	if st.Rejected > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d tombstones were refused (a subject that is not an opaque id, or a data class the module does not declare): they are NOT in the ledger (see can-core's log)", st.Rejected))
	}
	if len(st.NoFeed) > 0 {
		rep.Notes = append(rep.Notes, "no erasure feed, so nothing erased in them is recorded: "+strings.Join(st.NoFeed, ", "))
	}
	if len(st.NotAsked) > 0 {
		rep.Notes = append(rep.Notes, "not running, so not asked: "+strings.Join(st.NotAsked, ", "))
	}
	return rep, nil
}

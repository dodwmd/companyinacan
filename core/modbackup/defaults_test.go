package modbackup

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/board"
	"github.com/dodwmd/companyinacan/core/equity"
	"github.com/dodwmd/companyinacan/core/files"
	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/provision"
	"github.com/dodwmd/companyinacan/core/work"
)

// With no config file the backup reads the same paths the service would write.
func TestDefaultsMatchTheServices(t *testing.T) {
	check := func(binary, db, blobs string) {
		t.Helper()
		l, err := specs[binary].loadLayout("/nonexistent", false)
		if err != nil {
			t.Fatal(err)
		}
		if l.db != db || l.blobDir != blobs {
			t.Errorf("%s: %q %q, the service uses %q %q", binary, l.db, l.blobDir, db, blobs)
		}
	}
	w, err := work.LoadConfig("/nonexistent", false)
	if err != nil {
		t.Fatal(err)
	}
	check("can-work", w.DBPath, "")
	j, err := jobs.LoadConfig("/nonexistent", false)
	if err != nil {
		t.Fatal(err)
	}
	check("can-jobs", j.DBPath, j.ResumeDir)
	f, err := finance.LoadConfig("/nonexistent", false)
	if err != nil {
		t.Fatal(err)
	}
	check("can-finance", f.DBPath, "")
	fl := files.DefaultConfig()
	check("can-files", fl.DBPath, fl.Backend.Local.Dir)
	// These loaders may require other keys; where they accept an empty document the default counts.
	if c, err := people.ParseConfig([]byte("{}")); err == nil {
		check("can-people", c.DBPath, "")
	}
	if c, err := equity.ParseConfig([]byte("{}")); err == nil {
		check("can-equity", c.DBPath, "")
	}
	if c, err := board.ParseConfig([]byte("{}")); err == nil {
		check("can-board", c.DBPath, "")
	}
	if c, err := provision.ParseConfig([]byte("{}")); err == nil {
		check("can-provision", c.DBPath, "")
	}
	check("can-forms", "/data/forms.db", "")
}

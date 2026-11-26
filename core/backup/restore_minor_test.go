package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A company.yaml that differs is refused before anything is recorded, so another --dir works.
func TestRestoreDifferingCompanyYAMLDoesNotPinTheDirectory(t *testing.T) {
	e := newRestoreEnv(t)
	e.o.DirChosen = true
	if err := os.MkdirAll(e.o.CompanyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.o.CompanyDir, "company.yaml"), []byte("schema: 1\nname: Mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("err = %v", err)
	}
	if st := e.read("restore.json"); e.read("var/lib/can/dumps/company.yaml") != "" || strings.Contains(st, "claimed") || strings.Contains(st, "company_dir") {
		t.Error("the refusal left state or dumps behind")
	}
	e.o.CompanyDir = e.o.CompanyDir + "-other"
	if err := e.run(); err != nil {
		t.Fatalf("another --dir is refused: %v", err)
	}
}

// Whatever step of a volume copy fails, the guard stays, and every file is flushed before the
// copy counts.
func TestVolumeCopyKeepsItsGuardAndFlushesFiles(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "vol")
	if err := os.WriteFile(filepath.Join(src, "a.db"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := syncFile
	defer func() { syncFile = real }()
	synced := 0
	syncFile = func(f *os.File) error {
		synced++
		if _, err := os.Stat(filepath.Join(dst, copyMarker)); err != nil {
			t.Error("the guard is not there during the copy")
		}
		return errors.New("disk gone")
	}
	o := &Restore{}
	if err := o.copyVolume(src, dst, false); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("a file that could not be flushed counts as copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, copyMarker)); err != nil || synced != 1 {
		t.Fatalf("guard: %v, synced %d", err, synced)
	}
	syncFile = real
	if err := o.copyVolume(src, dst, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

// What the server's ledger holds by the time the restore goes live is merged, not overwritten;
// and what the can-core being overwritten had collected survives the emptying of its dumps.
func TestRestoreKeepsTheServersOwnErasures(t *testing.T) {
	e := newRestoreEnv(t)
	ledger := &Ledger{Path: filepath.Join(e.root, e.o.LedgerPath)}
	if err := os.MkdirAll(filepath.Dir(ledger.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	e.r.failOnce["complete"] = errors.New("x")
	if err := e.run(); err == nil {
		t.Fatal("expected the live step to fail")
	}
	if _, _, err := ledger.Append("late_1", "directory-basic"); err != nil {
		t.Fatal(err)
	}
	if err := e.run(); err != nil {
		t.Fatal(err, e.out.String())
	}
	es, err := ledger.Read()
	if err != nil || len(es) != 3 {
		t.Fatalf("%v %+v", err, es)
	}
	if !strings.Contains(e.out.String(), "late_1/directory-basic") {
		t.Error("an entry that was not replayed is not reported")
	}

	over := newRestoreEnv(t)
	dir := filepath.Join(over.root, tDumps, CollectedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&Ledger{Path: filepath.Join(dir, CollectedLedger)}).Append("collected_1", "directory-basic"); err != nil {
		t.Fatal(err)
	}
	over.o.ConfirmOverwrite = over.o.Kit.Company.Name
	if err := over.run(); err != nil {
		t.Fatal(err, over.out.String())
	}
	got := over.read("var/lib/can/erasure-ledger.jsonl")
	if !strings.Contains(got, "collected_1") {
		t.Errorf("the erasure collected on the overwritten server is gone: %q", got)
	}
}

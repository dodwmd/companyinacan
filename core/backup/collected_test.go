package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func collectedDir(t *testing.T, subjects ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), CollectedDir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l := &Ledger{Path: filepath.Join(dir, CollectedLedger)}
	for _, s := range subjects {
		if _, _, err := l.Append(s, "directory-basic"); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestImportCollected(t *testing.T) {
	dir := collectedDir(t, "pers_1", "pers_2")
	host := &Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl")}
	if _, _, err := host.Append("manual_1", "lead-submission"); err != nil {
		t.Fatal(err)
	}
	status := func(at time.Time) {
		if err := os.WriteFile(filepath.Join(dir, CollectedStatusFile), fmt.Appendf(nil, `{"at":%q}`, at.Format(time.RFC3339)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	status(now)
	for i, want := range []int{2, 0} {
		rep, err := ImportCollected(dir, host, now)
		if err != nil || rep.Added != want || len(rep.Problems) != 0 {
			t.Fatalf("run %d: %v %+v", i, err, rep)
		}
	}
	es, err := host.Read()
	if err != nil || len(es) != 3 {
		t.Fatalf("%v %+v", err, es)
	}
	status(now.Add(-3 * time.Hour))
	if rep, err := ImportCollected(dir, host, now); err != nil || len(rep.Problems) != 1 {
		t.Fatalf("a stale collection is not reported: %v %+v", err, rep)
	}
}

// The importer runs as root over a directory can-core writes: links and edits are refused.
func TestImportCollectedRefusesLinksAndTampering(t *testing.T) {
	other := collectedDir(t, "pers_1")
	host := &Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl")}
	linkedDir := filepath.Join(t.TempDir(), CollectedDir)
	if err := os.Symlink(other, linkedDir); err != nil {
		t.Skip("no symlinks")
	}
	linkedFile := collectedDir(t)
	if err := os.Symlink(filepath.Join(other, CollectedLedger), filepath.Join(linkedFile, CollectedLedger)); err != nil {
		t.Fatal(err)
	}
	edited := collectedDir(t, "pers_1", "pers_2")
	p := filepath.Join(edited, CollectedLedger)
	b, err := os.ReadFile(p) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "pers_1", "pers_9", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"linked dir": linkedDir, "linked file": linkedFile, "edited": edited} {
		if rep, err := ImportCollected(dir, host, time.Now()); err == nil {
			t.Errorf("%s accepted: %+v", name, rep)
		}
	}
	if es, _ := host.Read(); len(es) != 0 {
		t.Errorf("host ledger written: %+v", es)
	}
}

func TestAppendManyRefusesAllWhenOneIsBadAndKeepsTheChainAcrossWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.jsonl")
	l := &Ledger{Path: path}
	if _, err := l.AppendMany([]Entry{{Subject: "a", Class: "c", Date: "2026-01-01"}, {Subject: "x@y", Class: "c", Date: "2026-01-01"}}); err == nil {
		t.Fatal("an e-mail address was accepted")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("something was written")
	}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := &Ledger{Path: path} // its own mutex, as another process would have
			for i := range 20 {
				if _, _, err := mine.Append(fmt.Sprintf("w%d_%d", w, i), "c"); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if es, err := l.Read(); err != nil || len(es) != 80 {
		t.Fatalf("%v, %d entries", err, len(es))
	}
}

func TestScriptRunImportsCollectedErasuresBeforeSync(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	if err := os.Mkdir(filepath.Join(e.s.DumpsDir, CollectedDir), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(e.s.LedgerSource, []byte("{}\n"), 0o600)
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	c := e.calledRestic()
	imp, sync := strings.Index(c, "can backup ledger-import"), strings.Index(c, "can backup ledger-sync")
	if imp < 0 || sync < imp {
		t.Fatalf("import must come before sync:\n%s", c)
	}
	_ = os.WriteFile(e.dir+"/can-rc", []byte("1"), 0o600)
	if code, _ := e.run(t); code != ExitLedgerFailed {
		t.Errorf("a failed import: code %d", code)
	}
}

func TestScriptRunLedgerOnly(t *testing.T) {
	e := newScriptEnv(t, "0") // no marker: a full run would fail with 10
	_ = os.WriteFile(e.s.LedgerSource, []byte("{}\n"), 0o600)
	b, err := os.ReadFile(e.script)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := e.dir + "/only"
	if err := os.WriteFile(wrapper, []byte(strings.Replace(string(b), `"${1:-}"`, `"--ledger-only"`, 1)), 0o755); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	e.script = wrapper
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if c := e.calledRestic(); !strings.Contains(c, "can backup ledger-sync") || strings.Contains(c, "restic ") {
		t.Errorf("ledger only:\n%s", c)
	}
}

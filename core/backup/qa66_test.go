package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Entries written before entries had a module and a time keep their hash and are always replayed.
func TestLedgerKeepsOldEntriesAndRecordsLaterErasures(t *testing.T) {
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	l := &Ledger{Path: filepath.Join(t.TempDir(), "l.jsonl"), Now: func() time.Time { return now }}
	add := func(want bool) {
		t.Helper()
		if _, added, err := l.Append("s1", "c"); err != nil || added != want {
			t.Fatal(added, err)
		}
	}
	add(true)
	add(false)
	now = now.Add(48 * time.Hour) // erased again, after more data was created
	add(true)
	if es, err := l.Read(); err != nil || len(es) != 2 || es[1].At != "2026-06-03T10:00:00Z" {
		t.Fatalf("%v %+v", err, es)
	}
	old := Entry{Seq: 1, Subject: "a", Class: "c", Date: "2026-01-01"}
	old.Hash = old.computeHash()
	b := `{"seq":1,"subject":"a","class":"c","date":"2026-01-01","prev":"","hash":"` + old.Hash + `"}` + "\n"
	if es, _, err := parseLedger([]byte(b)); err != nil || len(es) != 1 {
		t.Fatalf("%v %+v", err, es)
	}
}

// The frequent sync must not call the webhook every quarter of an hour for one lasting fault.
func TestScriptRunLedgerOnlyAlertsOnce(t *testing.T) {
	e := newScriptEnv(t, "0")
	_ = os.WriteFile(e.s.LedgerSource, []byte("{}\n"), 0o600)
	_ = os.WriteFile(e.s.ConfigDir+"/"+WebhookFileName, []byte("https://hooks.example.invalid/x\n"), 0o600)
	b, err := os.ReadFile(e.script)
	if err != nil {
		t.Fatal(err)
	}
	only := e.dir + "/only"
	if err := os.WriteFile(only, []byte(strings.Replace(string(b), `"${1:-}"`, `"--ledger-only"`, 1)), 0o755); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	e.script = only
	posts := func() int { return strings.Count(e.calledRestic(), "curl ") }
	_ = os.WriteFile(e.dir+"/can-rc", []byte("1"), 0o600)
	for range 3 {
		if code, out := e.run(t); code != ExitLedgerFailed {
			t.Fatalf("code %d\n%s", code, out)
		}
	}
	if posts() != 1 {
		t.Fatalf("%d webhook calls for one fault:\n%s", posts(), e.calledRestic())
	}
	// after a run that succeeds, the same fault is news again
	_ = os.WriteFile(e.dir+"/can-rc", []byte("0"), 0o600)
	e.run(t)
	_ = os.WriteFile(e.dir+"/can-rc", []byte("1"), 0o600)
	e.run(t)
	if posts() != 2 {
		t.Fatalf("%d webhook calls", posts())
	}
}

// A can binary from before ledger-import would silently leave the erasures behind: it is named.
func TestScriptRunNamesAnOldCanBinary(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	if err := os.Mkdir(filepath.Join(e.s.DumpsDir, CollectedDir), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(e.dir+"/can-help", []byte("ledger-add ledger-sync\n"), 0o600)
	code, out := e.run(t)
	if code != ExitLedgerFailed || !strings.Contains(out, "too old") {
		t.Fatalf("code %d\n%s", code, out)
	}
	if strings.Contains(e.calledRestic(), "can backup ledger-import") {
		t.Error("called a command the binary does not have")
	}
}

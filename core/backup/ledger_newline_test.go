package backup

import (
	"bytes"
	"os"
	"testing"
)

// A crash can leave a whole entry without its newline: the next append must not join the two.
func TestLedgerAppendAfterUnterminatedLine(t *testing.T) {
	l := newLedger(t)
	if _, _, err := l.Append("s1", "c"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Path, bytes.TrimSuffix(raw, []byte("\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, added, err := l.Append("s1", "c"); err != nil || added {
		t.Fatalf("the unterminated entry counts: %v %v", added, err)
	}
	for _, s := range []string{"s2", "s3"} {
		if _, added, err := l.Append(s, "c"); err != nil || !added {
			t.Fatalf("%s: %v %v", s, added, err)
		}
	}
	es, err := l.Read()
	if err != nil || len(es) != 3 || es[1].Subject != "s2" {
		t.Fatalf("%v %+v", err, es)
	}
	raw, err = os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte("\n")); n != 3 || bytes.Contains(raw, []byte("}{")) || bytes.Contains(raw, []byte("\n\n")) {
		t.Fatalf("one entry per line: %q", raw)
	}
}

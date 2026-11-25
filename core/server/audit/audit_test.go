package audit

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

func newLog(t *testing.T) (*Log, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tick := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { tick = tick.Add(time.Second); return tick }
	return New(db), db
}

func fill(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(context.Background(), Entry{Actor: "admin", Action: "module.enable", Target: "crm", Detail: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppendChainsAndVerifies(t *testing.T) {
	l, _ := newLog(t)
	fill(t, l, 5)
	res, err := l.Verify(context.Background(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Violation != nil || res.Entries != 5 || res.Head.Seq != 5 || len(res.Head.Hash) != 64 {
		t.Fatalf("%+v", res)
	}
	recs, _ := l.List(context.Background(), 0, 10)
	if recs[0].PrevHash != strings.Repeat("0", 64) || recs[1].PrevHash != recs[0].Hash {
		t.Fatalf("chain not linked: %+v", recs[:2])
	}
}

func TestEmptyLogVerifies(t *testing.T) {
	l, _ := newLog(t)
	res, err := l.Verify(context.Background(), VerifyOptions{})
	if err != nil || res.Violation != nil || res.Entries != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	cases := []struct {
		name string
		mut  string
		kind string
	}{
		{"modified detail", `UPDATE audit_log SET detail='{"i":99}' WHERE seq=3`, KindHashMismatch},
		{"modified actor", `UPDATE audit_log SET actor='evil' WHERE seq=2`, KindHashMismatch},
		{"modified time", `UPDATE audit_log SET time=time+1 WHERE seq=4`, KindHashMismatch},
		{"deleted middle entry", `DELETE FROM audit_log WHERE seq=3`, KindSeqGap},
		{"deleted and renumbered", `DELETE FROM audit_log WHERE seq=3; UPDATE audit_log SET seq=seq-1 WHERE seq>3`, KindChainBreak},
		{"deleted first entry", `DELETE FROM audit_log WHERE seq=1`, KindSeqGap},
		{"reordered (swap seq)", `UPDATE audit_log SET seq=-1 WHERE seq=2; UPDATE audit_log SET seq=2 WHERE seq=3; UPDATE audit_log SET seq=3 WHERE seq=-1`, KindChainBreak},
		{"rehashed entry without fixing successors", `UPDATE audit_log SET hash='` + strings.Repeat("a", 64) + `' WHERE seq=2`, KindHashMismatch},
		{"forged prev_hash", `UPDATE audit_log SET prev_hash='` + strings.Repeat("b", 64) + `' WHERE seq=4`, KindChainBreak},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, db := newLog(t)
			fill(t, l, 5)
			if _, err := db.Exec(tc.mut); err != nil {
				t.Fatal(err)
			}
			res, err := l.Verify(context.Background(), VerifyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Violation == nil {
				t.Fatal("tampering not detected")
			}
			if res.Violation.Kind != tc.kind {
				t.Fatalf("kind = %s (%s), want %s", res.Violation.Kind, res.Violation.Message, tc.kind)
			}
		})
	}
}

func TestVerifyDetectsTruncationAgainstAnchoredHead(t *testing.T) {
	l, db := newLog(t)
	fill(t, l, 5)
	head, err := l.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Without an anchor, dropping the tail is undetectable by the chain alone...
	if _, err := db.Exec(`DELETE FROM audit_log WHERE seq>3`); err != nil {
		t.Fatal(err)
	}
	if res, _ := l.Verify(context.Background(), VerifyOptions{}); res.Violation != nil {
		t.Fatalf("unexpected: %+v", res.Violation)
	}
	// ...but an externally recorded head catches it.
	res, _ := l.Verify(context.Background(), VerifyOptions{Expected: &head})
	if res.Violation == nil || res.Violation.Kind != KindHeadMismatch {
		t.Fatalf("truncation not detected: %+v", res.Violation)
	}
}

func TestVerifyAnchoredHeadMatchesWhenUntouched(t *testing.T) {
	l, _ := newLog(t)
	fill(t, l, 3)
	head, _ := l.Head(context.Background())
	fill(t, l, 2) // log legitimately grew past the anchor
	res, _ := l.Verify(context.Background(), VerifyOptions{Expected: &head})
	if res.Violation != nil {
		t.Fatalf("%+v", res.Violation)
	}
}

func TestParseHead(t *testing.T) {
	h, err := ParseHead("7:" + strings.Repeat("c", 64))
	if err != nil || h.Seq != 7 {
		t.Fatalf("%+v %v", h, err)
	}
	for _, bad := range []string{"", "7", "x:abc", "7:short", "0:" + strings.Repeat("c", 64)} {
		if _, err := ParseHead(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestConcurrentAppendsStayChained(t *testing.T) {
	l, _ := newLog(t)
	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func() {
			fill(t, l, 10)
			done <- struct{}{}
		}()
	}
	for g := 0; g < 4; g++ {
		<-done
	}
	res, err := l.Verify(context.Background(), VerifyOptions{})
	if err != nil || res.Violation != nil || res.Entries != 40 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestExportJSONLines(t *testing.T) {
	l, _ := newLog(t)
	fill(t, l, 2)
	var b bytes.Buffer
	if err := l.Export(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(b.String(), "\n"); got != 2 || !strings.Contains(b.String(), `"action":"module.enable"`) {
		t.Fatalf("%q", b.String())
	}
}

package audit

import (
	"context"
	"testing"
)

// A long log must not hold the store's only connection for the whole walk: a concurrent query has
// to get in between chunks.
func TestVerifyReleasesTheConnectionBetweenChunks(t *testing.T) {
	l, db := newLog(t)
	fill(t, l, 3000)
	// The store has a single connection. After every page, while Verify is still walking, that
	// connection must be free: neither a pinned db.Conn, nor a transaction, nor open rows. This is
	// checked synchronously from inside the walk, so it depends on no timing or scheduling.
	pages := 0
	res, err := l.Verify(context.Background(), VerifyOptions{Chunk: 100, afterPage: func() {
		pages++
		if n := db.Stats().InUse; n != 0 {
			t.Fatalf("after page %d Verify still holds %d connection(s)", pages, n)
		}
	}})
	if err != nil || !res.OK() || res.Entries != 3000 {
		t.Fatalf("verify: %+v %v", res, err)
	}
	if pages < 30 {
		t.Fatalf("walked in %d pages, want at least 30 of 100 records", pages)
	}
}

func TestVerifyChunkedStillFindsEveryKindOfDamage(t *testing.T) {
	for _, chunk := range []int{1, 3, 7, 100} {
		l, db := newLog(t)
		fill(t, l, 10)
		if res, _ := l.Verify(context.Background(), VerifyOptions{Chunk: chunk}); !res.OK() || res.Entries != 10 {
			t.Fatalf("chunk %d clean: %+v", chunk, res)
		}
		if _, err := db.Exec(`DELETE FROM audit_log WHERE seq=6`); err != nil {
			t.Fatal(err)
		}
		res, _ := l.Verify(context.Background(), VerifyOptions{Chunk: chunk})
		if res.OK() || res.Violation.Kind != KindSeqGap || res.Violation.Seq != 7 {
			t.Fatalf("chunk %d: deletion across a page boundary not found: %+v", chunk, res)
		}
	}
}

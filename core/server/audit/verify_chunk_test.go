package audit

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// A long log must not hold the store's only connection for the whole walk: a concurrent query has
// to get in between chunks.
func TestVerifyReleasesTheConnectionBetweenChunks(t *testing.T) {
	l, db := newLog(t)
	// The store has a single connection, so a concurrent query can only be served while Verify is
	// still walking if Verify gives the connection back between chunks. That is asserted by
	// counting, with no wall-clock threshold: if Verify held the connection for the whole walk the
	// count would be zero.
	fill(t, l, 3000)
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	started := make(chan struct{})
	var servedWhileVerifying atomic.Int64
	var verifying atomic.Bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var n int
			_ = db.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
			if verifying.Load() {
				servedWhileVerifying.Add(1)
			}
			runtime.Gosched()
		}
	}()
	<-started
	verifying.Store(true)
	res, err := l.Verify(ctx, VerifyOptions{Chunk: 100})
	verifying.Store(false)
	close(stop)
	wg.Wait()
	if err != nil || !res.OK() || res.Entries != 3000 {
		t.Fatalf("verify: %+v %v", res, err)
	}
	if servedWhileVerifying.Load() == 0 {
		t.Fatal("no concurrent query was served while Verify was walking 30 chunks: it holds the connection")
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

package audit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A long log must not hold the store's only connection for the whole walk: a concurrent query has
// to get in between chunks.
func TestVerifyReleasesTheConnectionBetweenChunks(t *testing.T) {
	l, db := newLog(t)
	fill(t, l, 30000)
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var worst time.Duration
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			var n int
			_ = db.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
			if d := time.Since(start); d > worst {
				worst = d
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := l.Verify(ctx, VerifyOptions{Chunk: 1000})
	close(stop)
	wg.Wait()
	if err != nil || !res.OK() || res.Entries != 30000 {
		t.Fatalf("verify: %+v %v", res, err)
	}
	if worst > 100*time.Millisecond {
		t.Fatalf("a concurrent query waited %v for the connection", worst)
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

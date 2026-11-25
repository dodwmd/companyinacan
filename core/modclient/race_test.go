package modclient

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestReceiverIsSafeUnderConcurrentFirstUse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	h := &Receiver{Secret: "s3cret-s3cret-s3cret", Now: func() time.Time { return now }, Handle: func(context.Context, Event) error { return nil }}
	body := []byte(`{"id":"evt_r","type":"a.b.v1","time":"2026-01-01T00:00:00Z","source":"x","data":{}}`)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, signed("s3cret-s3cret-s3cret", now, body, "evt_r", "a.b.v1"))
		}()
	}
	wg.Wait()
}

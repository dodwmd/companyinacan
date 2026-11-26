package modclient

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeIntrospect struct {
	id    Identity
	err   error
	calls int
}

func (f *fakeIntrospect) Introspect(context.Context, string) (Identity, error) {
	f.calls++
	return f.id, f.err
}

func TestSummaryHandler(t *testing.T) {
	grant := SummaryTokenPrefix + strings.Repeat("a", 43)
	good := Identity{Active: true, Kind: SummaryKind, Subject: "user:alice", Permissions: []string{"work.read"}}
	var built int
	build := func(_ context.Context, subject string, perms []string) ([]SummaryFigure, error) {
		built++
		if subject != "user:alice" || len(perms) != 1 {
			t.Errorf("build(%q, %v)", subject, perms)
		}
		return nil, nil
	}
	for name, c := range map[string]struct {
		method, auth string
		id           Identity
		err          error
		status       int
		calls        int
	}{
		"ok":                    {auth: "Bearer " + grant, id: good, status: 200, calls: 1},
		"no credential":         {status: 401},
		"personal token":        {auth: "Bearer canpat_" + strings.Repeat("a", 43), id: good, status: 401},
		"malformed grant":       {auth: "Bearer cansum_short", id: good, status: 401},
		"inactive":              {auth: "Bearer " + grant, status: 401, calls: 1},
		"a user token's answer": {auth: "Bearer " + grant, id: Identity{Active: true, Kind: "user", Subject: "user:alice"}, status: 401, calls: 1},
		"a module's identity":   {auth: "Bearer " + grant, id: Identity{Active: true, Kind: "module"}, status: 401, calls: 1},
		"bad subject":           {auth: "Bearer " + grant, id: Identity{Active: true, Kind: SummaryKind, Subject: "service"}, status: 401, calls: 1},
		"can-core down":         {auth: "Bearer " + grant, err: errors.New("down"), status: 503, calls: 1},
		"post":                  {method: "POST", auth: "Bearer " + grant, id: good, status: 405},
	} {
		built = 0
		api := &fakeIntrospect{id: c.id, err: c.err}
		h := SummaryHandler(api, slog.New(slog.DiscardHandler), SummaryLimit{}, build)
		method := c.method
		if method == "" {
			method = http.MethodGet
		}
		r := httptest.NewRequest(method, SummaryPath, nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status || api.calls != c.calls {
			t.Errorf("%s: status %d calls %d, want %d %d", name, w.Code, api.calls, c.status, c.calls)
		}
		if (w.Code == 200) != (built == 1) {
			t.Errorf("%s: built %d times", name, built)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: cacheable", name)
		}
		if w.Code == 200 && strings.TrimSpace(w.Body.String()) != `{"version":1,"figures":[]}` {
			t.Errorf("%s: %s", name, w.Body)
		}
	}
}

// grantIntrospect knows one grant; junk is inactive, and "stuck" holds its caller until released.
type grantIntrospect struct {
	good, stuck string
	release     chan struct{}
	inFlight    atomic.Int32
}

func (f *grantIntrospect) Introspect(ctx context.Context, tok string) (Identity, error) {
	switch tok {
	case f.good:
		return Identity{Active: true, Kind: SummaryKind, Subject: "user:alice"}, nil
	case f.stuck:
		f.inFlight.Add(1)
		select {
		case <-f.release:
		case <-ctx.Done():
		}
	}
	return Identity{}, nil
}

func TestSummaryFailuresAreLimitedAndValidGrantsAreNot(t *testing.T) {
	grant := func(c string) string { return SummaryTokenPrefix + strings.Repeat(c, 43) }
	api := &grantIntrospect{good: grant("g"), stuck: grant("s"), release: make(chan struct{})}
	var mu sync.Mutex
	failed := map[string]int{}
	limit := SummaryLimit{
		Client: func(r *http.Request) string { return r.RemoteAddr },
		Failed: func(c string) bool { mu.Lock(); defer mu.Unlock(); failed[c]++; return failed[c] <= 3 },
	}
	h := SummaryHandler(api, slog.New(slog.DiscardHandler), limit,
		func(context.Context, string, []string) ([]SummaryFigure, error) { return nil, nil })
	get := func(addr, tok string) int {
		r := httptest.NewRequest(http.MethodGet, SummaryPath, nil)
		r.RemoteAddr = addr
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i, tok := range []string{"junk", grant("x"), "junk", "junk", grant("x")} {
		want := http.StatusUnauthorized
		if i >= 3 {
			want = http.StatusTooManyRequests
		}
		if got := get("attacker", tok); got != want {
			t.Errorf("failure %d: %d, want %d", i+1, got, want)
		}
	}
	// A valid grant is answered, from the very address over the limit too.
	for _, addr := range []string{"attacker", "can-core"} {
		if got := get(addr, api.good); got != http.StatusOK {
			t.Errorf("a valid grant from %s: %d", addr, got)
		}
	}
	// An address over the limit has one request introspected at a time, so its flood cannot take
	// the slots a valid grant from elsewhere needs.
	var wg sync.WaitGroup
	for range 2 * maxSummaryRedemptions {
		wg.Add(1)
		go func() { defer wg.Done(); get("attacker", api.stuck) }()
	}
	for api.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := get("can-core", api.good); got != http.StatusOK {
		t.Errorf("a valid grant during a flood: %d", got)
	}
	if n := api.inFlight.Load(); n != 1 {
		t.Errorf("%d requests of the address over the limit were introspected at once", n)
	}
	close(api.release)
	wg.Wait()
}

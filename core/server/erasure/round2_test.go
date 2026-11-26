package erasure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// One tombstone with a time the ledger cannot hold is refused by itself: the others of its module,
// and the other modules', are recorded.
func TestOneImpossibleTimeDoesNotFailTheBatch(t *testing.T) {
	people := &fakeFeed{items: []tombstone{
		{1, "pers_year_minus_one", "directory-basic", "0000-01-01", "0000-01-01T00:00:00+14:00"},
		{2, "pers_garbage", "directory-basic", "0000-01-01", "not a time"},
		{3, "pers_ok", "directory-basic", "2026-01-02", "2026-01-02T00:00:00Z"},
	}}
	jobs := &fakeFeed{items: []tombstone{{1, "cand_ok", "candidate-pii", "2026-01-02", "2026-01-02T00:00:00Z"}}}
	c := feedRig(t, map[string]*fakeFeed{"jobs": jobs, "people": people},
		declaring(t, "jobs", store.StateEnabled, "candidate-pii"), declaring(t, "people", store.StateEnabled, "directory-basic"))
	st, err := c.Collect(context.Background())
	if err != nil || st.Rejected != 2 || len(st.Failed) != 0 {
		t.Fatalf("%v %+v", err, st)
	}
	if es := collected(t, c); len(es) != 2 {
		t.Fatalf("%+v", es)
	}
}

// A feed that never ends, or answers slowly, is cut off and does not starve the module after it.
func TestASlowOrEndlessFeedDoesNotStarveTheOthers(t *testing.T) {
	defer func(d time.Duration, r int) { moduleDeadline, maxRejected = d, r }(moduleDeadline, maxRejected)
	moduleDeadline, maxRejected = 300*time.Millisecond, 20
	people := &fakeFeed{items: []tombstone{{1, "pers_ok", "directory-basic", "2026-01-02", "2026-01-02T00:00:00Z"}}}
	c := feedRig(t, map[string]*fakeFeed{"board": {}, "jobs": {}, "people": people},
		declaring(t, "board", store.StateEnabled, "board-private"), declaring(t, "jobs", store.StateEnabled, "candidate-pii"),
		declaring(t, "people", store.StateEnabled, "directory-basic"))
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer hang.Close()
	endless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var after int
		_, _ = fmt.Sscan(r.URL.Query().Get("after"), &after)
		_, _ = fmt.Fprintf(w, `{"items":[{"seq":%d,"subject":"x","class":"not-declared","date":"2026-01-02"}]}`, after+1)
	}))
	defer endless.Close()
	urls := c.ServiceURL
	c.ServiceURL = func(ns, s string, p int) string {
		switch ns {
		case "mod-board":
			return hang.URL
		case "mod-jobs":
			return endless.URL
		}
		return urls(ns, s, p)
	}
	st, err := c.Collect(context.Background())
	if err != nil || st.Failed["board"] == "" || !strings.Contains(st.Failed["jobs"], "refused") {
		t.Fatalf("%v %+v", err, st)
	}
	if es := collected(t, c); len(es) != 1 || es[0].Subject != "pers_ok" {
		t.Fatalf("%+v", es)
	}
}

// A feed that gives no time is not recorded again on every run, also on the day of the erasure.
func TestAFeedWithoutTimesDoesNotGrowTheLedger(t *testing.T) {
	people := &fakeFeed{items: []tombstone{{1, "pers_1", "directory-basic", "2026-05-01", ""}}}
	c := feedRig(t, map[string]*fakeFeed{"people": people}, declaring(t, "people", store.StateEnabled, "directory-basic"))
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { now = now.Add(time.Hour); return now }
	for range 4 {
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if es := collected(t, c); len(es) != 1 {
		t.Fatalf("%+v", es)
	}
}

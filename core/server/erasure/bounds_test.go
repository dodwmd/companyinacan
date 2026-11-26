package erasure

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// feedRig runs a collector over fake feeds, one per module.
func feedRig(t *testing.T, feedsByModule map[string]*fakeFeed, mods ...store.Module) *Collector {
	t.Helper()
	api := kube.NewFake()
	urls := map[string]string{}
	for id, f := range feedsByModule {
		s := httptest.NewServer(f)
		t.Cleanup(s.Close)
		urls["mod-"+id] = s.URL
		if _, err := api.Create(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": "can-module", "namespace": "mod-" + id},
			"stringData": map[string]any{feeds[id]: "tok"}}); err != nil {
			t.Fatal(err)
		}
	}
	return &Collector{API: api, Dir: t.TempDir(), Modules: func(context.Context) ([]store.Module, error) { return mods, nil },
		ServiceURL: func(ns, _ string, _ int) string { return urls[ns] }}
}

func declaring(t *testing.T, id, state string, classes ...string) store.Module {
	t.Helper()
	m := module(t, id, state)
	m.Manifest.DataClasses = m.Manifest.DataClasses[:0]
	for _, c := range classes {
		m.Manifest.DataClasses = append(m.Manifest.DataClasses, struct {
			Class       string `yaml:"class"`
			Retention   string `yaml:"retention"`
			Sensitivity string `yaml:"sensitivity"`
		}{Class: c})
	}
	return m
}

func collected(t *testing.T, c *Collector) []backup.Entry {
	t.Helper()
	es, err := backup.ReadCollected(filepath.Join(c.Dir, backup.CollectedDir))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// A compromised module cannot order another module's data erased.
func TestOnlyTheModulesOwnClassesAreAccepted(t *testing.T) {
	jobs := &fakeFeed{items: []tombstone{
		{1, "pers_victim", "directory-basic", "2026-01-02", ""},
		{2, "fil_victim", "file-content", "2026-01-02", ""},
		{3, "pers_victim", "entity-person", "2026-01-02", ""}, // shared by board and entity only
		{4, "cand_1", "candidate-pii", "2026-01-02", "2999-01-01T00:00:00Z"},
	}}
	board := &fakeFeed{items: []tombstone{{1, "pers_a", "entity-person", "2026-01-02", "2026-01-02T03:04:05Z"}}}
	c := feedRig(t, map[string]*fakeFeed{"jobs": jobs, "board": board},
		declaring(t, "board", store.StateEnabled, "board-private"), declaring(t, "jobs", store.StateEnabled, "candidate-pii"))
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	st, err := c.Collect(context.Background())
	if err != nil || st.Rejected != 3 {
		t.Fatalf("%v %+v", err, st)
	}
	es := collected(t, c)
	if len(es) != 2 || es[0].Module != "board" || es[0].At != "2026-01-02T03:04:05Z" || es[1].Module != "jobs" || es[1].Subject != "cand_1" {
		t.Fatalf("%+v", es)
	}
	if es[1].At != now.Format(time.RFC3339) {
		t.Errorf("a time in the future would be replayed into every later backup: %s", es[1].At)
	}
}

// A subject erased again later is a later erasure, recorded as one.
func TestALaterErasureOfTheSameSubjectIsRecorded(t *testing.T) {
	people := &fakeFeed{items: []tombstone{{1, "pers_1", "directory-basic", "2026-01-02", "2026-01-02T00:00:00Z"}}}
	c := feedRig(t, map[string]*fakeFeed{"people": people}, declaring(t, "people", store.StateEnabled, "directory-basic"))
	for _, at := range []string{"2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z", "2026-03-04T00:00:00Z"} {
		people.items[0].At = at
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if es := collected(t, c); len(es) != 2 || es[1].At != "2026-03-04T00:00:00Z" {
		t.Fatalf("%+v", es)
	}
}

// One module flooding its feed stays within its share and does not stop the others, and the ledger
// stays readable.
func TestAFloodingModuleIsBounded(t *testing.T) {
	defer func(a int, b int64) { maxNewPerRun, maxModuleBytes = a, b }(maxNewPerRun, maxModuleBytes)
	maxNewPerRun, maxModuleBytes = 3, 2000
	flood := &fakeFeed{}
	for i := range 40 {
		flood.items = append(flood.items, tombstone{int64(i + 1), fmt.Sprintf("cand_%d", i), "candidate-pii", "2026-01-02", "2026-01-02T00:00:00Z"})
	}
	people := &fakeFeed{items: []tombstone{{1, "pers_1", "directory-basic", "2026-01-02", "2026-01-02T00:00:00Z"}}}
	c := feedRig(t, map[string]*fakeFeed{"jobs": flood, "people": people},
		declaring(t, "jobs", store.StateEnabled, "candidate-pii"), declaring(t, "people", store.StateEnabled, "directory-basic"))
	st, err := c.Collect(context.Background())
	if err != nil || len(st.Failed) != 0 || len(collected(t, c)) != 4 {
		t.Fatalf("first run takes 3 and the other module's 1: %v %+v %d", err, st, len(collected(t, c)))
	}
	for range 10 {
		if st, err = c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	n := len(collected(t, c))
	if !strings.Contains(st.Failed["jobs"], "full") || st.Failed["people"] != "" || n < 4 || n > 10 {
		t.Fatalf("%+v, %d entries", st, n)
	}
}

// A module recorded as failed may have run and erased before: not reading it is reported.
func TestAFailedModuleThatCannotBeReadIsReported(t *testing.T) {
	c := feedRig(t, map[string]*fakeFeed{"people": {down: true}}, declaring(t, "people", store.StateFailed, "directory-basic"))
	st, err := c.Collect(context.Background())
	if err != nil || st.Failed["people"] == "" {
		t.Fatalf("%v %+v", err, st)
	}
	rep, err := backup.ImportCollected(filepath.Join(c.Dir, backup.CollectedDir), &backup.Ledger{Path: filepath.Join(t.TempDir(), "l")}, time.Now())
	if err != nil || len(rep.Problems) == 0 {
		t.Fatalf("the host import must fail on it: %v %+v", err, rep)
	}
}

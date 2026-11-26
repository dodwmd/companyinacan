package erasure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// fakeFeed serves GET /v1/erasures in pages of two, like the modules do in pages of 1000.
type fakeFeed struct {
	mu    sync.Mutex
	items []tombstone
	down  bool
}

func (f *fakeFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down || r.URL.Path != "/v1/erasures" || r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	out := []tombstone{}
	for _, t := range f.items {
		if t.Seq > after && len(out) < 2 {
			out = append(out, t)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": out})
}

func module(t *testing.T, id, state string) store.Module {
	t.Helper()
	m := store.Module{ID: id, State: state, Namespace: "mod-" + id}
	text := fmt.Sprintf("id: %[1]s\nprovides: [{service: %[1]s, port: 8080}]\ndata_classes: [{class: %[1]s-x}, {class: directory-basic}, {class: people-sensitive}]\n", id)
	if err := yaml.Unmarshal([]byte(text), &m.Manifest); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCollect(t *testing.T) {
	people := &fakeFeed{items: []tombstone{
		{1, "pers_1", "directory-basic", "2026-01-02", ""},
		{2, "pers_1", "people-sensitive", "2026-01-02", ""},
		{3, "jane@example.com", "directory-basic", "2026-01-02", ""}, // not an opaque id
		{4, "pers_2", "directory-basic", "2026-02-03", ""},
	}}
	jobs := &fakeFeed{down: true}
	srv := map[string]*httptest.Server{"people": httptest.NewServer(people), "jobs": httptest.NewServer(jobs)}
	api := kube.NewFake()
	for id, s := range srv {
		defer s.Close()
		if _, err := api.Create(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": "can-module", "namespace": "mod-" + id},
			"stringData": map[string]any{feeds[id]: "tok"}}); err != nil {
			t.Fatal(err)
		}
	}
	mods := []store.Module{module(t, "provisioning", store.StateEnabled), module(t, "files", store.StateDisabled), module(t, "jobs", store.StateEnabled), module(t, "people", store.StateEnabled)}
	dir := t.TempDir()
	c := &Collector{API: api, Dir: dir, Modules: func(context.Context) ([]store.Module, error) { return mods, nil },
		ServiceURL: func(ns, _ string, _ int) string { return srv[strings.TrimPrefix(ns, "mod-")].URL }}
	st, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Collected, []string{"people"}) || !slices.Equal(st.NoFeed, []string{"provisioning"}) || !slices.Equal(st.NotAsked, []string{"files"}) ||
		st.Failed["jobs"] == "" || st.Rejected != 1 {
		t.Fatalf("status %+v", st)
	}
	path := filepath.Join(dir, backup.CollectedDir, backup.CollectedLedger)
	raw, err := os.ReadFile(path) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "jane@") {
		t.Error("an e-mail address reached the ledger")
	}
	es, err := backup.ReadCollected(filepath.Join(dir, backup.CollectedDir))
	if err != nil || len(es) != 3 || es[2].Subject != "pers_2" || es[2].Date != "2026-02-03" {
		t.Fatalf("ledger %v %+v", err, es)
	}

	// A second run adds nothing; a restored module that numbers afresh is still read from the start.
	people.items = []tombstone{{1, "pers_3", "directory-basic", "2026-03-04", ""}}
	for range 2 {
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if es, err = backup.ReadCollected(filepath.Join(dir, backup.CollectedDir)); err != nil || len(es) != 4 {
		t.Fatalf("append-only and idempotent: %v %+v", err, es)
	}

	// The host reports the failed feed after importing what there is.
	host := &backup.Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl")}
	rep, err := backup.ImportCollected(filepath.Join(dir, backup.CollectedDir), host, time.Now())
	if err != nil || rep.Added != 4 || !strings.Contains(strings.Join(rep.Problems, " "), "jobs") || !strings.Contains(strings.Join(rep.Notes, " "), "provisioning") {
		t.Fatalf("import: %v %+v", err, rep)
	}
}

// Every feed's credential is one can-core generates for that module.
func TestFeedCredentialsAreGenerated(t *testing.T) {
	for id, key := range feeds {
		if !slices.ContainsFunc(modsecrets.For(id), func(s modsecrets.Spec) bool { return s.Key == key }) {
			t.Errorf("%s: %s is not generated", id, key)
		}
	}
}

// E06d: a module whose first install never succeeded is asked, but not being able to is no failure:
// a failure would fail the erasure step of every backup.
func TestCollectDoesNotReportANeverInstalledModule(t *testing.T) {
	mods := []store.Module{module(t, "people", store.StateFailed), module(t, "jobs", store.StateFailed)}
	c := &Collector{API: kube.NewFake(), Dir: t.TempDir(), Modules: func(context.Context) ([]store.Module, error) { return mods, nil },
		NeverInstalled: func(_ context.Context, id string) bool { return id == "people" }}
	st, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.NotAsked, []string{"people"}) || len(st.Failed) != 1 || st.Failed["jobs"] == "" {
		t.Fatalf("not asked %v, failed %v", st.NotAsked, st.Failed)
	}
}

// A module that has a published dump has held data, whatever its record says now.
func TestCollectReportsAFailedModuleThatHasADump(t *testing.T) {
	mods := []store.Module{module(t, "people", store.StateFailed)}
	c := &Collector{API: kube.NewFake(), Dir: t.TempDir(), Modules: func(context.Context) ([]store.Module, error) { return mods, nil },
		NeverInstalled: func(context.Context, string) bool { return true }}
	dir := filepath.Join(c.Dir, "modules", "people")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed["people"] == "" || len(st.NotAsked) != 0 {
		t.Fatalf("not asked %v, failed %v", st.NotAsked, st.Failed)
	}
}

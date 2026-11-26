package overview

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

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type fakeDeps struct {
	perms     map[string][]string // module -> the viewer's permissions
	providers []registry.Provider
	minted    atomic.Int32
	// broken fails the module record or the standing of a module.
	brokenRecord, brokenStanding string
}

func (f *fakeDeps) Resolve(context.Context, string) (registry.Resolution, error) {
	if len(f.providers) == 0 {
		return registry.Resolution{}, registry.ErrNoProvider
	}
	return registry.Resolution{Providers: f.providers}, nil
}

func (f *fakeDeps) GetModule(_ context.Context, id string) (store.Module, error) {
	if id == f.brokenRecord {
		return store.Module{}, errors.New("disk I/O error")
	}
	return store.Module{ID: id, State: store.StateEnabled, Manifest: config.Module{Name: strings.ToUpper(id)}}, nil
}

func (f *fakeDeps) Standing(_ context.Context, module, _ string) ([]string, bool, error) {
	if module == f.brokenStanding {
		return nil, false, errors.New("rbac: database is locked")
	}
	p := f.perms[module]
	return append([]string(nil), p...), len(p) > 0, nil
}

func (f *fakeDeps) MintSummary(context.Context, string, string) (string, error) {
	f.minted.Add(1)
	return "cansum_x", nil
}

func (f *fakeDeps) DropSummary(string) {}

func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func answer(body string, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}
}

func TestCollectAsksOnlyWhereTheViewerHasARightAndCaches(t *testing.T) {
	var work, finance atomic.Int32
	f := &fakeDeps{perms: map[string][]string{"work": {"work.read"}}, providers: []registry.Provider{
		{ModuleID: "finance", URL: serve(t, answer(`{"version":1,"figures":[{"key":"invoices.overdue","count":9}]}`, &finance))},
		{ModuleID: "work", URL: serve(t, answer(`{"version":1,"figures":[{"key":"work.urgent_open","count":3}]}`, &work))},
	}}
	now := time.Unix(1_700_000_000, 0)
	s := &Service{Registry: f, DB: f, Grants: f, Now: func() time.Time { return now }}
	ctx := context.Background()
	got, err := s.Collect(ctx, "user:alice")
	if err != nil || len(got) != 1 || got[0].ID != "work" || got[0].Unavailable || got[0].Values[0].Count != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if finance.Load() != 0 || f.minted.Load() != 1 {
		t.Fatalf("a module without a right was asked (%d calls, %d grants)", finance.Load(), f.minted.Load())
	}
	if _, _ = s.Collect(ctx, "user:alice"); work.Load() != 1 {
		t.Fatal("not cached")
	}
	// Somebody else, and the same person with other rights, never get the cached answer.
	if _, _ = s.Collect(ctx, "user:bob"); work.Load() != 2 {
		t.Fatal("the cache is shared between people")
	}
	f.perms["work"] = []string{"work.admin", "work.read"}
	if _, _ = s.Collect(ctx, "user:alice"); work.Load() != 3 {
		t.Fatal("a change of rights must not wait for the cache")
	}
	delete(f.perms, "work")
	if got, _ := s.Collect(ctx, "user:alice"); len(got) != 0 {
		t.Fatalf("figures after the right was removed: %+v", got)
	}
	f.perms["work"] = []string{"work.read"}
	now = now.Add(DefaultTTL)
	if _, _ = s.Collect(ctx, "user:alice"); work.Load() != 4 {
		t.Fatal("the cache did not expire")
	}
}

func TestSlowBrokenAndRedirectingModulesAreUnavailable(t *testing.T) {
	release := make(chan struct{})
	var elsewhere atomic.Int32
	target := serve(t, answer(`{"version":1,"figures":[]}`, &elsewhere))
	f := &fakeDeps{perms: map[string][]string{"work": {"p"}, "finance": {"p"}, "people": {"p"}, "forms": {"p"}, "entity": {"p"}}, providers: []registry.Provider{
		{ModuleID: "work", URL: serve(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})},
		{ModuleID: "finance", URL: serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })},
		{ModuleID: "people", URL: serve(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, http.StatusFound) })},
		{ModuleID: "forms", URL: "http://127.0.0.1:1"},
		{ModuleID: "entity", URL: serve(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"version":1,"figures":[],"pad":"` + strings.Repeat("a", maxAnswer) + `"}`))
		})},
	}}
	t.Cleanup(func() { close(release) })
	s := &Service{Registry: f, DB: f, Grants: f, Timeout: 100 * time.Millisecond, Log: slog.New(slog.DiscardHandler)}
	start := time.Now()
	got, err := s.Collect(context.Background(), "user:alice")
	if err != nil || len(got) != 5 {
		t.Fatalf("%+v %v", got, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a slow module held the page for %s", d)
	}
	for _, m := range got {
		if !m.Unavailable || len(m.Values) != 0 {
			t.Errorf("%s: %+v", m.ID, m)
		}
	}
	if elsewhere.Load() != 0 {
		t.Error("a redirect was followed with the grant")
	}
}

func TestParseIsStrict(t *testing.T) {
	got, err := parse("finance", []byte(`{"version":1,"later":true,"figures":[
		{"key":"contracts.expiring","count":1,"amounts":[{"currency":"AUD","minor":5}]},
		{"key":"people.headcount","count":7},
		{"key":"something.new","count":7},
		{"key":"invoices.overdue","count":2,"amounts":[{"currency":"AUD","minor":-5},{"currency":"JPY","minor":7}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// In display order; another module's key and an unknown one are dropped, as are amounts of a
	// figure that is not money.
	if len(got) != 2 || got[0].Key != "invoices.overdue" || len(got[0].Amounts) != 2 || got[1].Key != "contracts.expiring" || got[1].Amounts != nil {
		t.Fatalf("%+v", got)
	}
	for _, bad := range []string{
		``, `[]`, `{"version":2,"figures":[]}`,
		`{"version":1,"figures":[{"key":"invoices.overdue","count":-1}]}`,
		`{"version":1,"figures":[{"key":"invoices.overdue","count":1},{"key":"invoices.overdue","count":2}]}`,
		`{"version":1,"figures":[{"key":"invoices.overdue","count":1,"amounts":[{"currency":"Ada","minor":1}]}]}`,
		`{"version":1,"figures":[{"key":"invoices.overdue","count":1,"amounts":[{"currency":"AUD","minor":1},{"currency":"AUD","minor":2}]}]}`,
		`{"version":1,"figures":[{"key":"invoices.overdue","count":1,"amounts":[{"currency":"AUD","minor":1e300}]}]}`,
	} {
		if _, err := parse("finance", []byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestFormatMoney(t *testing.T) {
	for want, got := range map[string]string{
		"AUD 0.05": FormatMoney("AUD", 5), "AUD -12.30": FormatMoney("AUD", -1230),
		"JPY 500": FormatMoney("JPY", 500), "KWD 1.234": FormatMoney("KWD", 1234),
	} {
		if want != got {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// hang answers nothing until the test ends or the caller gives up.
func hang(t *testing.T, hits *atomic.Int32) string {
	t.Helper()
	release := make(chan struct{})
	url := serve(t, func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	return url
}

func TestAHangingModuleDoesNotMakeOthersUnavailableAndQueueTimeoutsAreNotCached(t *testing.T) {
	var stuck, ok atomic.Int32
	f := &fakeDeps{perms: map[string][]string{"work": {"p"}, "finance": {"p"}}, providers: []registry.Provider{
		{ModuleID: "work", URL: hang(t, &stuck)},
		{ModuleID: "finance", URL: serve(t, answer(`{"version":1,"figures":[{"key":"invoices.overdue","count":1}]}`, &ok))},
	}}
	s := &Service{Registry: f, DB: f, Grants: f, Timeout: 300 * time.Millisecond, Log: slog.New(slog.DiscardHandler)}
	// Many more people than the hanging module has slots, all at once.
	var wg sync.WaitGroup
	people := 4 * maxPerModule
	for i := range people {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Collect(context.Background(), "user:p"+utoa(uint64(i)))
			if err != nil || len(got) != 2 {
				t.Errorf("%+v %v", got, err)
				return
			}
			for _, m := range got {
				if m.Unavailable != (m.ID == "work") {
					t.Errorf("%s unavailable = %v", m.ID, m.Unavailable)
				}
			}
		}()
	}
	wg.Wait()
	if stuck.Load() < maxPerModule {
		t.Fatal("the hanging module was not asked")
	}
	// The failures of the calls that were made are cached briefly; those that only waited are not.
	s.mu.Lock()
	cached := 0
	for k := range s.cache {
		if strings.HasPrefix(k, "work\x00") {
			cached++
		}
	}
	s.mu.Unlock()
	if cached != maxPerModule {
		t.Errorf("%d failures of the hanging module are cached, want %d: waiting for a slot says nothing about it", cached, maxPerModule)
	}
}

func TestConcurrentLoadsShareOneCallEvenWhenTheFirstIsAbandoned(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	f := &fakeDeps{perms: map[string][]string{"work": {"p"}}, providers: []registry.Provider{{ModuleID: "work", URL: serve(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"version":1,"figures":[{"key":"work.urgent_open","count":2}]}`))
	})}}}
	s := &Service{Registry: f, DB: f, Grants: f, Timeout: 5 * time.Second}
	first, abandon := context.WithCancel(context.Background())
	gone := make(chan struct{})
	go func() { defer close(gone); _, _ = s.Collect(first, "user:alice") }()
	for hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	abandon()
	<-gone
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Collect(context.Background(), "user:alice")
			if err != nil || len(got) != 1 || got[0].Unavailable || got[0].Values[0].Count != 2 {
				t.Errorf("%+v %v", got, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if hits.Load() != 1 || f.minted.Load() != 1 {
		t.Errorf("%d calls and %d grants for one person and module", hits.Load(), f.minted.Load())
	}
}

func TestUnreadableRecordsAndRightsAreUnavailableNotHidden(t *testing.T) {
	var hits atomic.Int32
	url := serve(t, answer(`{"version":1,"figures":[]}`, &hits))
	f := &fakeDeps{brokenRecord: "work", brokenStanding: "finance", providers: []registry.Provider{{ModuleID: "work", URL: url}, {ModuleID: "finance", URL: url}}}
	s := &Service{Registry: f, DB: f, Grants: f, Log: slog.New(slog.DiscardHandler)}
	got, err := s.Collect(context.Background(), "user:alice")
	if err != nil || len(got) != 2 || !got[0].Unavailable || !got[1].Unavailable {
		t.Fatalf("%+v %v", got, err)
	}
	if hits.Load() != 0 || f.minted.Load() != 0 {
		t.Error("a module was asked without knowing the person's rights")
	}
}

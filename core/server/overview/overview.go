// Package overview asks the installed modules for the aggregate figures of the business overview
// (specs/053-business-overview). For each module that offers the `summary` capability and in which
// the viewer holds a permission, it mints a one-use summary grant, calls the module directly,
// checks the answer strictly and caches it briefly. can-core holds no credential for any module's
// data: without a signed-in person who has a right in the module, nothing is asked.
package overview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Limits.
const (
	DefaultTimeout = 2 * time.Second
	DefaultTTL     = 30 * time.Second
	failureTTL     = 5 * time.Second
	maxAnswer      = 16 << 10
	maxFigures     = 32
	maxAmounts     = 40
	maxCount       = int64(1e12)
	maxMinor       = int64(1) << 53
	maxCached      = 512
	// maxPerModule bounds the calls in flight to one module. The limit is per module, so one that
	// hangs holds only its own calls up.
	maxPerModule = 4
)

// errBusy is a call that never got one of its module's slots, or got one too late to be answered:
// it says nothing about the module and is not cached.
var errBusy = errors.New("too many calls to the module are in flight")

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// Grants is the part of the ticket service the overview uses (summary grants).
type Grants interface {
	Standing(ctx context.Context, module, subject string) ([]string, bool, error)
	MintSummary(ctx context.Context, subject, module string) (string, error)
	DropSummary(token string)
}

// Resolver finds the providers of a capability.
type Resolver interface {
	Resolve(ctx context.Context, capability string) (registry.Resolution, error)
}

// Modules reads installed module records.
type Modules interface {
	GetModule(ctx context.Context, id string) (store.Module, error)
}

// Value is one figure of a module, in display order.
type Value struct {
	Figure
	Count   int64
	Amounts []modclient.SummaryAmount
}

// Module is what one module answered for the viewer.
type Module struct {
	ID, Name string
	// Unavailable means the module did not give a usable answer in time.
	Unavailable bool
	Values      []Value
	// At is when the figures were obtained.
	At time.Time
}

// Service collects summaries.
type Service struct {
	Registry Resolver
	DB       Modules
	Grants   Grants
	// Client defaults to a client that follows no redirects and uses no proxy.
	Client  *http.Client
	Timeout time.Duration
	TTL     time.Duration
	Now     func() time.Time
	Log     *slog.Logger

	mu     sync.Mutex
	cache  map[string]entry
	client *http.Client
	slots  map[string]chan struct{} // by module
	calls  map[string]*call         // in flight, by cache key
}

// call is one fetch that every concurrent page load for the same person, module and permissions
// waits for.
type call struct {
	done chan struct{}
	m    Module
}

type entry struct {
	m       Module
	expires time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultTimeout
}

func (s *Service) setup() *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		s.client = s.Client
		if s.client == nil {
			t := http.DefaultTransport.(*http.Transport).Clone()
			t.Proxy = nil
			s.client = &http.Client{Transport: t}
		}
		c := *s.client
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		s.client = &c
		s.slots = map[string]chan struct{}{}
		s.calls = map[string]*call{}
		s.cache = map[string]entry{}
	}
	return s.client
}

// Collect returns the figures of every module that offers a summary and in which subject holds a
// permission, in the order of Figures. A module that cannot answer is listed as unavailable, and so
// is one whose record or the person's rights in it cannot be read; a module with nothing to show
// this person is not listed.
func (s *Service) Collect(ctx context.Context, subject string) ([]Module, error) {
	res, err := s.Registry.Resolve(ctx, modclient.SummaryCapability)
	if errors.Is(err, registry.ErrNoProvider) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolving summary providers: %w", err)
	}
	client := s.setup()
	out := make([]Module, len(res.Providers))
	var wg sync.WaitGroup
	for i, p := range res.Providers {
		m, err := s.DB.GetModule(ctx, p.ModuleID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && m.State != store.StateEnabled) {
			continue
		}
		name := m.Manifest.Name
		if name == "" {
			name = p.ModuleID
		}
		var perms []string
		ok := false
		if err == nil {
			perms, ok, err = s.Grants.Standing(ctx, p.ModuleID, subject)
		}
		if err != nil {
			// Whether this person has a right in the module cannot be told: nothing is asked, and
			// the page says the area is missing rather than leaving it out.
			s.warn(p.ModuleID, err)
			out[i] = Module{ID: p.ModuleID, Name: name, Unavailable: true, At: s.now()}
			continue
		}
		if !ok {
			continue // no right in this module: it is not asked and not mentioned
		}
		// The permissions are part of the key, so a role removed or added takes effect at once
		// rather than when the cached figures expire.
		slices.Sort(perms)
		key := p.ModuleID + "\x00" + subject + "\x00" + strings.Join(perms, ",")
		url := strings.TrimRight(p.URL, "/") + modclient.SummaryPath
		wg.Add(1)
		go func(i int, id, name string) {
			defer wg.Done()
			out[i] = s.shared(ctx, client, subject, id, name, url, key)
		}(i, p.ModuleID, name)
	}
	wg.Wait()
	var list []Module
	for _, m := range out {
		if m.ID != "" && (m.Unavailable || len(m.Values) > 0) {
			list = append(list, m)
		}
	}
	rank := func(id string) int {
		for i, f := range Figures {
			if f.Module == id {
				return i
			}
		}
		return len(Figures)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if a, b := rank(list[i].ID), rank(list[j].ID); a != b {
			return a < b
		}
		return list[i].ID < list[j].ID
	})
	return list, nil
}

func (s *Service) warn(module string, err error) {
	if s.Log != nil {
		s.Log.Warn("overview: no summary", "module", module, "error", err)
	}
}

// shared answers from the cache, or joins the call in flight for the key, or starts one. The call
// does not end with the page load that started it (others may be waiting for it), only with its own
// time limit.
func (s *Service) shared(ctx context.Context, client *http.Client, subject, id, name, url, key string) Module {
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && s.now().Before(e.expires) {
		s.mu.Unlock()
		return e.m
	}
	c, running := s.calls[key]
	if !running {
		c = &call{done: make(chan struct{})}
		s.calls[key] = c
		slot := s.slots[id]
		if slot == nil {
			slot = make(chan struct{}, maxPerModule)
			s.slots[id] = slot
		}
		go s.fetch(context.WithoutCancel(ctx), client, slot, c, subject, id, name, url, key)
	}
	s.mu.Unlock()
	select {
	case <-c.done:
		return c.m
	case <-ctx.Done(): // the browser gave up
		return Module{ID: id, Name: name, Unavailable: true, At: s.now()}
	}
}

func (s *Service) fetch(ctx context.Context, client *http.Client, slot chan struct{}, c *call, subject, id, name, url, key string) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	m := Module{ID: id, Name: name}
	values, err := s.ask(ctx, client, slot, subject, id, url)
	m.At, m.Values = s.now(), values
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if err != nil {
		m.Unavailable, ttl = true, min(ttl, failureTTL)
		s.warn(id, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.m = m
	delete(s.calls, key)
	close(c.done)
	if errors.Is(err, errBusy) {
		return
	}
	if len(s.cache) >= maxCached {
		for k, e := range s.cache {
			if !m.At.Before(e.expires) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= maxCached {
			s.cache = map[string]entry{}
		}
	}
	s.cache[key] = entry{m: m, expires: m.At.Add(ttl)}
}

func (s *Service) ask(ctx context.Context, client *http.Client, slot chan struct{}, subject, id, url string) (values []Value, err error) {
	select {
	case slot <- struct{}{}:
	default:
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return nil, errBusy
		}
		if ctx.Err() != nil { // the slot came too late to be of use
			<-slot
			return nil, errBusy
		}
		// Having queued, running out of time is the queue's doing, not proof the module is down.
		defer func() {
			if err != nil && ctx.Err() != nil {
				err = fmt.Errorf("%w: %w", errBusy, err)
			}
		}()
	}
	defer func() { <-slot }()
	tok, err := s.Grants.MintSummary(ctx, subject, id)
	if err != nil {
		return nil, err
	}
	defer s.Grants.DropSummary(tok)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the module answered HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxAnswer {
		return nil, errors.New("the answer is too large")
	}
	return parse(id, b)
}

// parse checks a module's answer. Keys this can-core does not know, or that belong to another
// module, are dropped (a newer module may report more); anything else out of bounds spoils the
// whole answer.
func parse(module string, b []byte) ([]Value, error) {
	var in modclient.Summary
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, errors.New("the answer is not valid JSON")
	}
	if in.Version != modclient.SummaryVersion {
		return nil, fmt.Errorf("summary version %d is not supported", in.Version)
	}
	if len(in.Figures) > maxFigures {
		return nil, errors.New("too many figures")
	}
	byPos := map[int]Value{}
	for _, f := range in.Figures {
		known, pos, ok := figureOf(module, f.Key)
		if !ok {
			continue
		}
		if _, dup := byPos[pos]; dup {
			return nil, errors.New("a figure is repeated")
		}
		if f.Count < 0 || f.Count > maxCount || len(f.Amounts) > maxAmounts {
			return nil, errors.New("a figure is out of range")
		}
		v := Value{Figure: known, Count: f.Count}
		if known.Money {
			seen := map[string]bool{}
			for _, a := range f.Amounts {
				if !currencyRe.MatchString(a.Currency) || seen[a.Currency] || a.Minor > maxMinor || a.Minor < -maxMinor {
					return nil, errors.New("an amount is not valid")
				}
				seen[a.Currency] = true
				v.Amounts = append(v.Amounts, a)
			}
		}
		byPos[pos] = v
	}
	var out []Value
	for i := range Figures {
		if v, ok := byPos[i]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

func utoa(u uint64) string { return strconv.FormatUint(u, 10) }

package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

const (
	alice = "pers_aaaaaaaaaaaaaaaaaaaaaaaa"
	bob   = "pers_bbbbbbbbbbbbbbbbbbbbbbbb"
	carol = "pers_cccccccccccccccccccccccc"
)

// ---- fake directory ----------------------------------------------------------------------------

type fakeDir struct {
	mu      sync.Mutex
	people  map[string]Person
	order   []string
	down    bool
	updates []string
}

func newFakeDir() *fakeDir { return &fakeDir{people: map[string]Person{}} }

func (d *fakeDir) put(p Person) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.people[p.ID]; !ok {
		d.order = append(d.order, p.ID)
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = "2026-01-01T00:00:00Z"
	}
	if p.CreatedAt == "" {
		p.CreatedAt = "2026-01-01T00:00:00Z"
	}
	d.people[p.ID] = p
}

func (d *fakeDir) Get(_ context.Context, id string) (Person, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return Person{}, fmt.Errorf("%w: down", ErrUnavailable)
	}
	p, ok := d.people[id]
	if !ok {
		return Person{}, ErrNotFound
	}
	return p, nil
}

func (d *fakeDir) List(_ context.Context, q ListQuery) ([]Person, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return nil, "", fmt.Errorf("%w: down", ErrUnavailable)
	}
	start := 0
	if q.Cursor != "" {
		_, _ = fmt.Sscan(q.Cursor, &start)
	}
	lim := q.Limit
	if lim <= 0 {
		lim = 50
	}
	var out []Person
	i := start
	for ; i < len(d.order) && len(out) < lim; i++ {
		if p := d.people[d.order[i]]; q.Status == "" || p.Status == q.Status {
			out = append(out, p)
		}
	}
	next := ""
	if i < len(d.order) {
		next = fmt.Sprint(i)
	}
	return out, next, nil
}

func (d *fakeDir) Update(_ context.Context, id string, pt PersonPatch) (Person, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.people[id]
	if !ok {
		return Person{}, ErrNotFound
	}
	if pt.PreferredName != nil {
		p.Preferred = *pt.PreferredName
	}
	if pt.WorkPhone != nil {
		p.WorkPhone = *pt.WorkPhone
	}
	p.UpdatedAt = "2026-02-02T00:00:00Z"
	d.people[id] = p
	d.updates = append(d.updates, id)
	return p, nil
}

func person(id, status, email string) Person {
	return Person{ID: id, Kind: "employee", Status: status, GivenName: "Given" + id[5:9], FamilyName: "Family", WorkEmail: email, SSOSubject: "user:sso-" + id[5:9], WorkPhone: "+61 400 000 000"}
}

// ---- fake connector ----------------------------------------------------------------------------

type call struct{ Op, Key, Ext, Arg string }

type fakeConn struct {
	name     string
	mu       sync.Mutex
	calls    []call
	accounts map[string]bool // keys already applied: a repeat is a no-op (idempotency)
	// failures: op -> remaining transient failures; permFail: op -> always permanent failure
	failures map[string]int
	permFail map[string]string
}

func newFakeConn(name string) *fakeConn {
	return &fakeConn{name: name, accounts: map[string]bool{}, failures: map[string]int{}, permFail: map[string]string{}}
}

func (c *fakeConn) Name() string   { return c.name }
func (c *fakeConn) Verified() bool { return false }

func (c *fakeConn) do(op, key, ext, arg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call{op, key, ext, arg})
	if code, ok := c.permFail[op]; ok {
		return permanent(code)
	}
	if c.failures[op] > 0 {
		c.failures[op]--
		return transient("http_503")
	}
	c.accounts[key] = true
	return nil
}

func (c *fakeConn) CreateAccount(_ context.Context, key string, id Identity) (string, error) {
	if err := c.do(opCreateAccount, key, "", id.UserName); err != nil {
		return "", err
	}
	return "ext-" + c.name + "-" + id.PartyID[5:9], nil
}
func (c *fakeConn) UpdateAccount(_ context.Context, key, ext string, id Identity) error {
	return c.do(opUpdateAccount, key, ext, id.DisplayName)
}
func (c *fakeConn) DisableAccount(_ context.Context, key, ext string) error {
	return c.do(opDisableAccount, key, ext, "")
}
func (c *fakeConn) DeleteAccount(_ context.Context, key, ext string) error {
	return c.do(opDeleteAccount, key, ext, "")
}
func (c *fakeConn) AddToGroup(_ context.Context, key, ext, g string) error {
	return c.do(opAddGroup, key, ext, g)
}
func (c *fakeConn) RemoveFromGroup(_ context.Context, key, ext, g string) error {
	return c.do(opRemoveGroup, key, ext, g)
}

func (c *fakeConn) ops() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, x := range c.calls {
		s := x.Op
		if x.Arg != "" && x.Op != opCreateAccount && x.Op != opUpdateAccount {
			s += ":" + x.Arg
		}
		out = append(out, s)
	}
	return out
}

func (c *fakeConn) count(op string) int {
	n := 0
	for _, o := range c.ops() {
		if o == op || strings.HasPrefix(o, op+":") {
			n++
		}
	}
	return n
}

// ---- other fakes -------------------------------------------------------------------------------

type fakeApprovals struct {
	mu      sync.Mutex
	created []modclient.ApprovalInput
	status  map[string]string
	refuse  bool
}

func (a *fakeApprovals) CreateApproval(_ context.Context, in modclient.ApprovalInput) (modclient.Approval, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refuse {
		return modclient.Approval{}, modclient.ErrApprovalRefused
	}
	for i, c := range a.created {
		if c.Key == in.Key {
			return modclient.Approval{ID: fmt.Sprintf("apr_%024d", i), Status: "pending"}, nil
		}
	}
	a.created = append(a.created, in)
	return modclient.Approval{ID: fmt.Sprintf("apr_%024d", len(a.created)-1), Status: "pending"}, nil
}
func (a *fakeApprovals) GetApproval(_ context.Context, id string) (modclient.Approval, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.status[id]
	if s == "" {
		s = "pending"
	}
	return modclient.Approval{ID: id, Status: s}, nil
}
func (a *fakeApprovals) CancelApproval(context.Context, string, string) error { return nil }

type fakeCore struct {
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (c *fakeCore) RevokeIdentity(_ context.Context, party, subject string) (modclient.RevokeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, [2]string{party, subject})
	return modclient.RevokeResult{}, c.err
}

type fakeTasks struct {
	mu      sync.Mutex
	ensured []string
	err     error
}

func (t *fakeTasks) Ensure(_ context.Context, person, tpl, _ string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return false, t.err
	}
	t.ensured = append(t.ensured, person+"/"+tpl)
	return true, nil
}

// ---- harness -----------------------------------------------------------------------------------

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t     *testing.T
	clk   *clock
	st    *Store
	dir   *fakeDir
	a, b  *fakeConn
	appr  *fakeApprovals
	core  *fakeCore
	tasks *fakeTasks
	eng   *Engine
	pol   Policy
}

func testPolicy() Policy {
	return Policy{TZ: time.UTC, MaxAttempts: 3, BaseDelay: 10 * time.Second, MaxDelay: time.Minute, StepTimeout: 5 * time.Second,
		MaxStepsPerRun: 200, MaxStepsPerTick: 100, JoinerTemplate: "tpl_join", LeaverTemplate: "tpl_leave",
		PrivilegedGroups: []string{"admins"},
		Conn:             map[string]ConnConfig{"app-a": {Name: "app-a", DefaultGroups: []string{"all-staff"}, SyncGroups: boolPtr(true)}, "app-b": {Name: "app-b", Privileged: false}}}
}

func newHarness(t *testing.T, mod func(*Policy)) *harness {
	t.Helper()
	h := &harness{t: t, clk: &clock{t: time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)}, dir: newFakeDir(), a: newFakeConn("app-a"), b: newFakeConn("app-b"),
		appr: &fakeApprovals{status: map[string]string{}}, core: &fakeCore{}, tasks: &fakeTasks{}, pol: testPolicy()}
	if mod != nil {
		mod(&h.pol)
	}
	st, err := OpenStore(":memory:", h.clk.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.st = st
	h.eng = h.newEngine()
	return h
}

func (h *harness) newEngine() *Engine {
	cs, err := NewConnectorSet(h.a, h.b)
	if err != nil {
		h.t.Fatal(err)
	}
	return &Engine{Store: h.st, Dir: h.dir, Conns: cs, Tasks: h.tasks, Core: h.core, Approvals: h.appr, Policy: h.pol, Now: h.clk.now,
		Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

var evSeq int

func (h *harness) event(typ string, data any) error {
	evSeq++
	b, _ := json.Marshal(data)
	return h.eng.HandleEvent(context.Background(), modclient.Event{ID: fmt.Sprintf("evt_%024d", evSeq), Type: typ, Data: b})
}

func (h *harness) tick() {
	h.t.Helper()
	if _, err := h.eng.Tick(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// settle ticks until nothing is left to do now (bounded).
func (h *harness) settle() {
	h.t.Helper()
	for i := 0; i < 30; i++ {
		n, err := h.eng.Tick(context.Background())
		if err != nil {
			h.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (h *harness) runs() []Run {
	h.t.Helper()
	rs, err := h.st.ListRuns(context.Background(), RunFilter{})
	if err != nil {
		h.t.Fatal(err)
	}
	for i := range rs {
		full, _ := h.st.GetRun(context.Background(), rs[i].ID)
		rs[i] = full
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	return rs
}

func (h *harness) onlyRun() Run {
	h.t.Helper()
	rs := h.runs()
	if len(rs) != 1 {
		h.t.Fatalf("want 1 run, have %d", len(rs))
	}
	return rs[0]
}

func (h *harness) outbox() []string {
	h.t.Helper()
	items, err := h.st.PendingEvents(context.Background(), 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Type+" "+string(it.Data))
	}
	return out
}

func (h *harness) auditActions() []string {
	recs, err := h.st.AuditList(context.Background(), 0, 500)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		out = append(out, r.Action)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

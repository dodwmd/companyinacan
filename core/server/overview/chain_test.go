package overview_test

// The whole chain with the real parts: can-core's grants, module API and RBAC-style standing on one
// side, the real work, finance and forms services on the other, wired as an installation wires them
// (modclient with a projected token). Only the network names and the token review are faked.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/server/overview"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
	"github.com/dodwmd/companyinacan/core/work"
)

const (
	workAdmin    = "work-admin-token-0123456789abcdefghij"
	financeAdmin = "finance-admin-token-0123456789abcdefg"
)

type chain struct {
	t       *testing.T
	svc     *overview.Service
	addr    map[string]string // module -> test server address
	rights  map[string][]string
	coreSrv *httptest.Server
}

func newChain(t *testing.T) *chain {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, u := range []string{"alice", "bob"} {
		if _, err := db.CreateAdmin(ctx, u, "x", "y"); err != nil {
			t.Fatal(err)
		}
	}
	c := &chain{t: t, addr: map[string]string{}, rights: map[string][]string{}}
	review := tokenreview.NewFake()
	dir := t.TempDir()
	mods := []struct {
		id, service string
		perms       []string
	}{{"finance", "finance", []string{"finance.read", "finance.ledger.read"}}, {"forms", "forms-api", []string{"leads.read", "leads.write"}}, {"work", "work", []string{"work.read"}}}
	for _, m := range mods {
		mf := config.Module{ID: m.id, Name: m.id, Version: "1.0.0", Permissions: m.perms}
		providers := []store.Provider{{Capability: "summary", ModuleID: m.id, Service: m.service, Port: 8080, Schema: "summary.v1", Namespace: "mod-" + m.id}}
		if err := db.UpsertModule(ctx, store.Module{ID: m.id, Version: "1.0.0", Namespace: "mod-" + m.id, ServiceAccount: m.id, State: store.StateEnabled, HMACSecret: "s", Manifest: mf}, providers, nil); err != nil {
			t.Fatal(err)
		}
		review.Add("projected-"+m.id, "mod-"+m.id, m.id)
	}
	reg := registry.New(db, "http://can-core")
	tickets := &ticket.Service{DB: db, Audit: audit.New(db), Holds: func(_ context.Context, subject, perm string) (bool, error) {
		for _, p := range c.rights[subject] {
			if p == perm {
				return true, nil
			}
		}
		return false, nil
	}}
	api := &moduleapi.Server{Auth: modauth.New(review, db), Registry: reg, Bus: events.NewBus(db), Log: quiet, Tickets: tickets}
	c.coreSrv = httptest.NewServer(api.Handler())
	t.Cleanup(c.coreSrv.Close)

	client := func(id string) *modclient.Client {
		f := filepath.Join(dir, id)
		if err := os.WriteFile(f, []byte("projected-"+id), 0o600); err != nil {
			t.Fatal(err)
		}
		return &modclient.Client{BaseURL: c.coreSrv.URL, TokenFile: f, ModuleID: id}
	}
	listen := func(id string, h http.Handler) {
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		c.addr[id] = s.Listener.Addr().String()
	}

	wcfg := work.DefaultConfig()
	wcfg.DBPath = filepath.Join(dir, "work.db")
	wa, err := work.NewApp(wcfg, work.Secrets{AdminToken: workAdmin}, work.Options{Logger: quiet, Can: client("work")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wa.Close() })
	listen("work", wa.Handler())

	fcfg := finance.DefaultConfig()
	fcfg.DBPath = filepath.Join(dir, "finance.db")
	fa, err := finance.NewApp(fcfg, finance.Secrets{AdminToken: financeAdmin, LinkSecret: "link-secret-0123456789abcdefghijklmnop"}, finance.Options{Logger: quiet, Can: client("finance"), CanSecret: "webhook-secret-0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fa.Close() })
	listen("finance", fa.Handler())

	gcfg, err := forms.ParseConfig([]byte("forms:\n  - id: contact\n    fields:\n      - {name: email, type: email, required: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	gcfg.DBPath = filepath.Join(dir, "forms.db")
	ga, err := forms.NewApp(gcfg, forms.Secrets{TokenKey: []byte("token-key-0123456789abcdefghijklmnop"), APIToken: "forms-api-token-0123456789abcdefghij"}, forms.Options{Logger: quiet, Can: client("forms")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ga.Close(context.Background()) })
	listen("forms", ga.APIHandler())

	// Cluster service names resolve to the test servers.
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		for id, a := range c.addr {
			if strings.Contains(address, ".mod-"+id+".svc.") {
				address = a
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	c.svc = &overview.Service{Registry: reg, DB: db, Grants: tickets, Log: quiet, TTL: time.Nanosecond,
		Client: &http.Client{Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true}}}
	return c
}

func (c *chain) figures(subject string) map[string]int64 {
	c.t.Helper()
	got, err := c.svc.Collect(context.Background(), subject)
	if err != nil {
		c.t.Fatal(err)
	}
	out := map[string]int64{}
	for _, m := range got {
		if m.Unavailable {
			c.t.Fatalf("%s is unavailable", m.ID)
		}
		for _, v := range m.Values {
			out[v.Key] = v.Count
		}
	}
	return out
}

func TestEveryModuleAnswersOnlyWithinThePersonsRights(t *testing.T) {
	c := newChain(t)
	c.rights["user:alice"] = []string{"finance.read", "work.read", "leads.read"}
	c.rights["user:bob"] = []string{"work.read"}

	alice := c.figures("user:alice")
	for _, k := range []string{"invoices.overdue", "invoices.outstanding", "contracts.awaiting_signature", "contracts.expiring", "work.urgent_overdue", "work.urgent_open", "leads.new"} {
		if _, ok := alice[k]; !ok {
			t.Errorf("alice has no %s: %v", k, alice)
		}
	}
	// Without finance rights there are no finance figures at all, and none of the leads.
	bob := c.figures("user:bob")
	if len(bob) != 2 {
		t.Fatalf("bob sees %v", bob)
	}
	// Ledger permissions alone do not carry finance.read, and leads.write alone does not show leads.
	c.rights["user:bob"] = []string{"finance.ledger.read", "leads.write"}
	if bob = c.figures("user:bob"); len(bob) != 0 {
		t.Fatalf("bob sees %v", bob)
	}
}

func TestASummaryGrantOpensNothingElse(t *testing.T) {
	c := newChain(t)
	c.rights["user:alice"] = []string{"finance.read"}
	grant := modclient.SummaryTokenPrefix + strings.Repeat("A", 43)
	for _, path := range []string{"/v1/summary", "/v1/invoices", "/v1/contracts", "/v1/reports/aging"} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+c.addr["finance"]+path, nil)
		req.Header.Set("Authorization", "Bearer "+grant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a made-up grant: %d", path, resp.StatusCode)
		}
	}
	// The static admin token is not a grant either: the summary is answered for a person only.
	req, _ := http.NewRequest(http.MethodGet, "http://"+c.addr["finance"]+"/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer "+financeAdmin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("admin token on /v1/summary: %d", resp.StatusCode)
	}
}

func TestACanCoreOutageIsUnavailableNotEmpty(t *testing.T) {
	c := newChain(t)
	c.rights["user:alice"] = []string{"work.read"}
	c.coreSrv.Close()
	got, err := c.svc.Collect(context.Background(), "user:alice")
	if err != nil || len(got) != 1 || !got[0].Unavailable {
		t.Fatalf("%+v %v", got, err)
	}
}

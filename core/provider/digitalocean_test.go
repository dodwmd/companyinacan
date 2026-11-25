package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func loadDOFixtures(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/digitalocean/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

type doDroplet struct {
	ID        int64
	Name      string
	Tags      []string
	Status    string
	pollsLeft int // GETs that still say "new"
	goneAfter int // GETs after DELETE that still return the droplet
	deleted   bool
}

type doFirewall struct {
	ID       string
	Name     string
	Tags     []string
	Inbound  json.RawMessage // as stored from POST/PUT
	Droplets []int64
}

func (fw *doFirewall) json() map[string]any {
	m := map[string]any{"id": fw.ID, "name": fw.Name, "tags": fw.Tags, "status": "succeeded", "droplet_ids": append([]int64{}, fw.Droplets...)}
	if fw.Inbound != nil {
		m["inbound_rules"] = fw.Inbound
	}
	return m
}

// fakeDO is a stateful stand-in for the DigitalOcean API. Lists are paginated
// one item per page so the pagination code is always exercised.
type fakeDO struct {
	t        *testing.T
	fx       map[string]json.RawMessage
	mu       sync.Mutex
	calls    []string
	bodies   map[string][]byte
	authSeen []string
	nextID   int64

	keyName     string // "" = no key with our fingerprint
	droplets    []*doDroplet
	firewalls   []*doFirewall
	actionPolls int // polls of the action that still say in-progress
	actionState string
	hook        func(r *http.Request) (int, string, http.Header) // forced response; code 0 = fall through
}

func newFakeDO(t *testing.T) *fakeDO {
	return &fakeDO{t: t, fx: loadDOFixtures(t), bodies: map[string][]byte{}, nextID: 8080, actionState: "completed"}
}

func (f *fakeDO) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeDO) write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	switch v := body.(type) {
	case string:
		if raw, ok := f.fx[v]; ok {
			_, _ = w.Write(raw)
		} else {
			_, _ = w.Write([]byte(v))
		}
	case nil:
	default:
		_ = json.NewEncoder(w).Encode(v)
	}
}

func dropletJSON(d *doDroplet, status string) map[string]any {
	nets := map[string]any{"v4": []any{}, "v6": []any{}}
	if status == "active" {
		nets = map[string]any{
			"v4": []any{
				map[string]any{"ip_address": "10.116.0.2", "type": "private"},
				map[string]any{"ip_address": "203.0.113.7", "type": "public"},
			},
			"v6": []any{},
		}
	}
	return map[string]any{"id": d.ID, "name": d.Name, "status": status, "tags": d.Tags, "networks": nets}
}

func (d *doDroplet) current() string {
	if d.pollsLeft > 0 {
		return "new"
	}
	return "active"
}

func ownedTags() []string { return []string{"managed-by:can", "company:acme-co"} }

func (f *fakeDO) page(w http.ResponseWriter, r *http.Request, key string, items []any) {
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	out := map[string]any{key: []any{}, "meta": map[string]any{"total": len(items)}}
	if page <= len(items) {
		out[key] = items[page-1 : page]
	}
	links := map[string]any{}
	if page < len(items) {
		links["pages"] = map[string]any{"next": "https://api.digitalocean.com/v2/x?page=" + strconv.Itoa(page+1)}
	}
	out["links"] = links
	f.write(w, 200, out)
}

func (f *fakeDO) find(id string) *doDroplet {
	n, _ := strconv.ParseInt(id, 10, 64)
	for _, d := range f.droplets {
		if d.ID == n && !d.deleted {
			return d
		}
	}
	return nil
}

func (f *fakeDO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		f.bodies[key] = b
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.write(w, 401, "error_unauthorized")
		return
	}
	if f.hook != nil {
		if code, body, hdr := f.hook(r); code != 0 {
			for k, v := range hdr {
				w.Header()[k] = v
			}
			f.write(w, code, body)
			return
		}
	}
	fp, _ := SSHFingerprint(testPubKey)
	switch {
	case key == "GET /account/keys/"+fp:
		switch f.keyName {
		case "":
			f.write(w, 404, "error_not_found")
		case "can-acme-co":
			f.write(w, 200, "key")
		default:
			f.write(w, 200, "key_foreign_name")
		}
	case key == "POST /account/keys":
		f.keyName = "can-acme-co"
		f.write(w, 201, "key")
	case key == "DELETE /account/keys/5011" || key == "DELETE /account/keys/5012":
		f.keyName = ""
		f.write(w, 204, nil)

	case key == "GET /firewalls":
		items := []any{}
		for _, fw := range f.firewalls {
			items = append(items, fw.json())
		}
		f.page(w, r, "firewalls", items)
	case key == "POST /firewalls":
		var req struct {
			Name    string          `json:"name"`
			Tags    []string        `json:"tags"`
			Inbound json.RawMessage `json:"inbound_rules"`
		}
		_ = json.Unmarshal(f.bodies[key], &req)
		fw := &doFirewall{ID: "fw-0000-aaaa", Name: req.Name, Tags: req.Tags, Inbound: req.Inbound}
		f.firewalls = append(f.firewalls, fw)
		f.write(w, 202, map[string]any{"firewall": fw.json()})
	case strings.HasPrefix(key, "PUT /firewalls/"):
		for _, fw := range f.firewalls {
			if "/firewalls/"+fw.ID == r.URL.Path {
				var req struct {
					Inbound json.RawMessage `json:"inbound_rules"`
				}
				_ = json.Unmarshal(f.bodies[key], &req)
				fw.Inbound = req.Inbound
				f.write(w, 200, map[string]any{"firewall": fw.json()})
				return
			}
		}
		f.write(w, 404, "error_not_found")
	case key == "GET /account/keys/5011" || key == "GET /account/keys/5012":
		if f.keyName == "" {
			f.write(w, 404, "error_not_found")
			return
		}
		id := int64(5011)
		if f.keyName != "can-acme-co" {
			id = 5012
		}
		f.write(w, 200, map[string]any{"ssh_key": map[string]any{"id": id, "name": f.keyName, "fingerprint": fp}})
	case strings.HasPrefix(key, "GET /firewalls/"):
		for _, fw := range f.firewalls {
			if "/firewalls/"+fw.ID == r.URL.Path {
				f.write(w, 200, map[string]any{"firewall": fw.json()})
				return
			}
		}
		f.write(w, 404, "error_not_found")
	case strings.HasPrefix(key, "DELETE /firewalls/"):
		for i, fw := range f.firewalls {
			if "/firewalls/"+fw.ID == r.URL.Path {
				f.firewalls = append(f.firewalls[:i], f.firewalls[i+1:]...)
				f.write(w, 204, nil)
				return
			}
		}
		f.write(w, 404, "error_not_found")

	case key == "GET /droplets":
		items := []any{}
		for _, d := range f.droplets {
			if d.deleted {
				continue
			}
			if n := r.URL.Query().Get("name"); n != "" && d.Name != n {
				continue
			}
			if tag := r.URL.Query().Get("tag_name"); tag != "" && !hasTag(d.Tags, tag) {
				continue
			}
			items = append(items, dropletJSON(d, d.current()))
		}
		f.page(w, r, "droplets", items)
	case key == "POST /droplets":
		var req struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		_ = json.Unmarshal(f.bodies[key], &req)
		d := &doDroplet{ID: f.nextID, Name: req.Name, Tags: req.Tags, pollsLeft: 1}
		f.droplets = append(f.droplets, d)
		f.write(w, 202, map[string]any{"droplet": dropletJSON(d, "new"), "links": map[string]any{"actions": []any{map[string]any{"id": 7001, "rel": "create", "href": "x"}}}})
	case strings.HasPrefix(key, "GET /droplets/"):
		d := f.find(strings.TrimPrefix(r.URL.Path, "/droplets/"))
		if d == nil {
			f.write(w, 404, "error_not_found")
			return
		}
		if d.goneAfter > 0 { // after a DELETE request
			d.goneAfter--
			if d.goneAfter == 0 {
				d.deleted = true
			}
			f.write(w, 200, map[string]any{"droplet": dropletJSON(d, "off")})
			return
		}
		st := d.current()
		if d.pollsLeft > 0 {
			d.pollsLeft--
		}
		f.write(w, 200, map[string]any{"droplet": dropletJSON(d, st)})
	case strings.HasPrefix(key, "DELETE /droplets/"):
		d := f.find(strings.TrimPrefix(r.URL.Path, "/droplets/"))
		if d == nil {
			f.write(w, 404, "error_not_found")
			return
		}
		d.goneAfter = 2
		f.write(w, 204, nil)
	case key == "GET /actions/7001":
		if f.actionPolls > 0 {
			f.actionPolls--
			f.write(w, 200, "action_in_progress")
			return
		}
		f.write(w, 200, "action_"+f.actionState)
	default:
		f.t.Errorf("unexpected request %s", key)
		f.write(w, 500, "error_server")
	}
}

func hasTag(tags []string, t string) bool {
	for _, x := range tags {
		if x == t {
			return true
		}
	}
	return false
}

func doSpec() Spec {
	return Spec{Name: "Acme Co", Domain: "acme.example", Options: map[string]any{
		"ssh_public_key_file": "/keys/id.pub",
	}}
}

type doFx struct {
	f      *fakeDO
	p      *DigitalOcean
	st     *Store
	sleeps []time.Duration
}

func doFixture(t *testing.T, f *fakeDO, r Runner) *doFx {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	x := &doFx{f: f, st: NewStore(t.TempDir())}
	d := testDeps(r)
	d.DOBase = srv.URL
	d.Getenv = func(k string) string {
		if k == "DIGITALOCEAN_TOKEN" {
			return testToken
		}
		return ""
	}
	d.ReadFile = func(p string) ([]byte, error) {
		if p == "/keys/id.pub" {
			return []byte(testPubKey + "\n"), nil
		}
		return nil, os.ErrNotExist
	}
	d.Sleep = func(_ context.Context, dur time.Duration) { x.sleeps = append(x.sleeps, dur) }
	x.p = &DigitalOcean{deps: d}
	return x
}

func TestDOCreateFullFlow(t *testing.T) {
	f := newFakeDO(t)
	f.actionPolls = 2
	x := doFixture(t, f, readyRunner())
	var out strings.Builder
	if err := x.p.Create(context.Background(), doSpec(), x.st, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	state, _ := x.st.Load()
	if state.Provider != "digitalocean" || state.SSHKeyID != 5011 || !state.SSHKeyOwned || state.FirewallUUID != "fw-0000-aaaa" ||
		state.Server == nil || state.Server.ID != 8080 || state.Server.IP != "203.0.113.7" || state.Server.Port != 22 {
		t.Fatalf("state: %+v %+v", state, state.Server)
	}
	if !state.Done("digitalocean.bootstrap") || state.K3sVersion != DefaultK3sVersion {
		t.Fatalf("bootstrap not recorded: %+v", state)
	}
	var req struct {
		Name     string   `json:"name"`
		Region   string   `json:"region"`
		Size     string   `json:"size"`
		Image    string   `json:"image"`
		SSHKeys  []any    `json:"ssh_keys"`
		UserData string   `json:"user_data"`
		Tags     []string `json:"tags"`
		IPv6     bool     `json:"ipv6"`
	}
	if err := json.Unmarshal(f.bodies["POST /droplets"], &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "can-acme-co" || req.Region != "fra1" || req.Size != "s-1vcpu-2gb" || req.Image != "ubuntu-24-04-x64" ||
		len(req.SSHKeys) != 1 || req.SSHKeys[0] != float64(5011) || !strings.HasPrefix(req.UserData, "#cloud-config\n") ||
		!hasTag(req.Tags, "managed-by:can") || !hasTag(req.Tags, "company:acme-co") {
		t.Fatalf("droplet request: %+v", req)
	}
	// firewall: 22/80/443 in, tagged for targeting, outbound allowed, never 6443/10250
	fw := string(f.bodies["POST /firewalls"])
	for _, want := range []string{`"ports":"22"`, `"ports":"80"`, `"ports":"443"`, `"outbound_rules"`, `"tags":["managed-by:can","company:acme-co"]`} {
		if !strings.Contains(fw, want) {
			t.Errorf("firewall body missing %s: %s", want, fw)
		}
	}
	if strings.Contains(fw, "6443") || strings.Contains(fw, "10250") {
		t.Error("firewall opens the k3s API or kubelet")
	}
	if strings.Contains(out.String(), testToken) {
		t.Error("token printed")
	}
	for _, a := range f.authSeen {
		if a != "Bearer "+testToken {
			t.Errorf("auth header %q", a)
		}
	}
	if f.count("GET /actions/7001") != 3 {
		t.Errorf("action polled %d times", f.count("GET /actions/7001"))
	}
	if !strings.Contains(string(f.bodies["POST /firewalls"]), `"addresses":["0.0.0.0/0","::/0"]`) {
		t.Errorf("ssh must be open to all without operator_ip: %s", fw)
	}
}

func TestDOFirewallRestrictsSSHToOperator(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	spec := doSpec()
	spec.Options["operator_ip"] = "198.51.100.9"
	if err := x.p.Create(context.Background(), spec, x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	var fw struct {
		In []struct {
			Ports   string `json:"ports"`
			Sources struct {
				Addresses []string `json:"addresses"`
			} `json:"sources"`
		} `json:"inbound_rules"`
	}
	if err := json.Unmarshal(f.bodies["POST /firewalls"], &fw); err != nil {
		t.Fatal(err)
	}
	for _, r := range fw.In {
		switch r.Ports {
		case "22":
			if len(r.Sources.Addresses) != 1 || r.Sources.Addresses[0] != "198.51.100.9/32" {
				t.Errorf("ssh sources %v", r.Sources.Addresses)
			}
		case "80", "443":
			if len(r.Sources.Addresses) != 2 {
				t.Errorf("%s sources %v", r.Ports, r.Sources.Addresses)
			}
		}
	}
}

func TestDOCreateIsIdempotent(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	for i := 0; i < 2; i++ {
		if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	for _, c := range []string{"POST /account/keys", "POST /firewalls", "POST /droplets"} {
		if n := f.count(c); n != 1 {
			t.Errorf("%s called %d times", c, n)
		}
	}
}

func TestDOResumesAfterCrashBetweenCreateAndStateWrite(t *testing.T) {
	f := newFakeDO(t)
	// a previous run created everything but died before saving any state
	f.keyName = "can-acme-co"
	want := (doConf{fwName: "can-acme-co", ownerTags: ownedTags()}).firewall().InboundRules
	rules, _ := json.Marshal(want)
	f.firewalls = []*doFirewall{{ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: ownedTags(), Inbound: rules}}
	f.droplets = []*doDroplet{{ID: 8080, Name: "can-acme-co", Tags: ownedTags()}}
	x := doFixture(t, f, readyRunner())
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("PUT") != 0 {
		t.Fatalf("matching firewall was rewritten: %v", f.calls)
	}
	for _, c := range []string{"POST /account/keys", "POST /firewalls", "POST /droplets"} {
		if f.count(c) != 0 {
			t.Fatalf("%s re-created", c)
		}
	}
	state, _ := x.st.Load()
	if state.Server == nil || state.Server.ID != 8080 || state.FirewallUUID != "fw-0000-aaaa" || state.SSHKeyOwned {
		t.Fatalf("not adopted: %+v", state)
	}
}

func TestDORefusesForeignDropletAndFirewall(t *testing.T) {
	t.Run("droplet", func(t *testing.T) {
		f := newFakeDO(t)
		f.droplets = []*doDroplet{{ID: 8080, Name: "can-acme-co", Tags: []string{"prod"}}}
		x := doFixture(t, f, readyRunner())
		err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "not created by can") {
			t.Fatalf("want refusal, got %v", err)
		}
		if f.count("POST /droplets") != 0 || f.count("DELETE") != 0 {
			t.Fatalf("touched a foreign droplet: %v", f.calls)
		}
	})
	t.Run("firewall", func(t *testing.T) {
		f := newFakeDO(t)
		f.firewalls = []*doFirewall{{ID: "fw-9999-zzzz", Name: "can-acme-co", Tags: []string{"web"}}}
		x := doFixture(t, f, readyRunner())
		err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "not created by can") {
			t.Fatalf("want refusal, got %v", err)
		}
		if f.count("POST /droplets") != 0 || f.count("DELETE") != 0 {
			t.Fatalf("went on after refusal: %v", f.calls)
		}
	})
	t.Run("droplet of another company with same name", func(t *testing.T) {
		f := newFakeDO(t)
		f.droplets = []*doDroplet{{ID: 8080, Name: "can-acme-co", Tags: []string{"managed-by:can", "company:other"}}}
		x := doFixture(t, f, readyRunner())
		if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err == nil {
			t.Fatal("adopted another company's droplet")
		}
	})
}

func TestDOReusesExistingSSHKeyAndDoesNotDeleteIt(t *testing.T) {
	f := newFakeDO(t)
	f.keyName = "my-laptop"
	x := doFixture(t, f, readyRunner())
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /account/keys") != 0 {
		t.Fatal("uploaded a key that already exists")
	}
	if state, _ := x.st.Load(); state.SSHKeyID != 5012 || state.SSHKeyOwned {
		t.Fatalf("key ownership: %+v", state)
	}
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /account/keys") != 0 {
		t.Fatal("deleted a key can did not upload")
	}
}

func TestDODestroyOrderWaitsAndIsIdempotent(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	var out strings.Builder
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var dels []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "DELETE") {
			dels = append(dels, c)
		}
	}
	if len(dels) != 3 || dels[0] != "DELETE /droplets/8080" || dels[1] != "DELETE /firewalls/fw-0000-aaaa" || dels[2] != "DELETE /account/keys/5011" {
		t.Fatalf("delete order: %v", dels)
	}
	// the firewall is only deleted once the droplet is confirmed gone (404)
	last := -1
	for i, c := range f.calls {
		if c == "DELETE /firewalls/fw-0000-aaaa" {
			last = i
		}
	}
	if f.calls[last-1] != "GET /firewalls/fw-0000-aaaa" || f.calls[last-2] != "GET /droplets/8080" {
		t.Fatalf("firewall deleted before droplet gone: %v", f.calls)
	}
	if len(f.droplets) == 0 || !f.droplets[0].deleted || len(f.firewalls) != 0 || f.keyName != "" {
		t.Fatal("resources left")
	}
	if _, err := os.Stat(x.st.Path); !os.IsNotExist(err) {
		t.Fatal("state file not removed")
	}
	out.Reset()
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("second destroy: %q", out.String())
	}
}

func TestDODestroyToleratesAlreadyDeletedDroplet(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	f.droplets[0].deleted = true
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /droplets") != 0 || len(f.firewalls) != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestDODestroyRechecksTagsRightBeforeDelete(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	f.droplets[0].Tags = []string{"prod"} // someone re-tagged the droplet
	err := x.p.Destroy(context.Background(), doSpec(), x.st, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete droplet") {
		t.Fatalf("want refusal, got %v", err)
	}
	if f.count("DELETE") != 0 {
		t.Fatalf("deleted something: %v", f.calls)
	}
}

func TestDODestroyWithEmptyStateFindsOwnResourcesOnly(t *testing.T) {
	f := newFakeDO(t)
	f.keyName = "can-acme-co"
	f.droplets = []*doDroplet{{ID: 8080, Name: "can-acme-co", Tags: ownedTags()}}
	f.firewalls = []*doFirewall{
		{ID: "fw-9999-zzzz", Name: "can-acme-co-other", Tags: []string{"web"}},
		{ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: ownedTags()},
	}
	x := doFixture(t, f, readyRunner())
	var out strings.Builder
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !f.droplets[0].deleted || len(f.firewalls) != 1 || f.firewalls[0].ID != "fw-9999-zzzz" || f.keyName != "can-acme-co" {
		t.Fatalf("after destroy: %+v %+v key=%q", f.droplets[0], f.firewalls, f.keyName)
	}
}

func TestDODestroyEmptyStateLeavesForeignSameNameAlone(t *testing.T) {
	f := newFakeDO(t)
	f.droplets = []*doDroplet{{ID: 8080, Name: "can-acme-co", Tags: []string{"prod"}}}
	f.firewalls = []*doFirewall{{ID: "fw-9999-zzzz", Name: "can-acme-co", Tags: []string{"web"}}}
	f.keyName = "my-laptop"
	x := doFixture(t, f, readyRunner())
	var out strings.Builder
	if err := x.p.Destroy(context.Background(), doSpec(), x.st, &out); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE") != 0 || !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("calls %v out %q", f.calls, out.String())
	}
}

func TestDOPaginationFindsOwnedItemsOnLaterPages(t *testing.T) {
	f := newFakeDO(t)
	f.droplets = []*doDroplet{
		{ID: 1, Name: "web-1", Tags: []string{"web"}},
		{ID: 2, Name: "web-2", Tags: []string{"web"}},
		{ID: 8080, Name: "can-acme-co", Tags: ownedTags()},
	}
	f.firewalls = []*doFirewall{
		{ID: "a", Name: "x", Tags: nil}, {ID: "b", Name: "y", Tags: nil},
		{ID: "fw-0000-aaaa", Name: "can-acme-co", Tags: ownedTags()},
	}
	x := doFixture(t, f, readyRunner())
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /droplets") != 0 || f.count("POST /firewalls") != 0 {
		t.Fatalf("missed an item on a later page: %v", f.calls)
	}
	if f.count("GET /firewalls?") != 0 && f.count("GET /firewalls") < 3 {
		t.Fatalf("did not page: %v", f.calls)
	}
}

func TestDORateLimitAndRetryRules(t *testing.T) {
	t.Run("429 on POST is retried after RateLimit-Reset", func(t *testing.T) {
		f := newFakeDO(t)
		var n int
		f.hook = func(r *http.Request) (int, string, http.Header) {
			if r.Method == "POST" && r.URL.Path == "/account/keys" && n < 2 {
				n++
				h := http.Header{}
				h.Set("RateLimit-Reset", strconv.FormatInt(time.Now().Add(7*time.Second).Unix(), 10))
				return 429, "error_rate_limit", h
			}
			return 0, "", nil
		}
		x := doFixture(t, f, readyRunner())
		if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
		if f.count("POST /account/keys") != 3 {
			t.Fatalf("calls: %d", f.count("POST /account/keys"))
		}
		var waited bool
		for _, s := range x.sleeps {
			if s >= 5*time.Second && s <= 8*time.Second {
				waited = true
			}
		}
		if !waited {
			t.Fatalf("did not wait for RateLimit-Reset: %v", x.sleeps)
		}
	})
	t.Run("500 on POST is not retried", func(t *testing.T) {
		f := newFakeDO(t)
		f.hook = func(r *http.Request) (int, string, http.Header) {
			if r.Method == "POST" && r.URL.Path == "/droplets" {
				return 500, "error_server", nil
			}
			return 0, "", nil
		}
		x := doFixture(t, f, readyRunner())
		if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err == nil {
			t.Fatal("want error")
		}
		if f.count("POST /droplets") != 1 {
			t.Fatalf("POST retried: %d", f.count("POST /droplets"))
		}
	})
	t.Run("500 on GET is retried", func(t *testing.T) {
		f := newFakeDO(t)
		var n int
		f.hook = func(r *http.Request) (int, string, http.Header) {
			if r.Method == "GET" && r.URL.Path == "/firewalls" && n < 2 {
				n++
				return 503, "error_server", nil
			}
			return 0, "", nil
		}
		x := doFixture(t, f, readyRunner())
		if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rate limit forever gives up", func(t *testing.T) {
		f := newFakeDO(t)
		f.hook = func(*http.Request) (int, string, http.Header) { return 429, "error_rate_limit", nil }
		x := doFixture(t, f, readyRunner())
		err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "digitalocean API") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDOErrorsNeverEchoTheToken(t *testing.T) {
	f := newFakeDO(t)
	f.hook = func(r *http.Request) (int, string, http.Header) {
		if r.Method == "POST" && r.URL.Path == "/droplets" {
			return 422, fmt.Sprintf(`{"id":"unprocessable_entity","message":"bad header Authorization: Bearer %s and %s"}`, testToken, testToken), nil
		}
		return 0, "", nil
	}
	x := doFixture(t, f, readyRunner())
	err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestDOOptionValidation(t *testing.T) {
	x := doFixture(t, newFakeDO(t), readyRunner())
	for name, mod := range map[string]func(Spec){
		"port":           func(s Spec) { s.Options["port"] = 2222 },
		"quoted port":    func(s Spec) { s.Options["port"] = "22" },
		"unknown option": func(s Spec) { s.Options["datacentre"] = "fra1" },
		"bad name":       func(s Spec) { s.Options["server_name"] = "Bad Name" },
		"bad region":     func(s Spec) { s.Options["region"] = "fra 1; rm" },
		"bad size":       func(s Spec) { s.Options["size"] = "$(x)" },
		"bad fingerprint": func(s Spec) {
			s.Options["ssh_key_fingerprints"] = []any{"nope"}
		},
	} {
		spec := doSpec()
		mod(spec)
		if _, err := x.p.Plan(context.Background(), spec, x.st); err == nil {
			t.Errorf("%s: want error", name)
		}
		if err := x.p.Create(context.Background(), spec, x.st, &strings.Builder{}); err == nil {
			t.Errorf("%s: create accepted it", name)
		}
	}
	if x.f.count("") != 0 {
		t.Fatalf("API called despite bad options: %v", x.f.calls)
	}
}

func TestDOAdditionalKeyFingerprintsAreAttached(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	spec := doSpec()
	spec.Options["ssh_key_fingerprints"] = []any{"3b:16:bf:e4:8b:00:8b:b8:59:8c:a9:d3:f0:19:45:fa"}
	if err := x.p.Create(context.Background(), spec, x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.bodies["POST /droplets"]), `"3b:16:bf:e4:8b:00:8b:b8:59:8c:a9:d3:f0:19:45:fa"`) {
		t.Fatalf("body: %s", f.bodies["POST /droplets"])
	}
}

func TestDOActionErroredAndContextCancel(t *testing.T) {
	t.Run("errored action", func(t *testing.T) {
		f := newFakeDO(t)
		f.actionState = "errored"
		x := doFixture(t, f, readyRunner())
		err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "errored") {
			t.Fatalf("got %v", err)
		}
		// the droplet id was saved before polling, so a re-run resumes
		if state, _ := x.st.Load(); state.Server == nil || state.Server.ID != 8080 {
			t.Fatalf("id not saved: %+v", state)
		}
	})
	t.Run("never active times out", func(t *testing.T) {
		f := newFakeDO(t)
		f.actionPolls = 1 << 30
		x := doFixture(t, f, readyRunner())
		spec := doSpec()
		spec.Options["wait_seconds"] = 30
		err := x.p.Create(context.Background(), spec, x.st, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "did not") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := newFakeDO(t)
		f.actionPolls = 1 << 30
		x := doFixture(t, f, readyRunner())
		ctx, cancel := context.WithCancel(context.Background())
		x.p.deps.Sleep = func(context.Context, time.Duration) { cancel() }
		err := x.p.Create(ctx, doSpec(), x.st, &strings.Builder{})
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDOPlanAndStatusAndAddress(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	acts, err := x.p.Plan(context.Background(), doSpec(), x.st)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 4 || acts[0].ID != "digitalocean.ssh-key" || acts[3].ID != "digitalocean.bootstrap" || acts[2].Done {
		t.Fatalf("plan: %+v", acts)
	}
	if !strings.Contains(acts[2].Description, "s-1vcpu-2gb") || !strings.Contains(acts[1].Description, "any") {
		t.Fatalf("plan text: %+v", acts)
	}
	if f.count("") != 0 {
		t.Fatal("plan touched the API")
	}
	if st, err := x.p.Status(context.Background(), doSpec(), x.st); err != nil || st.Phase != PhaseAbsent {
		t.Fatalf("status before create: %+v %v", st, err)
	}
	if _, err := x.p.Address(x.st); err == nil {
		t.Fatal("address before create")
	}
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if st, err := x.p.Status(context.Background(), doSpec(), x.st); err != nil || st.Phase != PhaseReady {
		t.Fatalf("status: %+v %v", st, err)
	}
	if a, err := x.p.Address(x.st); err != nil || a != "203.0.113.7" {
		t.Fatalf("address %q %v", a, err)
	}
	f.droplets[0].deleted = true
	if st, _ := x.p.Status(context.Background(), doSpec(), x.st); st.Phase != PhaseAbsent {
		t.Fatalf("status after delete: %+v", st)
	}
}

func TestDOTokenResolutionErrorsAreClear(t *testing.T) {
	x := doFixture(t, newFakeDO(t), readyRunner())
	x.p.deps.Getenv = func(string) string { return "" }
	err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "DIGITALOCEAN_TOKEN") {
		t.Fatalf("got %v", err)
	}
}

func TestDOTagsAreValidAndCompanySlugIsUsed(t *testing.T) {
	f := newFakeDO(t)
	x := doFixture(t, f, readyRunner())
	spec := doSpec()
	spec.Name = "Müller & Söhne, Inc."
	if err := x.p.Create(context.Background(), spec, x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range f.droplets[0].Tags {
		for _, r := range tag { // DO tags: letters, numbers, colons, dashes, underscores
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != ':' && r != '-' && r != '_' {
				t.Errorf("tag %q has invalid rune %q", tag, r)
			}
		}
	}
}

func TestDOTargetUsesPort22AndKnownHosts(t *testing.T) {
	x := doFixture(t, newFakeDO(t), readyRunner())
	if err := x.p.Create(context.Background(), doSpec(), x.st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	spec := doSpec()
	spec.Dir = "/co"
	spec.Options["port"] = 2222 // ignored for TargetFor on managed clouds
	tg, err := TargetFor(spec, x.st, x.p.deps)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Port != 22 || tg.KnownHosts != "/co/.can/known_hosts" || tg.Host != "203.0.113.7" {
		t.Fatalf("%+v", tg)
	}
}

func TestNewReturnsDigitalOcean(t *testing.T) {
	p, err := New("digitalocean", Deps{})
	if err != nil || p.Name() != "digitalocean" {
		t.Fatalf("%v %v", p, err)
	}
	if _, isStub := p.(stub); isStub {
		t.Fatal("still a stub")
	}
}

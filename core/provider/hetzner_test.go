package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "tok_live_0123456789abcdef"

// A real ed25519 public key (generated for tests; the private half was discarded).
const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ2o8e4mJw4mxk8t9S8YxqY0k3q8bX9r1Zk5c2n7t4Qd can-test"

func loadFixtures(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/hetzner/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// fakeHetzner is an httptest handler with just enough behaviour to exercise
// idempotency: it remembers what was created and counts calls.
type fakeHetzner struct {
	t        *testing.T
	fx       map[string]json.RawMessage
	mu       sync.Mutex
	calls    []string
	bodies   map[string][]byte
	authSeen []string

	haveKey, haveFW, haveServer bool
	keyOwned                    bool // key list shows can labels
	foreignFW, foreignServer    bool // same-named resource not created by can
	pollsUntilRunning           int
	status                      map[string]int // "METHOD /path" -> forced status
	forced                      map[string]string
}

func newFakeHetzner(t *testing.T) *fakeHetzner {
	return &fakeHetzner{t: t, fx: loadFixtures(t), bodies: map[string][]byte{}, status: map[string]int{}, forced: map[string]string{}}
}

func (f *fakeHetzner) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeHetzner) write(w http.ResponseWriter, code int, fixture string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(f.fx[fixture])
}

func (f *fakeHetzner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
	if r.Body != nil {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		f.bodies[key] = []byte(b.String())
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.write(w, 401, "error_unauthorized")
		return
	}
	if code, ok := f.status[key]; ok && code != 0 {
		if code != 200 {
			f.write(w, code, f.forced[key])
			return
		}
	}
	switch key {
	case "GET /ssh_keys":
		if f.haveKey && f.keyOwned {
			f.write(w, 200, "ssh_key_list_owned")
		} else if f.haveKey {
			f.write(w, 200, "ssh_key_list_one")
		} else {
			f.write(w, 200, "ssh_key_list_empty")
		}
	case "POST /ssh_keys":
		f.haveKey, f.keyOwned = true, true
		f.write(w, 201, "ssh_key_created")
	case "DELETE /ssh_keys/1011":
		f.haveKey = false
		f.write(w, 200, "empty")
	case "GET /firewalls":
		if f.haveFW && f.foreignFW {
			f.write(w, 200, "firewall_list_foreign")
		} else if f.haveFW {
			f.write(w, 200, "firewall_list_one")
		} else {
			f.write(w, 200, "firewall_list_empty")
		}
	case "POST /firewalls":
		f.haveFW = true
		f.write(w, 201, "firewall_created")
	case "GET /firewalls/2022":
		switch {
		case !f.haveFW:
			f.write(w, 404, "error_not_found")
		case f.foreignFW:
			f.write(w, 200, "firewall_get_foreign")
		default:
			f.write(w, 200, "firewall_get")
		}
	case "DELETE /firewalls/2022":
		f.haveFW = false
		f.write(w, 200, "empty")
	case "GET /servers":
		if f.haveServer && f.foreignServer {
			f.write(w, 200, "server_list_foreign")
		} else if f.haveServer {
			f.write(w, 200, "server_list_one")
		} else {
			f.write(w, 200, "server_list_empty")
		}
	case "POST /servers":
		f.haveServer = true
		f.write(w, 201, "server_initializing")
	case "GET /servers/3033":
		if !f.haveServer {
			f.write(w, 404, "error_not_found")
			return
		}
		if f.foreignServer {
			f.write(w, 200, "server_foreign")
			return
		}
		if f.pollsUntilRunning > 0 {
			f.pollsUntilRunning--
			f.write(w, 200, "server_initializing")
			return
		}
		f.write(w, 200, "server_running")
	case "DELETE /servers/3033":
		if !f.haveServer {
			f.write(w, 404, "error_not_found")
			return
		}
		f.haveServer = false
		f.write(w, 200, "empty")
	default:
		f.t.Errorf("unexpected request %s", key)
		f.write(w, 500, "empty")
	}
}

func hetznerSpec() Spec {
	return Spec{Name: "Acme Co", Domain: "acme.example", Options: map[string]any{
		"token_secret_ref":    "env:HCLOUD_TOKEN",
		"ssh_public_key_file": "/keys/id.pub",
	}}
}

func hetznerFixture(t *testing.T, f *fakeHetzner, r Runner) (*Hetzner, *Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d := testDeps(r)
	d.APIBase = srv.URL
	d.Getenv = func(k string) string {
		if k == "HCLOUD_TOKEN" {
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
	return &Hetzner{deps: d}, NewStore(t.TempDir()), srv
}

func readyRunner() *fakeRunner {
	return newFakeRunner(fakeRule{contains: "bootstrap.done", out: statusReady})
}

func TestHetznerCreateFullFlow(t *testing.T) {
	f := newFakeHetzner(t)
	f.pollsUntilRunning = 2
	p, st, _ := hetznerFixture(t, f, readyRunner())
	var out strings.Builder
	if err := p.Create(context.Background(), hetznerSpec(), st, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	state, _ := st.Load()
	if state.SSHKeyID != 1011 || !state.SSHKeyOwned || state.FirewallID != 2022 || state.Server == nil || state.Server.ID != 3033 || state.Server.IP != "203.0.113.7" {
		t.Fatalf("state: %+v %+v", state, state.Server)
	}
	if !state.Done("hetzner.bootstrap") || state.K3sVersion != DefaultK3sVersion {
		t.Fatalf("bootstrap not recorded: %+v", state)
	}

	// The create body carries cloud-init, key, firewall and labels.
	var req CreateServerRequest
	if err := json.Unmarshal(f.bodies["POST /servers"], &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "can-acme-co" || req.ServerType != "cx23" || req.Image != "ubuntu-24.04" || req.Location != "fsn1" ||
		len(req.SSHKeys) != 1 || req.SSHKeys[0] != 1011 || len(req.Firewalls) != 1 || req.Firewalls[0].Firewall != 2022 ||
		!strings.HasPrefix(req.UserData, "#cloud-config\n") || req.Labels["managed-by"] != "can" || !req.StartAfter {
		t.Fatalf("create request: %+v", req)
	}
	// Firewall: ssh/http/https/icmp, never 6443.
	fw := string(f.bodies["POST /firewalls"])
	for _, want := range []string{`"port":"22"`, `"port":"80"`, `"port":"443"`, `"protocol":"icmp"`} {
		if !strings.Contains(fw, want) {
			t.Errorf("firewall body missing %s: %s", want, fw)
		}
	}
	if strings.Contains(fw, "6443") {
		t.Error("firewall opens the k3s API")
	}
	// The token went only in the Authorization header and never into output.
	if strings.Contains(out.String(), testToken) {
		t.Error("token printed")
	}
	for _, a := range f.authSeen {
		if a != "Bearer "+testToken {
			t.Errorf("auth header %q", a)
		}
	}
}

func TestHetznerCreateIsIdempotent(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	for i := 0; i < 2; i++ {
		if err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	for _, c := range []string{"POST /ssh_keys", "POST /firewalls", "POST /servers"} {
		if n := f.count(c); n != 1 {
			t.Errorf("%s called %d times", c, n)
		}
	}
}

func TestHetznerResumesAfterInterruptedRun(t *testing.T) {
	f := newFakeHetzner(t)
	f.haveServer = true // created by a previous run that died before saving state
	p, st, _ := hetznerFixture(t, f, readyRunner())
	if err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST /servers"); n != 0 {
		t.Fatalf("server created again (%d)", n)
	}
	if state, _ := st.Load(); state.Server == nil || state.Server.ID != 3033 {
		t.Fatalf("existing server not adopted: %+v", state)
	}
}

func TestHetznerReusesExistingSSHKeyAndDoesNotDeleteIt(t *testing.T) {
	f := newFakeHetzner(t)
	f.haveKey = true
	p, st, _ := hetznerFixture(t, f, readyRunner())
	if err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /ssh_keys") != 0 {
		t.Fatal("uploaded a key that already exists")
	}
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE /ssh_keys") != 0 {
		t.Fatal("deleted a key can did not upload")
	}
}

func TestHetznerDestroyDeletesEverythingAndIsIdempotent(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	if err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &out); err != nil {
		t.Fatal(err)
	}
	if f.haveServer || f.haveFW || f.haveKey {
		t.Fatalf("resources left: server=%v fw=%v key=%v", f.haveServer, f.haveFW, f.haveKey)
	}
	if _, err := os.Stat(st.Path); !os.IsNotExist(err) {
		t.Fatal("state file not removed")
	}
	out.Reset()
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &out); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE") != 3 || !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("second destroy must be a no-op: %q %v", out.String(), f.calls)
	}
}

func TestHetznerDestroyToleratesAlreadyDeletedServer(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	_ = p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	f.haveServer = false // deleted in the console
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
}

func TestHetznerFirewallInUseIsRetried(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	_ = p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/firewalls/") && n < 2 {
			n++
			f.write(w, 422, "error_in_use")
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p.deps.APIBase = srv.URL
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("retries: %d", n)
	}
}

func TestHetznerBadTokenNeverLeaks(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	p.deps.Getenv = func(string) string { return "wrong-token-value-999" }
	err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	if err == nil {
		t.Fatal("want auth error")
	}
	if strings.Contains(err.Error(), "wrong-token-value-999") {
		t.Fatal("token in error")
	}
	// The API echoed a (different) token in its message: pattern redaction covers it
	// only when it is the one we hold, so assert the message is at least surfaced.
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("error should carry the API code: %v", err)
	}
}

func TestHetznerAPIEchoedTokenIsRedacted(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	// the fixture message contains testToken; hold that token so it is known
	p.deps.Getenv = func(string) string { return testToken }
	f.status["GET /ssh_keys"] = 401
	f.forced["GET /ssh_keys"] = "error_unauthorized"
	err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked or no error: %v", err)
	}
	if !strings.Contains(err.Error(), Redacted) {
		t.Fatalf("expected redaction marker: %v", err)
	}
}

func TestHetznerRateLimitRetriesThenSucceeds(t *testing.T) {
	f := newFakeHetzner(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			f.write(w, 429, "error_rate_limit")
			return
		}
		f.write(w, 200, "server_list_empty")
	}))
	defer srv.Close()
	c := NewHetznerClient(srv.URL, testToken, http.DefaultClient, NewRedactor(), noSleep)
	_, found, err := c.FindServer(context.Background(), "x")
	if err != nil || found || hits != 3 {
		t.Fatalf("found=%v err=%v hits=%d", found, err, hits)
	}
}

func TestHetznerRateLimitGivesUp(t *testing.T) {
	f := newFakeHetzner(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.write(w, 429, "error_rate_limit") }))
	defer srv.Close()
	c := NewHetznerClient(srv.URL, testToken, http.DefaultClient, NewRedactor(), noSleep)
	_, _, err := c.FindServer(context.Background(), "x")
	var ae *APIError
	if err == nil || !errors.As(err, &ae) || ae.Status != 429 || ae.Code != "rate_limit_exceeded" {
		t.Fatalf("%v", err)
	}
}

func TestHetznerPostIsNotRetriedOnServerError(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(500) }))
	defer srv.Close()
	c := NewHetznerClient(srv.URL, testToken, http.DefaultClient, NewRedactor(), noSleep)
	if _, err := c.CreateServer(context.Background(), CreateServerRequest{Name: "x"}); err == nil || hits != 1 {
		t.Fatalf("err=%v hits=%d (a retried POST could create two servers)", err, hits)
	}
}

func TestHetznerStatus(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	ctx := context.Background()

	s, err := p.Status(ctx, hetznerSpec(), st)
	if err != nil || s.Phase != PhaseAbsent {
		t.Fatalf("before create: %+v %v", s, err)
	}
	_ = p.Create(ctx, hetznerSpec(), st, &strings.Builder{})
	if s, _ = p.Status(ctx, hetznerSpec(), st); s.Phase != PhaseReady || !PreflightOK(s.Checks) {
		t.Fatalf("after create: %+v", s)
	}
	f.pollsUntilRunning = 1
	if s, _ = p.Status(ctx, hetznerSpec(), st); s.Phase != PhaseProvisioning {
		t.Fatalf("initializing: %+v", s)
	}
	f.haveServer = false
	if s, _ = p.Status(ctx, hetznerSpec(), st); s.Phase != PhaseAbsent {
		t.Fatalf("server gone: %+v", s)
	}
}

func TestHetznerAddress(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	if _, err := p.Address(st); err == nil {
		t.Fatal("want error before create")
	}
	_ = p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	if ip, err := p.Address(st); err != nil || ip != "203.0.113.7" {
		t.Fatalf("%q %v", ip, err)
	}
}

func TestHetznerPlan(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	spec := hetznerSpec()
	spec.Options["operator_ip"] = "198.51.100.9"
	acts, err := p.Plan(context.Background(), spec, st)
	if err != nil || len(acts) != 4 {
		t.Fatalf("%v %v", acts, err)
	}
	for _, a := range acts {
		if a.Done {
			t.Errorf("%s done before create", a.ID)
		}
	}
	if !strings.Contains(acts[1].Description, "198.51.100.9") {
		t.Errorf("operator restriction not shown: %s", acts[1].Description)
	}
	if len(f.calls) != 0 {
		t.Fatalf("Plan must not call the API: %v", f.calls)
	}
	_ = p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	acts, _ = p.Plan(context.Background(), hetznerSpec(), st)
	for _, a := range acts {
		if !a.Done {
			t.Errorf("%s not done after create", a.ID)
		}
	}
}

func TestHetznerBootstrapTimeout(t *testing.T) {
	f := newFakeHetzner(t)
	r := newFakeRunner(fakeRule{contains: "bootstrap.done", err: fmt.Errorf("connection refused")})
	p, st, _ := hetznerFixture(t, f, r)
	spec := hetznerSpec()
	spec.Options["wait_seconds"] = 1
	err := p.Create(context.Background(), spec, st, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "cloud-init") {
		t.Fatalf("%v", err)
	}
}

func TestHetznerOptionValidation(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	spec := hetznerSpec()
	spec.Options["server_name"] = "Bad Name!"
	if _, err := p.Plan(context.Background(), spec, st); err == nil {
		t.Error("bad server name accepted")
	}
	spec = hetznerSpec()
	spec.Options["k3s_version"] = "latest"
	if err := p.Create(context.Background(), spec, st, &strings.Builder{}); err == nil {
		t.Error("unpinned k3s accepted")
	}
	spec = hetznerSpec()
	spec.Options["token_secret_ref"] = "env:NOPE"
	p.deps.Getenv = func(string) string { return "" }
	if err := p.Create(context.Background(), spec, st, &strings.Builder{}); err == nil || len(f.calls) != 0 {
		t.Error("missing token must fail before any API call")
	}
}

func TestSSHFingerprint(t *testing.T) {
	fp, err := SSHFingerprint(testPubKey)
	if err != nil || len(strings.Split(fp, ":")) != 16 {
		t.Fatalf("%q %v", fp, err)
	}
	for _, bad := range []string{"", "ssh-ed25519", "ssh-ed25519 !!!notbase64"} {
		if _, err := SSHFingerprint(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestHetznerRefusesToAdoptForeignFirewall(t *testing.T) {
	f := newFakeHetzner(t)
	f.haveFW, f.foreignFW = true, true
	p, st, _ := hetznerFixture(t, f, readyRunner())
	err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "not created by can") {
		t.Fatalf("%v", err)
	}
	if f.count("POST /servers") != 0 || f.count("DELETE") != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
	if s, _ := st.Load(); s.FirewallID != 0 {
		t.Fatal("foreign firewall recorded in state")
	}
}

func TestHetznerRefusesToAdoptForeignServer(t *testing.T) {
	f := newFakeHetzner(t)
	f.haveServer, f.foreignServer = true, true
	p, st, _ := hetznerFixture(t, f, readyRunner())
	err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "not created by can") {
		t.Fatalf("%v", err)
	}
	if f.count("POST /servers") != 0 || f.count("DELETE") != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
	if s, _ := st.Load(); s.Server != nil {
		t.Fatal("foreign server recorded in state")
	}
}

func TestHetznerDestroyNeverDeletesForeignResources(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	if err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	// someone relabelled / replaced the resources after can created them
	f.foreignServer = true
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{}); err == nil || f.count("DELETE /servers") != 0 {
		t.Fatalf("server delete: err=%v calls=%v", err, f.calls)
	}
	f.foreignServer = false
	f.haveServer = false
	f.foreignFW = true
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{}); err == nil || f.count("DELETE /firewalls") != 0 {
		t.Fatalf("firewall delete: err=%v calls=%v", err, f.calls)
	}
}

func TestHetznerRejectsNonDefaultSSHPort(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	spec := hetznerSpec()
	spec.Options["port"] = 2222
	if err := p.Create(context.Background(), spec, st, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("%v", err)
	}
	if _, err := p.Plan(context.Background(), spec, st); err == nil {
		t.Fatal("plan accepted port")
	}
	if len(f.calls) != 0 {
		t.Fatal("API called")
	}
	spec.Options["port"] = 22
	if _, err := p.Plan(context.Background(), spec, st); err != nil {
		t.Fatal(err)
	}
}

func TestHomeExpansion(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for in, want := range map[string]string{"~/.ssh/id.pub": "/home/tester/.ssh/id.pub", "~": "/home/tester", "/abs/x": "/abs/x", "rel~/x": "rel~/x", "~other/x": "~other/x"} {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q)=%q want %q", in, got, want)
		}
	}
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	var read []string
	inner := p.deps.ReadFile
	p.deps.ReadFile = func(path string) ([]byte, error) { read = append(read, path); return inner("/keys/id.pub") }
	spec := hetznerSpec()
	spec.Options["ssh_public_key_file"] = "~/keys/id.pub"
	if err := p.Create(context.Background(), spec, st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if len(read) == 0 || read[0] != "/home/tester/keys/id.pub" {
		t.Fatalf("read %v", read)
	}
	// secret refs
	d := testDeps(nil)
	d.ReadFile = func(path string) ([]byte, error) { read = append(read, path); return []byte("tok-123456"), nil }
	if _, err := d.ResolveSecret("file:~/.hcloud"); err != nil || read[len(read)-1] != "/home/tester/.hcloud" {
		t.Fatalf("%v %v", err, read)
	}
}

func TestHetznerRateLimitResetHeader(t *testing.T) {
	now := time.Unix(1000, 0)
	if d := resetWait("1007", now); d != 7*time.Second {
		t.Fatal(d)
	}
	if d := resetWait("999", now); d != 0 {
		t.Fatal(d)
	}
	if d := resetWait("99999", now); d != time.Minute {
		t.Fatal(d)
	}
	if d := resetWait("junk", now); d != 0 {
		t.Fatal(d)
	}
}

func TestHetznerBootstrapTimeoutShowsLastSSHError(t *testing.T) {
	f := newFakeHetzner(t)
	r := newFakeRunner(fakeRule{contains: "bootstrap.done", err: fmt.Errorf("ssh: connection refused")})
	p, st, _ := hetznerFixture(t, f, r)
	spec := hetznerSpec()
	spec.Options["wait_seconds"] = 1
	err := p.Create(context.Background(), spec, st, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("%v", err)
	}
}

func TestHetznerDestroyWaitsForServerBeforeFirewall(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	_ = p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{})
	_ = p.Destroy(context.Background(), hetznerSpec(), st, &strings.Builder{})
	di, ds := -1, -1
	for i, c := range f.calls {
		if c == "DELETE /servers/3033" {
			di = i
		}
		if c == "DELETE /firewalls/2022" {
			ds = i
		}
	}
	if di < 0 || ds < di {
		t.Fatalf("order: %v", f.calls)
	}
	// a GET poll sits between them
	if f.calls[di+1] != "GET /servers/3033" {
		t.Fatalf("server deletion not polled: %v", f.calls[di:])
	}
}

func TestHetznerDestroyWithEmptyStateFindsOwnedResources(t *testing.T) {
	f := newFakeHetzner(t)
	p, st, _ := hetznerFixture(t, f, readyRunner())
	if err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(); err != nil { // crash after create, state never saved
		t.Fatal(err)
	}
	var out strings.Builder
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.haveServer || f.haveFW || f.haveKey {
		t.Fatalf("leaked: server=%v fw=%v key=%v\n%s", f.haveServer, f.haveFW, f.haveKey, out.String())
	}
}

func TestHetznerDestroyWithEmptyStateLeavesForeignResources(t *testing.T) {
	f := newFakeHetzner(t)
	f.haveServer, f.foreignServer = true, true
	f.haveFW, f.foreignFW = true, true
	f.haveKey = true // unlabelled key: someone else's
	p, st, _ := hetznerFixture(t, f, readyRunner())
	var out strings.Builder
	if err := p.Destroy(context.Background(), hetznerSpec(), st, &out); err != nil {
		t.Fatal(err)
	}
	if f.count("DELETE") != 0 || !strings.Contains(out.String(), "leaving it alone") {
		t.Fatalf("calls=%v out=%s", f.calls, out.String())
	}
}

func TestHetznerAdoptedKeyIsOwnedOnlyWithLabels(t *testing.T) {
	for _, owned := range []bool{false, true} {
		f := newFakeHetzner(t)
		f.haveKey, f.keyOwned = true, owned
		p, st, _ := hetznerFixture(t, f, readyRunner())
		if err := p.Create(context.Background(), hetznerSpec(), st, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
		if s, _ := st.Load(); s.SSHKeyOwned != owned {
			t.Errorf("labelled=%v: SSHKeyOwned=%v", owned, s.SSHKeyOwned)
		}
	}
}

func TestOptionsRejectNonIntegerPorts(t *testing.T) {
	for _, v := range []any{"22", 22.5, []any{22}, true} {
		spec := sshSpec()
		spec.Options["port"] = v
		if _, err := newSSH(newFakeRunner()).Plan(context.Background(), spec, NewStore(t.TempDir())); err == nil {
			t.Errorf("ssh port %#v accepted", v)
		}
		hs := hetznerSpec()
		hs.Options["port"] = v
		h := &Hetzner{deps: testDeps(nil)}
		if _, err := h.Plan(context.Background(), hs, NewStore(t.TempDir())); err == nil {
			t.Errorf("hetzner port %#v accepted", v)
		}
	}
	spec := sshSpec()
	spec.Options["swap_mb"] = "lots"
	if _, err := BootstrapOptionsFrom(spec); err == nil {
		t.Error("swap_mb string accepted")
	}
	spec = sshSpec()
	spec.Options["port"] = 2222.0 // yaml/json float with no fraction is fine
	if _, err := BootstrapOptionsFrom(spec); err != nil {
		t.Error(err)
	}
}

func TestOnlyLeadingTildeSlashIsExpanded(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, in := range []string{"~bob/x", "$HOME/x", "${HOME}/x"} {
		if expandHome(in) != in {
			t.Errorf("%q must not be expanded", in)
		}
	}
}

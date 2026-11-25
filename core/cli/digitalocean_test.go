package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/provider"
)

const (
	doCLIToken = "dop_v1_0123456789abcdefABCDEF"
	cfCLIToken = "cf_token_0123456789abcdefXYZ"
	cliPubKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ2o8e4mJw4mxk8t9S8YxqY0k3q8bX9r1Zk5c2n7t4Qd x"
)

const doCompanyYAML = `schema: 1
name: Acme Co
domain: acme.example
provider:
  type: digitalocean
  options:
    ssh_public_key_file: /keys/id.pub
dns:
  provider: cloudflare
`

// cliDO is a very small stateful DigitalOcean fake: one key, one firewall, one droplet.
type cliDO struct {
	mu                  sync.Mutex
	calls               []string
	key, fw, drop, gone bool
}

func (f *cliDO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, k)
	w.Header().Set("Content-Type", "application/json")
	fp, _ := provider.SSHFingerprint(cliPubKey)
	tags := []string{"managed-by:can", "company:acme-co"}
	droplet := map[string]any{"id": 8080, "name": "can-acme-co", "status": "active", "tags": tags,
		"networks": map[string]any{"v4": []any{map[string]any{"ip_address": "203.0.113.7", "type": "public"}}}}
	switch {
	case k == "GET /account/keys/"+fp:
		if !f.key {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"id":"not_found","message":"nope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ssh_key":{"id":5011,"name":"can-acme-co"}}`))
	case k == "GET /account/keys/5011":
		_ = json.NewEncoder(w).Encode(map[string]any{"ssh_key": map[string]any{"id": 5011, "name": "can-acme-co", "fingerprint": fp}})
	case k == "POST /account/keys":
		f.key = true
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ssh_key":{"id":5011,"name":"can-acme-co"}}`))
	case strings.HasPrefix(k, "DELETE /account/keys/"):
		f.key = false
		w.WriteHeader(204)
	case k == "GET /firewalls":
		fws := []any{}
		if f.fw {
			fws = append(fws, map[string]any{"id": "fw1", "name": "can-acme-co", "tags": tags})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"firewalls": fws})
	case k == "POST /firewalls":
		f.fw = true
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"firewall":{"id":"fw1","name":"can-acme-co"}}`))
	case k == "GET /firewalls/fw1":
		_ = json.NewEncoder(w).Encode(map[string]any{"firewall": map[string]any{"id": "fw1", "name": "can-acme-co", "tags": tags}})
	case k == "DELETE /firewalls/fw1":
		f.fw = false
		w.WriteHeader(204)
	case k == "GET /droplets":
		ds := []any{}
		if f.drop && !f.gone {
			ds = append(ds, droplet)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"droplets": ds})
	case k == "POST /droplets":
		f.drop = true
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"droplet": droplet, "links": map[string]any{"actions": []any{map[string]any{"id": 1, "rel": "create"}}}})
	case k == "GET /actions/1":
		_, _ = w.Write([]byte(`{"action":{"id":1,"status":"completed"}}`))
	case k == "GET /droplets/8080":
		if !f.drop || f.gone {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"id":"not_found","message":"nope"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"droplet": droplet})
	case k == "DELETE /droplets/8080":
		f.gone = true
		w.WriteHeader(204)
	default:
		w.WriteHeader(500)
	}
}

// cliCF is a tiny Cloudflare fake with a single zone and a record list.
type cliCF struct {
	mu      sync.Mutex
	calls   []string
	records []map[string]any
	zone    bool
}

func (f *cliCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, k)
	w.Header().Set("Content-Type", "application/json")
	ok := func(res any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": res, "result_info": map[string]any{"page": 1, "total_pages": 1}})
	}
	switch {
	case k == "GET /zones":
		if f.zone && r.URL.Query().Get("name") == "acme.example" {
			ok([]any{map[string]any{"id": "z1", "name": "acme.example"}})
			return
		}
		ok([]any{})
	case k == "GET /zones/z1/dns_records":
		name := r.URL.Query().Get("name")
		out := []any{}
		for _, rec := range f.records {
			if rec["name"] == name {
				out = append(out, rec)
			}
		}
		ok(out)
	case k == "POST /zones/z1/dns_records":
		var rec map[string]any
		_ = json.NewDecoder(r.Body).Decode(&rec)
		rec["id"] = "r" + string(rune('a'+len(f.records)))
		f.records = append(f.records, rec)
		ok(rec)
	case strings.HasPrefix(k, "GET /zones/z1/dns_records/"):
		for _, rec := range f.records {
			if "/zones/z1/dns_records/"+rec["id"].(string) == r.URL.Path {
				ok(rec)
				return
			}
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"gone"}]}`))
	case strings.HasPrefix(k, "DELETE /zones/z1/dns_records/"):
		for i, rec := range f.records {
			if "/zones/z1/dns_records/"+rec["id"].(string) == r.URL.Path {
				f.records = append(f.records[:i], f.records[i+1:]...)
				break
			}
		}
		ok(map[string]any{})
	default:
		w.WriteHeader(500)
	}
}

func doLifecycle(t *testing.T, do *cliDO, cf *cliCF) {
	t.Helper()
	dosrv := httptest.NewServer(do)
	cfsrv := httptest.NewServer(cf)
	t.Cleanup(dosrv.Close)
	t.Cleanup(cfsrv.Close)
	r := newRunner()
	useLifecycle(t, func() provider.Deps {
		return provider.Deps{
			DOBase: dosrv.URL, CFBase: cfsrv.URL,
			Runner: func(provider.Target) provider.Runner { return r },
			Getenv: func(k string) string {
				return map[string]string{"DIGITALOCEAN_TOKEN": doCLIToken, "CLOUDFLARE_API_TOKEN": cfCLIToken}[k]
			},
			ReadFile: func(p string) ([]byte, error) {
				if p == "/keys/id.pub" {
					return []byte(cliPubKey + "\n"), nil
				}
				return nil, os.ErrNotExist
			},
			Sleep: func(context.Context, time.Duration) {},
		}
	}, goodResolver())
}

func TestDigitalOceanUpPlanTouchesNothing(t *testing.T) {
	dir := writeCompany(t, doCompanyYAML)
	do, cf := &cliDO{}, &cliCF{zone: true}
	doLifecycle(t, do, cf)
	code, out, errs := run(t, "up", dir, "--plan", "--provision-only", "--modules-dir", "../../modules")
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	for _, want := range []string{"upload SSH public key", "cloud firewall", "droplet \"can-acme-co\" (s-1vcpu-2gb", "managed-by:can", "cloudflare: upsert A acme.example", "proxied=false"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
	if len(do.calls)+len(cf.calls) != 0 {
		t.Fatalf("--plan called an API: %v %v", do.calls, cf.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, ".can")); !os.IsNotExist(err) {
		t.Fatal("--plan wrote .can/")
	}
	code, out, _ = run(t, "up", dir, "--plan", "--output", "json", "--provision-only", "--modules-dir", "../../modules")
	var res upResult
	if code != ExitOK || json.Unmarshal([]byte(out), &res) != nil || res.Provider != "digitalocean" || !res.Planned || len(res.Actions) < 7 {
		t.Fatalf("json plan: code=%d %q", code, out)
	}
}

func TestDigitalOceanUpDownEndToEnd(t *testing.T) {
	dir := writeCompany(t, doCompanyYAML)
	do, cf := &cliDO{}, &cliCF{zone: true}
	doLifecycle(t, do, cf)
	code, out, errs := run(t, "up", dir, "--provision-only", "--modules-dir", "../../modules")
	if code != ExitOK {
		t.Fatalf("up: code=%d out=%q err=%q", code, out, errs)
	}
	if len(cf.records) != 3 || !strings.Contains(out, "203.0.113.7") {
		t.Fatalf("records %v out %q", cf.records, out)
	}
	for _, s := range []string{out, errs} {
		if strings.Contains(s, doCLIToken) || strings.Contains(s, cfCLIToken) {
			t.Fatal("token leaked")
		}
	}
	if code, out, errs = run(t, "status", dir, "--modules-dir", "../../modules"); code != ExitOK || !strings.Contains(out, "digitalocean droplet") {
		t.Fatalf("status: code=%d out=%q err=%q", code, out, errs)
	}
	// down: DNS records first, then droplet, firewall, key
	if code, out, errs = run(t, "down", dir, "--yes", "--modules-dir", "../../modules"); code != ExitOK {
		t.Fatalf("down: code=%d out=%q err=%q", code, out, errs)
	}
	if len(cf.records) != 0 || !do.gone || do.fw || do.key {
		t.Fatalf("left: records=%v do=%+v", cf.records, do)
	}
	var firstDel, dnsDel int
	for i, c := range append(append([]string{}, cf.calls...), do.calls...) {
		if strings.HasPrefix(c, "DELETE /zones") && dnsDel == 0 {
			dnsDel = i + 1
		}
		if strings.HasPrefix(c, "DELETE /droplets") && firstDel == 0 {
			firstDel = i + 1
		}
	}
	if dnsDel == 0 || firstDel == 0 {
		t.Fatalf("deletes missing: %v %v", cf.calls, do.calls)
	}
}

func TestCloudflareProblemsStopBeforeAnyServerIsCreated(t *testing.T) {
	dir := writeCompany(t, doCompanyYAML)
	do, cf := &cliDO{}, &cliCF{zone: false} // zone missing
	doLifecycle(t, do, cf)
	code, _, errs := run(t, "up", dir, "--provision-only", "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, "no Cloudflare zone") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	for _, c := range do.calls {
		if strings.HasPrefix(c, "POST") {
			t.Fatalf("created %s despite the DNS problem", c)
		}
	}
	// --dns manual overrides company.yaml and needs no Cloudflare at all
	if code, _, errs = run(t, "up", dir, "--dns", "manual", "--provision-only", "--modules-dir", "../../modules"); code != ExitOK {
		t.Fatalf("manual override: code=%d err=%q", code, errs)
	}
}

func TestDigitalOceanTokenNeverReachesOutput(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"id":"forbidden","message":"token ` + doCLIToken + ` has no write access"}`))
	}))
	defer api.Close()
	dir := writeCompany(t, "schema: 1\nname: Acme Co\ndomain: acme.example\nprovider:\n  type: digitalocean\n  options:\n    ssh_public_key_file: /keys/id.pub\n")
	useLifecycle(t, func() provider.Deps {
		return provider.Deps{
			DOBase: api.URL,
			Getenv: func(k string) string { return map[string]string{"DIGITALOCEAN_TOKEN": doCLIToken}[k] },
			ReadFile: func(string) ([]byte, error) {
				return []byte(cliPubKey), nil
			},
			Sleep: func(context.Context, time.Duration) {},
		}
	}, goodResolver())
	for _, mode := range [][]string{{}, {"--output", "json"}} {
		code, out, errs := run(t, append([]string{"up", dir, "--provision-only", "--modules-dir", "../../modules"}, mode...)...)
		if code != ExitFailure || strings.Contains(out+errs, doCLIToken) || !strings.Contains(errs, provider.Redacted) {
			t.Fatalf("code=%d (%v)\nstdout=%q\nstderr=%q", code, mode, out, errs)
		}
	}
}

func TestDownPlanMentionsDigitalOceanAndCloudflare(t *testing.T) {
	dir := writeCompany(t, doCompanyYAML)
	doLifecycle(t, &cliDO{}, &cliCF{})
	code, out, _ := run(t, "down", dir, "--plan", "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "DELETES the droplet") || !strings.Contains(out, "Cloudflare DNS records") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestValidateCatchesDigitalOceanAndDNSMistakes(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider:\n  type: digitalocean\n  options: { port: 2222, token_secret_ref: dop_v1_literal }\ndns: { provider: cloudflare, zone: b.example }\n")
	code, out, _ := run(t, "validate", dir, "--modules-dir", "../../modules")
	if code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"/provider/options", "/provider/options/token_secret_ref", "/dns/zone"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dop_v1_literal") {
		t.Error("validate echoed the literal token")
	}
}

func TestDownRefusesWhenDNSBlockWasRemovedButRecordsRemain(t *testing.T) {
	dir := writeCompany(t, doCompanyYAML)
	do, cf := &cliDO{}, &cliCF{zone: true}
	doLifecycle(t, do, cf)
	if code, out, errs := run(t, "up", dir, "--provision-only", "--modules-dir", "../../modules"); code != ExitOK {
		t.Fatalf("up: %d %q %q", code, out, errs)
	}
	// the operator deletes the dns block, then runs down
	noDNS := strings.Replace(doCompanyYAML, "dns:\n  provider: cloudflare\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(noDNS), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "down", dir, "--yes", "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, "dns_records") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if do.gone || !do.fw || len(cf.records) != 3 {
		t.Fatalf("something was destroyed: %+v %d", do, len(cf.records))
	}
	if st, _ := provider.NewStore(dir).Load(); len(st.DNSRecords) != 3 {
		t.Fatalf("state was wiped: %+v", st.DNSRecords)
	}
}

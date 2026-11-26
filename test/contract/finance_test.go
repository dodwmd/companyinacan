package contract

// Epic E15a: the finance module. Offline: the manifest is validated with `can validate`'s code,
// event and capability schemas are exercised with fixtures and with events a real can-finance
// instance emits, and the OpenAPI document is checked against the router so they cannot drift.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/schemas"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

var financeEvents = map[string][]byte{
	"invoice.sent.v1":       events.InvoiceSentV1,
	"invoice.paid.v1":       events.InvoicePaidV1,
	"contract.signed.v1":    events.ContractSignedV1,
	"contract.expiring.v1":  events.ContractExpiringV1,
	"timesheet.approved.v1": events.TimesheetApprovedV1,
	"expense.approved.v1":   events.ExpenseApprovedV1,

	"journal.posted.v1":          events.JournalPostedV1,
	"bank.statement.imported.v1": events.BankStatementImportedV1,
}

var financeCaps = map[string][]byte{"invoices.v1": capabilities.InvoicesV1, "contracts.v1": capabilities.ContractsV1, "timesheets.v1": capabilities.TimesheetsV1}

func TestFinanceManifest(t *testing.T) {
	const rel = "modules/finance/module.yaml"
	for _, p := range validateModule(filepath.Join(root, rel)) {
		if p.Severity != problem.Warning || p.Rule != "semantic.module-status" {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	m := readYAML(t, rel)
	if m["status"] != "experimental" {
		t.Errorf("status = %v: not verified on a cluster, so it must say experimental", m["status"])
	}
	if asMap(m["provider"])["type"] != "inhouse" {
		t.Error("finance is in-house")
	}
	var caps []string
	for _, p := range asList(m["provides"]) {
		pm := asMap(p)
		caps = append(caps, asStr(pm["capability"])+"="+asStr(pm["schema"]))
		if _, ok := financeCaps[asStr(pm["schema"])]; !ok && asStr(pm["schema"]) != "summary.v1" { // checked in summary_test.go
			t.Errorf("provided schema %v is not embedded", pm["schema"])
		}
	}
	sort.Strings(caps)
	if strings.Join(caps, " ") != "contracts=contracts.v1 invoices=invoices.v1 summary=summary.v1 timesheets=timesheets.v1" {
		t.Errorf("provides = %v", caps)
	}
	perms := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		perms[asStr(p)] = true
	}
	for _, p := range []string{"finance.read", "finance.write", "finance.approve", "finance.admin"} {
		if !perms[p] {
			t.Errorf("permission %s missing", p)
		}
	}
	for role, ps := range asMap(m["roles"]) {
		for _, p := range asList(ps) {
			if !perms[asStr(p)] {
				t.Errorf("role %s grants undeclared permission %v", role, p)
			}
		}
	}
	classes := map[string]string{}
	for _, dc := range asList(m["data_classes"]) {
		d := asMap(dc)
		classes[asStr(d["class"])] = asStr(d["retention"])
	}
	if classes["finance-invoice"] != "7y" || classes["finance-party-contact"] != "legal" || classes["finance-audit"] == "" {
		t.Errorf("data classes %v: invoices keep 7y, party contact data is under legal retention", classes)
	}
	var approvals []string
	for _, a := range asList(m["approvals"]) {
		approvals = append(approvals, asStr(asMap(a)["type"]))
	}
	if strings.Join(approvals, ",") != "invoice,timesheet,expense,purchase-order" {
		t.Errorf("approvals %v", approvals)
	}
	pubs := asList(m["publishes"])
	if len(pubs) != len(financeEvents) {
		t.Fatalf("publishes %d events, want %d", len(pubs), len(financeEvents))
	}
	for _, p := range pubs {
		pm := asMap(p)
		name := asStr(pm["event"])
		want, ok := financeEvents[name]
		if !ok {
			t.Errorf("publishes unknown event %s", name)
			continue
		}
		got, err := os.ReadFile(filepath.Join(root, "modules", "finance", strings.TrimPrefix(asStr(pm["schema"]), "./")))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("modules/finance/%s differs from schemas/events/%s.json", pm["schema"], name)
		}
	}
	res := asMap(m["resources"])
	if res["ram_mb"] != 28 || res["ram_limit_mb"] != 64 {
		t.Errorf("resources %v: keep in step with the measured footprint (specs/014-finance-basics/research.md R8)", res)
	}
}

func TestFinanceChartAndImage(t *testing.T) {
	v := readYAML(t, "modules/finance/chart/values.yaml")
	if asMap(v["config"])["trust_proxy"] != false {
		t.Error("trust_proxy must default to false")
	}
	if _, ok := v["env"]; ok {
		t.Error("secrets are not chart values: can-core injects the can-module Secret")
	}
	sc := asMap(v["securityContext"])
	if sc["runAsNonRoot"] != true || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext %v", sc)
	}
	if !chartDigestOK(asStr(asMap(v["image"])["digest"])) {
		t.Error("image digest must be a sha256 or the REPLACE_WITH_ placeholder (`make release-check` rejects it), never a guess")
	}
	if got := asList(asMap(v["ingress"])["pathPrefixes"]); len(got) != 1 || got[0] != "/public/invoices/" || len(asList(asMap(v["ingress"])["paths"])) != 0 {
		t.Errorf("only the signed invoice link may be public, got %v", got)
	}
	df, err := os.ReadFile(filepath.Join(root, "deploy", "finance", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REPLACE_WITH_GOLANG_DIGEST", "REPLACE_WITH_DISTROLESS_STATIC_DIGEST", "USER 65532:65532", "CGO_ENABLED=0"} {
		if !strings.Contains(string(df), want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(root, "modules", "finance", "settings.schema.json"))
	compileSchema(t, "settings.json", raw, "")
}

func TestNamesKnowFinanceCapabilities(t *testing.T) {
	var names struct {
		Capabilities []struct {
			Name string `yaml:"name"`
		} `yaml:"capabilities"`
	}
	if err := yaml.Unmarshal(schemas.Names, &names); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, c := range names.Capabilities {
		have[c.Name] = true
	}
	for _, n := range []string{"invoices", "contracts", "timesheets"} {
		if !have[n] {
			t.Errorf("schemas/names.yaml lacks %s", n)
		}
	}
}

func TestFinanceEventFixtures(t *testing.T) {
	for name, raw := range financeEvents {
		t.Run(name, func(t *testing.T) {
			s := compileSchema(t, name+".json", raw, "")
			for kind, wantValid := range map[string]bool{"valid": true, "invalid": false} {
				files, _ := filepath.Glob(filepath.Join(root, "testdata", "events", name, kind+"-*.json"))
				if len(files) < 2 {
					t.Fatalf("need %s fixtures for %s, have %d", kind, name, len(files))
				}
				for _, f := range files {
					b, _ := os.ReadFile(f)
					err := validateJSON(t, s, b)
					if wantValid && err != nil {
						t.Errorf("%s should be valid: %v", filepath.Base(f), err)
					}
					if !wantValid && err == nil {
						t.Errorf("%s should be rejected", filepath.Base(f))
					}
				}
			}
		})
	}
}

// Fixture names are <valid|invalid>-<Definition>-<case>.json, checked against #/$defs/<Definition>.
func TestFinanceCapabilityFixtures(t *testing.T) {
	for name, raw := range financeCaps {
		t.Run(name, func(t *testing.T) {
			compileSchema(t, name+".json", raw, "")
			var top map[string]any
			if err := json.Unmarshal(raw, &top); err != nil {
				t.Fatal(err)
			}
			verbs := asList(top["x-verbs"])
			if len(verbs) == 0 || asStr(top["x-capability"]) == "" {
				t.Fatal("x-capability and x-verbs are required")
			}
			defs := asMap(top["$defs"])
			for _, v := range verbs {
				vm := asMap(v)
				for _, k := range []string{"request", "response"} {
					if d := asStr(vm[k]); d != "" && defs[d] == nil {
						t.Errorf("verb %v names unknown definition %q", vm["verb"], d)
					}
				}
				if m := asStr(vm["method"]); m != "GET" && vm["write"] != true {
					t.Errorf("verb %v: non-GET verbs must be flagged write: true", vm["verb"])
				}
			}
			files, _ := filepath.Glob(filepath.Join(root, "testdata", "capabilities", name, "*.json"))
			seenValid := map[string]bool{}
			invalid := 0
			for _, f := range files {
				parts := strings.SplitN(strings.TrimSuffix(filepath.Base(f), ".json"), "-", 3)
				if len(parts) != 3 {
					t.Fatalf("fixture %s must be <valid|invalid>-<Definition>-<case>.json", f)
				}
				s := compileSchema(t, name+".json", raw, "#/$defs/"+parts[1])
				b, _ := os.ReadFile(f)
				err := validateJSON(t, s, b)
				if parts[0] == "valid" {
					seenValid[parts[1]] = true
					if err != nil {
						t.Errorf("%s should be valid: %v", filepath.Base(f), err)
					}
				} else {
					invalid++
					if err == nil {
						t.Errorf("%s should be rejected", filepath.Base(f))
					}
				}
			}
			if invalid < 8 {
				t.Errorf("only %d invalid fixtures", invalid)
			}
			for _, v := range verbs {
				for _, k := range []string{"request", "response"} {
					if d := asStr(asMap(v)[k]); d != "" && !seenValid[d] {
						t.Errorf("definition %s has no valid fixture", d)
					}
				}
			}
		})
	}
}

// What a running service really sends must satisfy the published schemas, and the signature must verify.
func TestFinanceLiveEventsMatchSchemas(t *testing.T) {
	type call struct {
		body []byte
		hdr  http.Header
	}
	got := make(chan call, 32)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- call{b, r.Header.Clone()}
	}))
	defer recv.Close()

	cfg := finance.DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "f.db")
	cfg.Webhook.URL = recv.URL
	cfg.ESignProvider = "fake"
	const (
		secret = "whsec-0123456789abcdef"
		tok    = "contract-token-0123456789abcdefXYZ"
		link   = "contract-link-secret-0123456789abcdefXYZ"
		prov   = "contract-provider-secret-01234"
	)
	app, err := finance.NewApp(cfg, finance.Secrets{AdminToken: tok, LinkSecret: link, WebhookSecret: secret, ProviderSecret: prov, SealKey: strings.Repeat("0123456789abcdef", 4)}, finance.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	}()
	base := "http://" + ln.Addr().String()
	do := func(token, method, path string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, b)
		}
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return out
	}
	mint := func(subject string, scopes []string, own bool) string {
		return do(tok, "POST", "/v1/tokens", map[string]any{"kind": "service", "subject": subject, "scopes": scopes, "own_only": own})["token"].(string)
	}
	maker, boss := mint("maker", []string{"finance.write"}, false), mint("boss", []string{"finance.approve"}, false)
	alice := mint("person_17", []string{"finance.write"}, true)
	do(tok, "POST", "/v1/ledger/accounts/template", map[string]any{"apply_mappings": true}) // E15c: every approval below posts a journal entry
	party := int(do(maker, "POST", "/v1/parties", map[string]any{"name": "Acme Fake Ltd", "kind": "customer"})["id"].(float64))
	inv := int(do(maker, "POST", "/v1/invoices", map[string]any{"party_id": party, "lines": []map[string]any{{"description": "x", "quantity_milli": 1000, "unit_price_minor": 5000}}})["id"].(float64))
	n := "/v1/invoices/" + strconv.Itoa(inv)
	do(boss, "POST", n+"/approve", nil)
	do(maker, "POST", n+"/send", nil)
	do(maker, "POST", n+"/payments", map[string]any{"amount_minor": 5000})
	// E15c: a bank statement import (counts only in the event; the account number is sealed).
	ba := int(do(tok, "POST", "/v1/bank/accounts", map[string]any{"name": "Main", "currency": "USD", "number": "DE89370400440532013000", "ledger_account_code": "1000"})["id"].(float64))
	stmt, _ := http.NewRequest("POST", base+"/v1/bank/accounts/"+strconv.Itoa(ba)+"/statements?format=csv&delimiter=comma&date_col=1&amount_col=2&id_col=3", strings.NewReader("date,amount,id\n2026-01-05,50.00,T1\n"))
	stmt.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := http.DefaultClient.Do(stmt); err != nil || resp.StatusCode != 201 {
		t.Fatalf("statement import: %v %v", err, resp)
	}

	body := "Agreement for {{customer}}."
	c := do(maker, "POST", "/v1/contracts", map[string]any{"party_id": party, "title": "T", "body": body, "variables": map[string]string{"customer": "Acme"}, "renewal_date": "2026-01-10", "reminder_days": 5})
	cid := strconv.Itoa(int(c["id"].(float64)))
	do(maker, "POST", "/v1/contracts/"+cid+"/send", map[string]any{"manual": true})
	do(maker, "POST", "/v1/contracts/"+cid+"/signature", map[string]any{"sha256": c["sha256"]})
	do(tok, "POST", "/v1/contracts/sweep", map[string]any{"as_of": "2026-01-06"})

	sh := do(alice, "POST", "/v1/timesheets", map[string]any{"week_start": "2026-11-23"})
	sid := strconv.Itoa(int(sh["id"].(float64)))
	do(alice, "PUT", "/v1/timesheets/"+sid+"/entries", map[string]any{"entries": []map[string]any{{"day": "2026-11-23", "project": "ACME", "minutes": 60, "billable": true}}})
	do(alice, "POST", "/v1/timesheets/"+sid+"/submit", nil)
	do(boss, "POST", "/v1/timesheets/"+sid+"/approve", nil)
	ex := do(alice, "POST", "/v1/expenses", map[string]any{"description": "Train", "amount_minor": 4500, "currency": "USD", "category": "travel", "incurred_on": "2026-11-20"})
	eid := strconv.Itoa(int(ex["id"].(float64)))
	do(alice, "POST", "/v1/expenses/"+eid+"/submit", nil)
	do(boss, "POST", "/v1/expenses/"+eid+"/approve", nil)

	seen := map[string]int{}
	for len(seen) < len(financeEvents) {
		select {
		case c := <-got:
			var ev struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(c.body, &ev); err != nil {
				t.Fatal(err)
			}
			schema, ok := financeEvents[ev.Type]
			if !ok {
				t.Fatalf("unexpected event type %s", ev.Type)
			}
			if err := validateJSON(t, compileSchema(t, ev.Type+".json", schema, ""), c.body); err != nil {
				t.Errorf("%s does not match its schema: %v\n%s", ev.Type, err, c.body)
			}
			if strings.Contains(string(c.body), "Acme") {
				t.Errorf("%s carries a name: %s", ev.Type, c.body)
			}
			var sec int64
			for _, ch := range c.hdr.Get("X-Can-Timestamp") {
				sec = sec*10 + int64(ch-'0')
			}
			if c.hdr.Get("X-Can-Signature") != finance.Sign([]byte(secret), sec, c.body) {
				t.Errorf("bad signature on %s", ev.Type)
			}
			seen[ev.Type]++
		case <-time.After(15 * time.Second):
			t.Fatalf("timed out waiting for events, saw %v", seen)
		}
	}
}

var financeRouteRE = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) (/[^"]*)"`)

func financeOps(t *testing.T) (ops map[string]bool, doc map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "specs", "014-finance-basics", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asStr(doc["openapi"]), "3.1") {
		t.Fatalf("openapi = %v, want 3.1.x", doc["openapi"])
	}
	ops = map[string]bool{}
	for path, item := range asMap(doc["paths"]) {
		for method := range asMap(item) {
			switch method {
			case "get", "post", "put", "patch", "delete":
				ops[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	return ops, doc
}

func TestFinanceOpenAPIMatchesRouter(t *testing.T) {
	ops, _ := financeOps(t)
	src, err := os.ReadFile(filepath.Join(root, "core", "finance", "api.go"))
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]bool{}
	for _, m := range financeRouteRE.FindAllStringSubmatch(string(src), -1) {
		routes[m[1]+" "+m[2]] = true
	}
	if len(routes) < 60 {
		t.Fatalf("found only %d routes in api.go; did the registration style change?", len(routes))
	}
	for r := range routes {
		if !ops[r] {
			t.Errorf("route %q is not in openapi.yaml", r)
		}
	}
	for o := range ops {
		if !routes[o] {
			t.Errorf("openapi.yaml documents %q but the router has no such route", o)
		}
	}
}

func TestFinanceOpenAPIRefsAndCapabilityVerbs(t *testing.T) {
	ops, doc := financeOps(t)
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if strings.HasPrefix(ref, "#/") {
					var cur any = doc
					for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
						cur = asMap(cur)[part]
					}
					if cur == nil {
						t.Errorf("unresolved $ref %s", ref)
					}
				} else if file, frag, _ := strings.Cut(ref, "#/$defs/"); true {
					raw, err := os.ReadFile(filepath.Join(root, "specs", "014-finance-basics", "contracts", file))
					if err != nil {
						t.Errorf("$ref %s: %v", ref, err)
					} else {
						var top map[string]any
						_ = json.Unmarshal(raw, &top)
						if asMap(top["$defs"])[frag] == nil {
							t.Errorf("$ref %s: no such definition", ref)
						}
					}
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
	for name, raw := range financeCaps {
		var top map[string]any
		if err := json.Unmarshal(raw, &top); err != nil {
			t.Fatal(err)
		}
		for _, v := range asList(top["x-verbs"]) {
			vm := asMap(v)
			if key := asStr(vm["method"]) + " " + asStr(vm["path"]); !ops[key] {
				t.Errorf("%s verb %v (%s) is not in openapi.yaml", name, vm["verb"], key)
			}
		}
	}
}

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Cloudflare shapes are hand-written from https://developers.cloudflare.com/api/
// and are UNVERIFIED against a live account.

type cfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

type fakeCF struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []string
	bodies  map[string][]byte
	zones   map[string]string // name -> id
	records []*cfRecord
	next    int
	hook    func(r *http.Request) (int, string, http.Header)
	auth    []string
}

func newFakeCF(t *testing.T) *fakeCF {
	return &fakeCF{t: t, bodies: map[string][]byte{}, zones: map[string]string{"acme.example": "zone1"}, next: 100}
}

func (f *fakeCF) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeCF) ok(w http.ResponseWriter, result any, info map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result}
	if info != nil {
		out["result_info"] = info
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeCF) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 9109, "message": msg}}, "messages": []any{}, "result": nil})
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	b, _ := io.ReadAll(r.Body)
	f.bodies[key] = b
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		f.fail(w, 403, "Authentication error")
		return
	}
	if f.hook != nil {
		if code, body, hdr := f.hook(r); code != 0 {
			for k, v := range hdr {
				w.Header()[k] = v
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
			return
		}
	}
	switch {
	case key == "GET /zones":
		name := r.URL.Query().Get("name")
		res := []any{}
		if id, ok := f.zones[name]; ok {
			res = append(res, map[string]any{"id": id, "name": name, "status": "active"})
		}
		f.ok(w, res, map[string]any{"page": 1, "total_pages": 1})
	case key == "GET /zones/zone1/dns_records":
		name := r.URL.Query().Get("name")
		var match []any
		for _, rec := range f.records {
			if name == "" || rec.Name == name {
				match = append(match, rec)
			}
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		res := []any{}
		if page <= len(match) { // one record per page
			res = match[page-1 : page]
		}
		tp := len(match)
		if tp < 1 {
			tp = 1
		}
		f.ok(w, res, map[string]any{"page": page, "total_pages": tp})
	case key == "POST /zones/zone1/dns_records":
		var rec cfRecord
		_ = json.Unmarshal(b, &rec)
		f.next++
		rec.ID = "rec" + strconv.Itoa(f.next)
		f.records = append(f.records, &rec)
		f.ok(w, rec, nil)
	case strings.HasPrefix(key, "PUT /zones/zone1/dns_records/"):
		id := strings.TrimPrefix(r.URL.Path, "/zones/zone1/dns_records/")
		for _, rec := range f.records {
			if rec.ID == id {
				var in cfRecord
				_ = json.Unmarshal(b, &in)
				rec.Content, rec.TTL, rec.Proxied, rec.Comment = in.Content, in.TTL, in.Proxied, in.Comment
				f.ok(w, rec, nil)
				return
			}
		}
		f.fail(w, 404, "Record does not exist")
	case strings.HasPrefix(key, "DELETE /zones/zone1/dns_records/"):
		id := strings.TrimPrefix(r.URL.Path, "/zones/zone1/dns_records/")
		for i, rec := range f.records {
			if rec.ID == id {
				f.records = append(f.records[:i], f.records[i+1:]...)
				f.ok(w, map[string]any{"id": id}, nil)
				return
			}
		}
		f.fail(w, 404, "Record does not exist")
	case strings.HasPrefix(key, "GET /zones/zone1/dns_records/"):
		id := strings.TrimPrefix(r.URL.Path, "/zones/zone1/dns_records/")
		for _, rec := range f.records {
			if rec.ID == id {
				f.ok(w, rec, nil)
				return
			}
		}
		f.fail(w, 404, "Record does not exist")
	default:
		f.t.Errorf("unexpected request %s", key)
		f.fail(w, 500, "unexpected")
	}
}

type cfFx struct {
	f      *fakeCF
	p      *Cloudflare
	st     *Store
	sleeps []time.Duration
}

func cfFixture(t *testing.T, f *fakeCF, cfg DNSConfig) *cfFx {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	x := &cfFx{f: f, st: NewStore(t.TempDir())}
	d := testDeps(nil)
	d.CFBase = srv.URL
	d.Getenv = func(k string) string {
		if k == "CLOUDFLARE_API_TOKEN" {
			return testToken
		}
		return ""
	}
	d.Sleep = func(_ context.Context, dur time.Duration) { x.sleeps = append(x.sleeps, dur) }
	p, err := NewDNSWith("cloudflare", cfg, d, x.st)
	if err != nil {
		t.Fatal(err)
	}
	x.p = p.(*Cloudflare)
	return x
}

func cfRecs() []Record {
	r, _ := Records("acme.example", "203.0.113.7", []string{"admin", "help"})
	return r
}

func TestCloudflareCreatesRecordsAndRemembersThem(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	var out strings.Builder
	if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(f.records) != 3 {
		t.Fatalf("records: %+v", f.records)
	}
	for _, r := range f.records {
		if r.Type != "A" || r.Content != "203.0.113.7" || r.TTL != 300 || r.Proxied || r.Comment != "managed-by=can" {
			t.Errorf("record %+v", r)
		}
	}
	state, _ := x.st.Load()
	if len(state.DNSRecords) != 3 || state.DNSRecords[0].Plugin != "cloudflare" || state.DNSRecords[0].Zone != "zone1" || state.DNSRecords[0].Adopted {
		t.Fatalf("state: %+v", state.DNSRecords)
	}
	if strings.Contains(out.String(), testToken) {
		t.Error("token printed")
	}
	for _, a := range f.auth {
		if a != "Bearer "+testToken {
			t.Errorf("auth %q", a)
		}
	}
}

func TestCloudflareIsIdempotentAndUpdatesOwnedRecords(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	ctx := context.Background()
	if err := x.p.Apply(ctx, "acme.example", cfRecs(), io.Discard); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := x.p.Apply(ctx, "acme.example", cfRecs(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.count("POST") != 0 || f.count("PUT") != 0 {
		t.Fatalf("second apply wrote: %v", f.calls)
	}
	// the server moved: owned records are updated in place
	moved, _ := Records("acme.example", "203.0.113.99", []string{"admin", "help"})
	if err := x.p.Apply(ctx, "acme.example", moved, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.count("PUT") != 3 || f.count("POST") != 0 || f.records[0].Content != "203.0.113.99" {
		t.Fatalf("calls %v records %+v", f.calls, f.records[0])
	}
	if state, _ := x.st.Load(); len(state.DNSRecords) != 3 {
		t.Fatalf("state grew: %+v", state.DNSRecords)
	}
}

func TestCloudflareZoneLookupWalksUpToParentDomain(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	recs, _ := Records("app.acme.example", "203.0.113.7", nil)
	if err := x.p.Apply(context.Background(), "app.acme.example", recs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 || f.records[0].Name != "app.acme.example" {
		t.Fatalf("%+v", f.records)
	}
	// an explicit zone is used as is
	f2 := newFakeCF(t)
	f2.zones = map[string]string{"acme.example": "zone1"}
	x2 := cfFixture(t, f2, DNSConfig{Provider: "cloudflare", Zone: "acme.example"})
	if err := x2.p.Apply(context.Background(), "app.acme.example", recs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f2.count("GET /zones?") != 0 && f2.count("GET /zones") != 1 {
		t.Fatalf("calls %v", f2.calls)
	}
}

func TestCloudflareZoneNotFound(t *testing.T) {
	f := newFakeCF(t)
	f.zones = map[string]string{}
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no Cloudflare zone") {
		t.Fatalf("got %v", err)
	}
	if f.count("POST") != 0 {
		t.Fatal("wrote records")
	}
}

func TestCloudflareNeverTouchesForeignRecords(t *testing.T) {
	foreign := func() *fakeCF {
		f := newFakeCF(t)
		f.records = []*cfRecord{{ID: "x1", Type: "A", Name: "admin.acme.example", Content: "192.0.2.1", TTL: 300, Comment: "mine"}}
		return f
	}
	t.Run("refused by default", func(t *testing.T) {
		f := foreign()
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
		err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not created by can") {
			t.Fatalf("got %v", err)
		}
		if f.count("PUT") != 0 || f.count("DELETE") != 0 || f.records[0].Content != "192.0.2.1" {
			t.Fatalf("touched: %v", f.calls)
		}
	})
	t.Run("explicit adopt of an unproxied record", func(t *testing.T) {
		f := foreign()
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare", AdoptExisting: true})
		if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err != nil {
			t.Fatal(err)
		}
		if f.records[0].Content != "203.0.113.7" || f.records[0].Comment != "mine" {
			t.Fatalf("adopted record: %+v", f.records[0])
		}
		state, _ := x.st.Load()
		var adopted int
		for _, r := range state.DNSRecords {
			if r.Adopted {
				adopted++
			}
		}
		if adopted != 1 {
			t.Fatalf("state: %+v", state.DNSRecords)
		}
		// destroy removes what can created and keeps the adopted record
		if err := x.p.Remove(context.Background(), "acme.example", io.Discard); err != nil {
			t.Fatal(err)
		}
		if len(f.records) != 1 || f.records[0].ID != "x1" {
			t.Fatalf("after remove: %+v", f.records)
		}
	})
	t.Run("adopt flag does not cover proxied records", func(t *testing.T) {
		f := foreign()
		f.records[0].Proxied = true
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare", AdoptExisting: true})
		if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err == nil {
			t.Fatal("adopted a proxied record")
		}
		if f.count("PUT") != 0 {
			t.Fatal("modified it")
		}
	})
	t.Run("cname at the same name blocks", func(t *testing.T) {
		f := newFakeCF(t)
		f.records = []*cfRecord{{ID: "c1", Type: "CNAME", Name: "help.acme.example", Content: "other.example", TTL: 300}}
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare", AdoptExisting: true})
		err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "CNAME") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCloudflareCrashBeforeStateWriteAdoptsOwnRecord(t *testing.T) {
	f := newFakeCF(t)
	f.records = []*cfRecord{{ID: "r9", Type: "A", Name: "acme.example", Content: "203.0.113.7", TTL: 300, Comment: "managed-by=can"}}
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.count("POST") != 2 {
		t.Fatalf("calls %v", f.calls)
	}
	state, _ := x.st.Load()
	found := false
	for _, r := range state.DNSRecords {
		found = found || (r.ID == "r9" && !r.Adopted)
	}
	if !found || len(state.DNSRecords) != 3 {
		t.Fatalf("state %+v", state.DNSRecords)
	}
}

func TestCloudflareRemoveOnlyDeletesWhatItCreated(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	ctx := context.Background()
	if err := x.p.Apply(ctx, "acme.example", cfRecs(), io.Discard); err != nil {
		t.Fatal(err)
	}
	f.records = append(f.records, &cfRecord{ID: "keep", Type: "TXT", Name: "acme.example", Content: "v=spf1", TTL: 300})
	// someone replaced the comment on one record: it is no longer ours
	f.records[1].Comment = "handmade"
	var out strings.Builder
	if err := x.p.Remove(ctx, "acme.example", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var left []string
	for _, r := range f.records {
		left = append(left, r.ID)
	}
	if len(f.records) != 2 || f.records[1].ID != "keep" || f.records[0].Comment != "handmade" {
		t.Fatalf("left %v", left)
	}
	if !strings.Contains(out.String(), "no longer carries") {
		t.Fatalf("out %q", out.String())
	}
	if state, _ := x.st.Load(); len(state.DNSRecords) != 0 {
		t.Fatalf("state: %+v", state.DNSRecords)
	}
	// already gone is fine; empty state is a no-op
	if err := x.p.Remove(ctx, "acme.example", io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflareRemoveToleratesMissingRecord(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err != nil {
		t.Fatal(err)
	}
	f.records = nil
	if err := x.p.Remove(context.Background(), "acme.example", io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestCloudflareProxiedAndTTL(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare", TTL: 600})
	recs := cfRecs()
	for i := range recs {
		recs[i].TTL = 600
	}
	if err := x.p.Apply(context.Background(), "acme.example", recs, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.records[0].TTL != 600 || f.records[0].Proxied {
		t.Fatalf("%+v", f.records[0])
	}
	f2 := newFakeCF(t)
	x2 := cfFixture(t, f2, DNSConfig{Provider: "cloudflare", Proxied: true})
	if err := x2.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if !f2.records[0].Proxied || f2.records[0].TTL != 1 {
		t.Fatalf("proxied records need automatic TTL: %+v", f2.records[0])
	}
	for _, ttl := range []int{-5, 30, 100000} {
		if _, err := NewDNSWith("cloudflare", DNSConfig{Provider: "cloudflare", TTL: ttl}, testDeps(nil), NewStore(t.TempDir())); err == nil {
			t.Errorf("ttl %d accepted", ttl)
		}
	}
}

func TestCloudflarePagination(t *testing.T) {
	f := newFakeCF(t)
	f.records = []*cfRecord{
		{ID: "a", Type: "A", Name: "admin.acme.example", Content: "1.1.1.1", TTL: 300, Comment: "other"},
		{ID: "b", Type: "AAAA", Name: "admin.acme.example", Content: "::1", TTL: 300, Comment: "managed-by=can"},
		{ID: "c", Type: "A", Name: "admin.acme.example", Content: "203.0.113.7", TTL: 300, Comment: "managed-by=can"},
	}
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	recs, _ := Records("acme.example", "203.0.113.7", []string{"admin"})
	// the owned A record is on page 3, behind a foreign A record and an owned AAAA:
	// every page is read, the owned A wins and nothing is written.
	if err := x.p.Apply(context.Background(), "acme.example", recs[1:], io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /zones/zone1/dns_records") < 3 || f.count("POST") != 0 || f.count("PUT") != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestCloudflareRateLimitsAndRetryRules(t *testing.T) {
	t.Run("429 honours Retry-After then succeeds", func(t *testing.T) {
		f := newFakeCF(t)
		n := 0
		f.hook = func(r *http.Request) (int, string, http.Header) {
			if r.Method == "POST" && n < 1 {
				n++
				h := http.Header{}
				h.Set("Retry-After", "9")
				return 429, `{"success":false,"errors":[{"code":971,"message":"rate limited"}]}`, h
			}
			return 0, "", nil
		}
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
		if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err != nil {
			t.Fatal(err)
		}
		var ok bool
		for _, s := range x.sleeps {
			ok = ok || s == 9*time.Second
		}
		if !ok {
			t.Fatalf("sleeps %v", x.sleeps)
		}
	})
	t.Run("500 on POST is not retried", func(t *testing.T) {
		f := newFakeCF(t)
		f.hook = func(r *http.Request) (int, string, http.Header) {
			if r.Method == "POST" {
				return 500, `{"success":false,"errors":[{"code":1,"message":"boom"}]}`, nil
			}
			return 0, "", nil
		}
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
		if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err == nil {
			t.Fatal("want error")
		}
		if f.count("POST") != 1 {
			t.Fatalf("POST retried: %v", f.calls)
		}
	})
	t.Run("500 on GET is retried", func(t *testing.T) {
		f := newFakeCF(t)
		n := 0
		f.hook = func(r *http.Request) (int, string, http.Header) {
			if r.Method == "GET" && r.URL.Path == "/zones" && n < 2 {
				n++
				return 502, `{"success":false,"errors":[]}`, nil
			}
			return 0, "", nil
		}
		x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
		if err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCloudflareErrorsAreRedacted(t *testing.T) {
	f := newFakeCF(t)
	f.hook = func(r *http.Request) (int, string, http.Header) {
		return 400, fmt.Sprintf(`{"success":false,"errors":[{"code":6003,"message":"Invalid request headers: Authorization: Bearer %s"}]}`, testToken), nil
	}
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	err := x.p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard)
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "cloudflare API") {
		t.Fatalf("got %v", err)
	}
}

func TestCloudflareTokenMustBeAReference(t *testing.T) {
	d := testDeps(nil)
	p, err := NewDNSWith("cloudflare", DNSConfig{Provider: "cloudflare", TokenRef: "abcdef0123456789literal"}, d, NewStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	err = p.Apply(context.Background(), "acme.example", cfRecs(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "env:NAME or file:PATH") || strings.Contains(err.Error(), "0123456789literal") {
		t.Fatalf("got %v", err)
	}
}

func TestCloudflarePlanDescribesWithoutCalling(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	lines := x.p.PlanRecords("acme.example", cfRecs())
	if len(lines) != 3 || !strings.Contains(lines[0], "acme.example") || !strings.Contains(strings.Join(lines, "\n"), "proxied=false") {
		t.Fatalf("%v", lines)
	}
	if len(f.calls) != 0 {
		t.Fatal("plan called the API")
	}
}

func TestNewDNSWith(t *testing.T) {
	if p, err := NewDNSWith("", DNSConfig{}, testDeps(nil), nil); err != nil || p.Name() != "manual" {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := NewDNSWith("provider-native", DNSConfig{}, testDeps(nil), nil); err == nil {
		t.Fatal("provider-native should not be available")
	}
	if _, err := NewDNSWith("route66", DNSConfig{}, testDeps(nil), nil); err == nil {
		t.Fatal("unknown")
	}
	if _, err := NewDNS("cloudflare"); err == nil {
		t.Fatal("NewDNS(cloudflare) has no settings to work with")
	}
}

func TestDNSConfigFromMap(t *testing.T) {
	c, err := DNSConfigFrom(map[string]any{"provider": "cloudflare", "zone": "acme.example", "ttl": 120, "proxied": false, "adopt_existing": true, "token_secret_ref": "env:CF"})
	if err != nil || c.Zone != "acme.example" || c.TTL != 120 || !c.AdoptExisting || c.TokenRef != "env:CF" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := DNSConfigFrom(map[string]any{"provider": "cloudflare", "colour": "red"}); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := DNSConfigFrom(map[string]any{"ttl": "300"}); err == nil {
		t.Fatal("quoted ttl accepted")
	}
}

func TestCloudflarePreflightChecksTokenAndZoneWithoutWriting(t *testing.T) {
	f := newFakeCF(t)
	x := cfFixture(t, f, DNSConfig{Provider: "cloudflare"})
	if err := x.p.Preflight(context.Background(), "acme.example"); err != nil {
		t.Fatal(err)
	}
	f.zones = map[string]string{}
	if err := x.p.Preflight(context.Background(), "acme.example"); err == nil {
		t.Fatal("missing zone accepted")
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("preflight wrote: %v", f.calls)
		}
	}
}
